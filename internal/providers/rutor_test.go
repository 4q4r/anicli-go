package providers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// Fixture provenance (PR87): rutor_search_dandadan.html and
// rutor_search_empty.html carry verbatim live captures of the site's
// own search page (GET https://rutor.info/search/0/0/100/0/dandadan/
// and .../zzzqqqxxxuuu несуществует/ — 2026-09-23, anonymous 200,
// UTF-8), untrimmed. The live-verified row shape baked into the
// parser: data rows carry a magnet link (the Jackett row selector),
// the .torrent rides a.downgif as a PROTOCOL-RELATIVE //d.rutor.info/
// download URL, seed/leech live in span.green/span.red, and the size
// sits in its own td ("38.92&nbsp;GB") — td counts vary between rows
// (the comments cell is optional upstream), so the size is fished by
// content, never by position.

// rutorRow is the real element order of one live search-result row,
// parameterized for constructed-row tests (the tokyotosho fixture
// convention): optional download and magnet anchors, the /torrent/
// title link, the (optional upstream!) comments cell, size and the
// seed/leech spans.
func rutorRow(dl, magnet, title, size, seed, leech string) string {
	var b strings.Builder
	b.WriteString(`<tr class="gai"><td>22&nbsp;Янв&nbsp;26</td><td >`)
	if dl != "" {
		b.WriteString(`<a class="downgif" href="` + dl + `"><img src="//cdnbunny.org/i/d.gif" alt="D" /></a>`)
	}
	if magnet != "" {
		b.WriteString(`<a href="` + magnet + `"><img src="//cdnbunny.org/i/m.png" alt="M" /></a>`)
	}
	b.WriteString(`<a href="/torrent/1044920/slug">` + title + `</a></td>`)
	if size != "" || seed != "" {
		// The comments cell is optional upstream (Jackett's «some
		// results don't have comments which throws off td count») —
		// omit it in the sizeless variant to keep that hazard pinned.
		if size != "" {
			b.WriteString(`<td align="right">17<img src="//cdnbunny.org/i/com.gif" alt="C" /></td>`)
		}
		b.WriteString(`<td align="right">` + size + `</td>`)
	}
	if seed != "" || leech != "" {
		b.WriteString(`<td align="center">`)
		if seed != "" {
			b.WriteString(`<span class="green"><img src="//cdnbunny.org/t/arrowup.gif" alt="S" />&nbsp;` + seed + `</span>&nbsp;`)
		}
		if leech != "" {
			b.WriteString(`<img src="//cdnbunny.org/t/arrowdown.gif" alt="L" /><span class="red">&nbsp;` + leech + `</span>`)
		}
		b.WriteString(`</td>`)
	}
	b.WriteString(`</tr>`)
	return b.String()
}

// rutorPage wraps rows in the live page's results-table skeleton.
func rutorPage(rows ...string) string {
	return `<html><head><meta http-equiv="content-type" content="text/html; charset=utf-8" /></head><body>` +
		`<div id="index"><b>Страницы:  1</b> Результатов поиска ` +
		`N (max. 2000)<table width="100%">` +
		`<tr class="backgr"><td width="10px">Добавлен</td><td colspan="2">Название</td>` +
		`<td width="1px">Размер</td><td width="1px">Пиры</td></tr>` +
		strings.Join(rows, "") + `</table></div></body></html>`
}

func newRutorFixtureAt(t *testing.T, baseURL string) *RuTor {
	t.Helper()
	return newRutor(baseURL, testClient(t, "rutor"), nil)
}

func rutorServer(t *testing.T, body string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	rutorMagnet1 = "magnet:?xt=urn:btih:7496e402a9bde1d042505f297979b11830e4b19b&dn=rutor.info&tr=udp://opentor.net:6969&tr=http://retracker.local/announce"
	rutorMagnet2 = "magnet:?xt=urn:btih:ac90e5631f8d78d8932c167743386762369eb545&dn=rutor.info&tr=udp://opentor.net:6969&tr=http://retracker.local/announce"
)

func TestRutorSearchParsesHTML(t *testing.T) {
	t.Parallel()

	srvURL := rutorServer(t, string(fixture(t, "rutor_search_dandadan.html")), nil)
	p := newRutorFixtureAt(t, srvURL)
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (the verbatim live capture)", len(results))
	}

	// The .downgif href is PROTOCOL-RELATIVE on the wire
	// (//d.rutor.info/download/{id}): the host rides the href, the
	// scheme is inherited from the serving page — http on the
	// httptest fixture, https on the real site.
	scheme := strings.SplitN(srvURL, ":", 2)[0]

	first := results[0]
	if first.Title != "Дандадан / Dandadan / Dan Da Dan [S01-02] (2024-2025) BDRip-HEVC 1080p | D, P, L | Студийная Банда, Flarrow Films, AniBaza, Amber, TVShows, AniLibria, Dream Cast" {
		t.Errorf("title = %q", first.Title)
	}
	if first.SourceID != "rutor" {
		t.Errorf("source id = %q, want rutor", first.SourceID)
	}
	if first.URL != scheme+"://d.rutor.info/download/1040330" {
		t.Errorf("url = %q, want the absolute .torrent download URL", first.URL)
	}
	if got := first.Meta[SearchMetaSize]; got != "38.92 GB" {
		t.Errorf("size meta = %v, want the page's own 38.92&nbsp;GB normalized", got)
	}
	if got := first.Meta[SearchMetaSeeders]; got != "8" {
		t.Errorf("seeders meta = %v, want 8 (span.green)", got)
	}
	if got := first.Meta[SearchMetaLeechers]; got != "3" {
		t.Errorf("leechers meta = %v, want 3 (span.red)", got)
	}
	if got := first.Meta[SearchMetaQuality]; got != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", got)
	}

	second := results[1]
	if second.Title != "Дандадан / Dandadan / Dan Da Dan [S02] (2025) WEB-DL 1080p | D, P, L | Студийная Банда, AniBaza, TVShows, Dream Cast" {
		t.Errorf("second title = %q", second.Title)
	}
	if second.URL != scheme+"://d.rutor.info/download/1044920" {
		t.Errorf("second url = %q, want the absolute .torrent URL", second.URL)
	}
	if got := second.Meta[SearchMetaSeeders]; got != "12" {
		t.Errorf("second seeders = %v, want 12", got)
	}

	last := results[3]
	if last.Title != "Дандадан / Dandadan [S01] (2024) WEBRip 1080p | L2 | AEROChannelEkat & Риша" {
		t.Errorf("last title = %q (the &amp; entity must decode)", last.Title)
	}
	if last.URL != scheme+"://d.rutor.info/download/1016633" {
		t.Errorf("last url = %q", last.URL)
	}
	if got := last.Meta[SearchMetaSize]; got != "16.47 GB" {
		t.Errorf("last size = %v, want 16.47 GB", got)
	}
}

func TestRutorSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(rutorPage(rutorRow("", rutorMagnet1, "Show 1080p", "1.00&nbsp;GB", "5", "1"))))
	}))
	t.Cleanup(srv.Close)
	p := newRutorFixtureAt(t, srv.URL)

	// The live-verified query form: a Cyrillic multi-word query rides
	// the search PATH percent-encoded (spaces %20, UTF-8 bytes), the
	// Jackett recipe path with the default sort (0 = created desc).
	if _, err := p.Search(context.Background(), "черная лагуна"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// r.URL.Path is the server-side DECODED path; the wire form the
	// provider sent lives in EscapedPath.
	want := "/search/0/0/100/0/" + url.PathEscape("черная лагуна") + "/"
	if gotPath != want {
		t.Errorf("path = %q, want %q (the live-verified search-path form)", gotPath, want)
	}
}

func TestRutorSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newRutorFixtureAt(t, rutorServer(t, string(fixture(t, "rutor_search_dandadan.html")), &hits))
	for _, query := range []string{"", "   "} {
		_, err := p.Search(context.Background(), query)
		if err == nil {
			t.Fatalf("query %q: empty search must fail loud", query)
		}
		if !errors.Is(err, contracts.ErrInvalidInput) {
			t.Errorf("query %q: error = %v, want ErrInvalidInput", query, err)
		}
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (no network on the guard)", hits)
	}
}

// TestRutorSearchZeroResultsIsClean pins the live zero-result shape
// (rutor_search_empty.html, verbatim capture 2026-09-23): HTTP 200,
// the «Результатов поиска 0» marker, no data rows — the row selector
// naturally matches nothing and the search settles as empty results,
// never an error (unlike TT's footer shape, nothing here needs
// special-casing: the HTML row selector is the zero-detection).
func TestRutorSearchZeroResultsIsClean(t *testing.T) {
	t.Parallel()

	p := newRutorFixtureAt(t, rutorServer(t, string(fixture(t, "rutor_search_empty.html")), nil))
	results, err := p.Search(context.Background(), "несуществует")
	if err != nil {
		t.Fatalf("zero results must not error, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none", results)
	}
}

// TestRutorSearchToleratesGarbageHTML pins the HTML tolerance: goquery
// on a non-HTML body matches no rows — that is an empty surface, not
// a malfunction (HTML parsers never fail loud on bytes; the typed
// error path is the transport's).
func TestRutorSearchToleratesGarbageHTML(t *testing.T) {
	t.Parallel()

	p := newRutorFixtureAt(t, rutorServer(t, "this is not html at all", nil))
	results, err := p.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("garbage HTML must not error, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none", results)
	}
}

func TestRutorSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newRutorFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "rutor" {
		t.Errorf("error = %v, want a rutor-tagged ProviderError", err)
	}
}

// TestRutorSearchSeedlessDropped pins the PR44 rule on the live seed
// fields: a row reporting span.green = 0 is a dead result and is
// dropped BEFORE the preflight spends a fetch on it; a row with no
// seed span at all is kept (fail-soft: no field, no filter).
func TestRutorSearchSeedlessDropped(t *testing.T) {
	t.Parallel()

	page := rutorPage(
		rutorRow("//dl.example/a.torrent", rutorMagnet1, "Seeded 1080p", "1.00&nbsp;GB", "5", "1"),
		rutorRow("//dl.example/b.torrent", rutorMagnet2, "Seedless 1080p", "2.00&nbsp;GB", "0", "3"),
		rutorRow("//dl.example/c.torrent", rutorMagnet1, "No seed field 1080p", "3.00&nbsp;GB", "", "1"),
	)
	p := newRutorFixtureAt(t, rutorServer(t, page, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the 0-seed row dropped, the no-field row kept)", len(results))
	}
	if results[0].Title != "Seeded 1080p" || results[1].Title != "No seed field 1080p" {
		t.Errorf("titles = [%q, %q], want seeded + no-field survivors", results[0].Title, results[1].Title)
	}
}

// TestRutorSearchSkipsLinklessRows: a row with neither a .downgif
// nor a usable magnet has nothing the engine could ingest — dropped,
// never a dead result (the TorrentBase rule). A broken magnet (short/absent
// infohash) counts as unusable.
func TestRutorSearchSkipsLinklessRows(t *testing.T) {
	t.Parallel()

	page := rutorPage(
		rutorRow("//dl.example/a.torrent", rutorMagnet1, "Usable 1080p", "1.00&nbsp;GB", "5", "1"),
		rutorRow("", "", "No links 1080p", "2.00&nbsp;GB", "5", "1"),
		rutorRow("", "magnet:?xt=urn:btih:zzzz", "Broken magnet 1080p", "3.00&nbsp;GB", "5", "1"),
	)
	p := newRutorFixtureAt(t, rutorServer(t, page, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Usable 1080p" {
		t.Fatalf("results = %v, want only the usable row", results)
	}
}

// TestRutorSearchMagnetFallback pins the link choice: when the row
// has no .downgif anchor, the magnet itself becomes the result link
// VERBATIM (it carries tr= announces; the engine owns magnets and the
// preflight is an HTTP probe that keeps magnet-shaped links
// unprobed).
func TestRutorSearchMagnetFallback(t *testing.T) {
	t.Parallel()

	page := rutorPage(rutorRow("", rutorMagnet2, "Magnet only 1080p", "4.00&nbsp;GB", "7", "2"))
	p := newRutorFixtureAt(t, rutorServer(t, page, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].URL != rutorMagnet2 {
		t.Errorf("url = %q, want the magnet verbatim", results[0].URL)
	}
}

func TestRutorGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	p := newRutor(RutorBase, testClient(t, "rutor"), eng)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	eps, err := p.GetEpisodes(ctx, dead)
	if err == nil {
		t.Fatal("GetEpisodes on unreachable metadata must fail loud")
	}
	if len(eps) != 0 {
		t.Errorf("episodes = %v, want none", eps)
	}
	if !strings.Contains(err.Error(), "торренты") {
		t.Errorf("error = %v, want the torrent-base wait failure", err)
	}
}

func TestRutorCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newRutorFixtureAt(t, rutorServer(t, string(fixture(t, "rutor_search_dandadan.html")), nil))
	if !p.IsTorrent() {
		t.Error("rutor must carry the torrent capability")
	}
	if p.ID() != "rutor" {
		t.Errorf("id = %q", p.ID())
	}
	if p.Name() != "RuTor" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (RU dubs, original audio)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ru" {
		t.Errorf("content language = %q, want ru (RU voiceovers)", got)
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the site root, not empty")
	}
}

// TestRutorNotUnconfiguredByDefault pins the no-credentials
// convention: nothing to configure — never in the disabled table with
// default settings (the [torrent] gating is the disabled-table rule).
func TestRutorNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "rutor" {
			t.Fatalf("rutor must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestRutorDisabledWhenTorrentOff pins the disabled-table rule shared
// across the family: without the [torrent] subsystem the provider cannot play
// anything, so it is not registered at all.
func TestRutorDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false

	var found *DisabledProvider
	for _, d := range UnconfiguredProviders(cfg) {
		if d.ID == "rutor" {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatal("rutor must be unconfigured when [torrent] is disabled")
	}
	if !strings.Contains(found.Reason, "[torrent]") {
		t.Errorf("reason = %q, want the torrent-subsystem wording", found.Reason)
	}
}

// --- PR66 preflight pins (the shared TorrentBase mechanism, exercised
// for rutor's own route): dead hosts dropped BEFORE surfacing,
// survivors' bytes feed the engine, no double fetch.

// TestRutorSearchPreflightDropsDeadHosts: every surfaced result's
// .torrent bytes are pre-fetched (bounded, short per-URL timeout)
// BEFORE the result surfaces; a dead host drops the result. Survivors
// keep page order and metadata.
func TestRutorSearchPreflightDropsDeadHosts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
	var torrentBytes bytes.Buffer
	if err := mi.Write(&torrentBytes); err != nil {
		t.Fatalf("serialize metainfo: %v", err)
	}

	var fetches atomic.Int64
	live := torrentFixtureServer(t, torrentBytes.Bytes(), &fetches)
	dead := newDeadListener(t)

	page := rutorPage(
		rutorRow(live.URL+"/good1.torrent", rutorMagnet1, "Good 1 1080p", "1.00&nbsp;GB", "5", "1"),
		rutorRow("http://"+dead.Addr().String()+"/dead.torrent", rutorMagnet2, "Dead 1080p", "2.00&nbsp;GB", "5", "1"),
		rutorRow(live.URL+"/good2.torrent", rutorMagnet1, "Good 2 1080p", "3.00&nbsp;GB", "5", "1"),
	)
	p := newRutor(rutorServer(t, page, nil), testClient(t, "rutor"), newOfflineTestEngine(t))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host dropped)", len(results))
	}
	if results[0].URL != live.URL+"/good1.torrent" || results[1].URL != live.URL+"/good2.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in page order", results[0].URL, results[1].URL)
	}
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestRutorSearchPreflightFeedsIngestionNoRefetch: bytes that PASSED
// the preflight are handed to the engine right away, so the later
// GetEpisodes (the resolve leg) must NOT re-fetch the .torrent — the
// cache-reuse assertion of the PR66 ruling.
func TestRutorSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
	if testing.Short() {
		t.Skip("engine-based ingest in short mode")
	}
	t.Parallel()

	dir := t.TempDir()
	_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
	var torrentBytes bytes.Buffer
	if err := mi.Write(&torrentBytes); err != nil {
		t.Fatalf("serialize metainfo: %v", err)
	}

	var fetches atomic.Int64
	live := torrentFixtureServer(t, torrentBytes.Bytes(), &fetches)
	page := rutorPage(rutorRow(live.URL+"/good.torrent", rutorMagnet1, "Good 1080p", "1.00&nbsp;GB", "5", "1"))
	p := newRutor(rutorServer(t, page, nil), testClient(t, "rutor"), newOfflineTestEngine(t))

	if _, err := p.Search(context.Background(), "show"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("preflight fetches = %d, want 1", fetches.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eps, err := p.GetEpisodes(ctx, live.URL+"/good.torrent")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) == 0 {
		t.Fatal("episodes = 0, want the release file (metadata was already ingested)")
	}
	if fetches.Load() != 1 {
		t.Errorf("fetches after GetEpisodes = %d, want 1 (ingestion must not double-fetch)", fetches.Load())
	}
}

// TestRutorSearchPreflightNotMetainfoDropped: a host that answers
// HTTP 200 with NON-metainfo content (login wall, parked page) is
// dead too — the content check drops it.
func TestRutorSearchPreflightNotMetainfoDropped(t *testing.T) {
	t.Parallel()

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>login wall, no torrent here</body></html>"))
	}))
	t.Cleanup(html.Close)
	live := torrentFixtureServer(t, func() []byte {
		dir := t.TempDir()
		_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			t.Fatalf("serialize metainfo: %v", err)
		}
		return buf.Bytes()
	}(), nil)

	page := rutorPage(
		rutorRow(html.URL+"/wall.torrent", rutorMagnet1, "Wall 1080p", "1.00&nbsp;GB", "5", "1"),
		rutorRow(live.URL+"/good.torrent", rutorMagnet2, "Good 1080p", "2.00&nbsp;GB", "5", "1"),
	)
	p := newRutor(rutorServer(t, page, nil), testClient(t, "rutor"), newOfflineTestEngine(t))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/good.torrent" {
		t.Fatalf("results = %v, want only the live link", results)
	}
}

// TestRutorSearchPreflightSlowHostDropped: the per-URL budget is
// short (~10s in production); a host stalling past it is dropped
// while the rest of the surface still surfaces.
func TestRutorSearchPreflightSlowHostDropped(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	live := torrentFixtureServer(t, func() []byte {
		dir := t.TempDir()
		_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			t.Fatalf("serialize metainfo: %v", err)
		}
		return buf.Bytes()
	}(), nil)

	page := rutorPage(
		rutorRow(slow.URL+"/slow.torrent", rutorMagnet1, "Slow 1080p", "1.00&nbsp;GB", "5", "1"),
		rutorRow(live.URL+"/good.torrent", rutorMagnet2, "Good 1080p", "2.00&nbsp;GB", "5", "1"),
	)
	p := newRutor(rutorServer(t, page, nil), testClient(t, "rutor"), newOfflineTestEngine(t))
	p.preflightTimeout = 50 * time.Millisecond

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/good.torrent" {
		t.Fatalf("results = %v, want only the fast live link", results)
	}
}

// TestRutorSearchPreflightLogsTypedReason: drops are logged with the
// URL and the typed failure reason, never silent.
func TestRutorSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/dead.torrent"
	page := rutorPage(rutorRow(deadURL, rutorMagnet1, "Dead 1080p", "1.00&nbsp;GB", "5", "1"))
	p := newRutor(rutorServer(t, page, nil), testClient(t, "rutor"), newOfflineTestEngine(t))

	var logBuf bytes.Buffer
	p.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))

	if _, err := p.Search(context.Background(), "show"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, deadURL) {
		t.Errorf("log = %q, want the dropped URL", logged)
	}
	if !strings.Contains(logged, "preflight") {
		t.Errorf("log = %q, want the typed preflight reason", logged)
	}
}

// TestRutorSearchNoEngineSkipsPreflight pins the nil-engine rule:
// without the [torrent] engine there is nothing to preflight or feed,
// so Search keeps the legacy behavior (no prefetch requests — this is
// also what keeps hand-built unit tests network-free).
func TestRutorSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newRutorFixtureAt(t, rutorServer(t, string(fixture(t, "rutor_search_dandadan.html")), &hits))
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (nil engine: no preflight, nothing dropped)", len(results))
	}
	// Exactly ONE request happened: the search page itself. No
	// preflight attempted the fixture's real d.rutor.info URLs.
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only)", hits)
	}
}
