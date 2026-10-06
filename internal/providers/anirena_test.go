package providers

// The anirena provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anirena/main.lua, the PR143 Go→Lua
// migration — the twenty-sixth, the torrent family's second Lua slot
// after the PR142 rutor migration): these
// tests pin the script through the same contracts.Provider surface
// and the same verbatim live captures the compiled Go implementation
// was held to. The torrent plumbing (the PR66 .torrent preflight,
// episodes and streams) stays GO: the roster slot is wrapped by the
// luaTorrent adapter (lua_torrent.go) — the adapter's own contract
// lives in lua_torrent_test.go, the adapter×script integration pins
// in the second half of this file.
//
// Fixtures in this file are REAL API captures (the repo convention:
// offline fixtures must carry provenance):
//
//   - testdata/anirena_search_rss.xml — GET
//     https://www.anirena.com/rss?q=black+lagoon, captured with curl on
//     2026-09-23 (13 items: 11 Category "Anime …", 2 "Manga/Manhwa/Comic").
//   - testdata/anirena_search_empty.xml — GET
//     https://www.anirena.com/rss?q=kjwqvxhjwqlkjhzzz, same day (0 items).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
)

func TestAniRenaSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "anirena", anirenaServer(t, string(fixture(t, "anirena_search_rss.xml"))))
	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// The live capture carries 13 items: 11 Category "Anime …" and 2
	// "Manga/Manhwa/Comic" (the server IGNORES category= — live-verified
	// 2026-09-23), so exactly the 11 anime entries surface.
	if len(results) != 11 {
		t.Fatalf("results = %d, want 11 (anime categories only)", len(results))
	}

	batch := results[0]
	// The RSS title carries a leading "[Anime > Subtitle(s) and/or
	// Audio(s)] " category prefix — stripped for the release title.
	if batch.Title != "Black Lagoon - The Second Barrage - 01 ~ 12 [1080p][Multiple Subtitle]" {
		t.Errorf("title = %q", batch.Title)
	}
	if batch.SourceID != "anirena" {
		t.Errorf("source id = %q, want anirena", batch.SourceID)
	}
	// The <enclosure> is the direct .torrent download URL — the
	// byte-faithful torrent surface the Go engine consumes downstream.
	if batch.URL != "https://www.anirena.com/torrents/019d5dd9-39ce-7912-a1ba-bb9373c89dd3.torrent" {
		t.Errorf("url = %q, want the <enclosure> .torrent URL", batch.URL)
	}
	if batch.Meta[SearchMetaSize] != "7.7 GB" {
		t.Errorf("size meta = %v, want the description Size field", batch.Meta[SearchMetaSize])
	}
	if batch.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", batch.Meta[SearchMetaQuality])
	}
	// The RSS feed carries NO seed fields (live-verified): the seed
	// meta keys must be absent so filterSeedless keeps the item (the
	// fail-soft "no field, no filter" contract).
	if _, ok := batch.Meta[SearchMetaSeeders]; ok {
		t.Errorf("seeders meta = %v, want absent (the feed has no seed fields)", batch.Meta[SearchMetaSeeders])
	}
}

func TestAniRenaSearchRequestParams(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anirena_search_rss.xml"))
	})
	p := luaProvider(t, "anirena", srv.URL)

	if _, err := p.Search(context.Background(), "black lagoon"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rec.Path != "/rss" {
		t.Errorf("path = %q, want /rss", rec.Path)
	}
	// The query rides q= URL-encoded (the Go url.Values.Encode shape
	// the compiled provider sent: space → '+').
	if want := "q=black+lagoon"; rec.Query != want {
		t.Errorf("query = %q, want %q", rec.Query, want)
	}
	// Live-verified 2026-09-23: the server IGNORES the documented
	// category= parameter (the q= feed spans ALL categories), so the
	// request must not carry it — category filtering is client-side
	// (TestAniRenaSearchParsesRSS pins the effect).
	if strings.Contains(rec.Query, "category=") {
		t.Errorf("query = %q, want no category= (server ignores it — filtering is client-side)", rec.Query)
	}
}

func TestAniRenaSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := anirenaServerCounted(t, string(fixture(t, "anirena_search_rss.xml")), &hits)
	p := luaProvider(t, "anirena", srv)
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

// TestAniRenaSearchEmptyFeed pins the no-results shape: the live
// no-hit feed (real capture) parses to zero results and no error.
func TestAniRenaSearchEmptyFeed(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "anirena", anirenaServer(t, string(fixture(t, "anirena_search_empty.xml"))))
	results, err := p.Search(context.Background(), "kjwqvxhjwqlkjhzzz")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

// TestAniRenaSearchSkipsOutOfScopeItems: an item with a non-Anime (or
// missing) category, and an item without an <enclosure>, has nothing
// this provider could surface — dropped like every linkless item,
// never handed downstream as dead results.
func TestAniRenaSearchSkipsOutOfScopeItems(t *testing.T) {
	t.Parallel()

	feed := anirenaFeed(
		anirenaItemXML("[Manga/Manhwa/Comic] Manga PDF", "Size: 1.0 GB | Uploader: u | Category: Manga/Manhwa/Comic", anirenaEnclosure("m1")),
		anirenaItemXML("[Anime > RAW] Show - 01", "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW", anirenaEnclosure("a1")),
		anirenaItemXML("[Audio] Soundtrack", "Size: 1.0 GB | Uploader: u | Category: Audio", anirenaEnclosure("s1")),
		anirenaItemXML("[?] No category in description", "", anirenaEnclosure("n1")),
		anirenaItemXML("[Anime > RAW] No enclosure", "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW", ""),
	)
	p := luaProvider(t, "anirena", anirenaServer(t, feed))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (only the Anime-category item with an enclosure)", len(results))
	}
	if results[0].Title != "Show - 01" {
		t.Errorf("title = %q, want the category prefix stripped", results[0].Title)
	}
}

func TestAniRenaSearchMalformedEnvelopeTypedError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "anirena", anirenaServer(t, "this is not xml at all"))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed RSS must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anirena" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want an anirena search ProviderError", err)
	}
}

// TestAniRenaSearchHTTPErrorFailsLoud pins the HTTP failure wall. The
// compiled provider wrapped the netclient error in a ProviderError
// itself; the Lua transport surfaces the netclient sentinel classes
// through the typed anicli:<kind>: markers instead — 503 carries the
// plain StatusError class (only 403/404/geo/timeout keep the typed
// wall, the anilibria migration precedent), so the pin here is the
// loud, provider-tagged failure.
func TestAniRenaSearchHTTPErrorFailsLoud(t *testing.T) {
	t.Parallel()

	srv := anirenaServerStatus(t, http.StatusServiceUnavailable)
	p := luaProvider(t, "anirena", srv)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	if !strings.Contains(err.Error(), "anirena") {
		t.Errorf("error = %v, want the provider-tagged message", err)
	}
}

// TestAniRenaSearchCapsResults pins the bounded-surface rule (the
// animetosho AnimeToshoSearchLimit rationale): the feed has no usable
// server-side limit parameter (live-verified 2026-09-23), so the
// script caps the parsed items client-side BEFORE the category
// filter — the Go adapter's preflight fan-out downstream can never
// spend unbounded fetches.
func TestAniRenaSearchCapsResults(t *testing.T) {
	t.Parallel()

	items := make([]string, 0, 35)
	for i := range 35 {
		items = append(items, anirenaItemXML(
			fmt.Sprintf("[Anime > RAW] Show - %02d", i+1),
			"Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW",
			anirenaEnclosure(fmt.Sprintf("cap%02d", i))))
	}
	p := luaProvider(t, "anirena", anirenaServer(t, anirenaFeed(items...)))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 30 {
		t.Fatalf("results = %d, want the 30-item cap", len(results))
	}
}

// TestAniRenaTitleStripsCategoryPrefix pins the exact prefix rule
// through the search surface: a leading bracket group naming one of
// the site categories (optionally "Cat > Subcat") is stripped from the
// release title; release-group tags like [SubsPlease] and prefix-less
// titles stay verbatim. Every item carries an Anime description
// category so the scope filter never confounds the title pin.
func TestAniRenaTitleStripsCategoryPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct{ raw, want string }{
		{"[Anime > Subtitle(s) and/or Audio(s)] Show - 01", "Show - 01"},
		{"[Anime > RAW] Show", "Show"},
		{"[Anime] Show", "Show"},
		{"[Manga/Manhwa/Comic] Manga", "Manga"},
		{"[Audio] Music", "Music"},
		{"[Literature] Book", "Book"},
		{"[Live Action] Show", "Show"},
		{"[Pictures] Gallery", "Gallery"},
		{"[Software] App", "App"},
		{"[Hentai] Show", "Show"},
		{"[Other] Misc", "Misc"},
		// Not a category prefix: release-group tags survive.
		{"[SubsPlease] Show - 01 (1080p) [AAC].mkv", "[SubsPlease] Show - 01 (1080p) [AAC].mkv"},
		{"Show without any prefix", "Show without any prefix"},
	}
	items := make([]string, 0, len(cases))
	for i, tc := range cases {
		desc := "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW"
		items = append(items, anirenaItemXML(tc.raw, desc, anirenaEnclosure(fmt.Sprintf("ttl%02d", i))))
	}
	p := luaProvider(t, "anirena", anirenaServer(t, anirenaFeed(items...)))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != len(cases) {
		t.Fatalf("results = %d, want %d", len(results), len(cases))
	}
	for i, tc := range cases {
		if results[i].Title != tc.want {
			t.Errorf("results[%d].title = %q, want %q (raw %q)", i, results[i].Title, tc.want, tc.raw)
		}
	}
}

// TestAniRenaNotUnconfiguredByDefault pins the no-credentials convention:
// anirena has nothing to configure, so it must never appear in the
// disabled table with default settings.
func TestAniRenaNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "anirena" {
			t.Fatalf("anirena must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestAniRenaCapabilityDeclarations pins the script-declared capability
// surfaces (the Adapt composite): the JA/multilingual content
// language, the latin-only index routing (PR42) and the both-type
// catalog assessment — the exact declarations the compiled provider
// carried in Go code.
func TestAniRenaCapabilityDeclarations(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anirena")
	if p.Name() != "AniRena" {
		t.Errorf("name = %q, want AniRena", p.Name())
	}
	if p.BaseURL() != "https://www.anirena.com" {
		t.Errorf("base url = %q, want the site root", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the adapted provider lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP/multilingual releases)", got)
	}
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the adapted provider lost the NamePreference surface")
	}
	if np.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin", np.NamePreference())
	}
}

// --- the luaTorrent adapter × the bundled script (the compiled
// provider's torrent contract, ported) ---

// TestAniRenaTorrentSlotInRegistry pins the migration-safety parity:
// through the REAL registry the anirena slot keeps every surface it
// carried as a compiled provider — the torrent capability (the
// parity smoke's torrent leg routes on it), the engine-injection
// duck, the JA content language and the latin index routing.
func TestAniRenaTorrentSlotInRegistry(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	torrentFound := false
	for _, id := range reg.TorrentProviderIDs() {
		if id == "anirena" {
			torrentFound = true
		}
	}
	if !torrentFound {
		t.Fatal("anirena lost the torrent capability in the registry (the luaTorrent adapter wrap is missing?)")
	}
	p, ok := reg.Get("anirena")
	if !ok {
		t.Fatal("anirena is not registered")
	}
	// The wrapper layers peel the same way the registry's own wiring
	// does (the engine injection runs on the bare list BEFORE the
	// delegator wraps).
	if _, ok := bareProvider(p).(interface{ SetEngine(*torrent.Engine) }); !ok {
		t.Fatal("the anirena slot lost the engine-injection surface")
	}
	if got := reg.ContentLanguage("anirena"); got != "ja" {
		t.Errorf("content language = %q, want ja through the adapter", got)
	}
	if got := reg.NamePreference("anirena"); got != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin through the adapter", got)
	}
}

// newAniRenaLuaTorrent builds the migration shape: the bundled script
// (production base re-pointed at baseURL) wrapped in the torrent
// adapter with an offline engine wired.
func newAniRenaLuaTorrent(t *testing.T, baseURL string) *luaTorrent {
	t.Helper()
	inner := luaProvider(t, "anirena", baseURL)
	a := newLuaTorrent(inner)
	a.SetEngine(newOfflineTestEngine(t))
	return a
}

// TestAniRenaSearchPreflightDropsDeadHosts pins the PR66 owner ruling
// through the adapter: every surfaced result's .torrent bytes are
// pre-fetched (bounded, short per-URL budget) BEFORE the result
// surfaces; a dead host drops the result. The feed has no seed
// fields, so nothing is dropped by the seedless filter first — the
// preflight is the only gate here.
func TestAniRenaSearchPreflightDropsDeadHosts(t *testing.T) {
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

	feed := anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - 01", anirenaDesc("1.0 GiB"), live.URL+"/download/1.torrent"),
		anirenaItemXML("[Anime > RAW] Show - dead", anirenaDesc("1.0 GiB"), "http://"+dead.Addr().String()+"/download/2.torrent"),
		anirenaItemXML("[Anime > RAW] Show - 03", anirenaDesc("1.0 GiB"), live.URL+"/download/4.torrent"),
	)
	p := newAniRenaLuaTorrent(t, anirenaServer(t, feed))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host dropped)", len(results))
	}
	if results[0].URL != live.URL+"/download/1.torrent" || results[1].URL != live.URL+"/download/4.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in feed order", results[0].URL, results[1].URL)
	}
	// Each live result was fetched exactly once (the preflight).
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestAniRenaSearchPreflightFeedsIngestionNoRefetch: bytes that PASSED
// the preflight are handed to the engine right away, so the later
// GetEpisodes (the resolve leg) must NOT re-fetch the .torrent — the
// cache-reuse assertion of the ruling.
func TestAniRenaSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
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
	link := live.URL + "/download/1.torrent"
	p := newAniRenaLuaTorrent(t, anirenaServer(t, anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - 01", anirenaDesc("1.0 GiB"), link))))

	if _, err := p.Search(context.Background(), "show"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("preflight fetches = %d, want 1", fetches.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eps, err := p.GetEpisodes(ctx, link)
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

// TestAniRenaSearchPreflightNotMetainfoDropped: a host that answers
// HTTP 200 with NON-metainfo content (an HTML error page) is dead too
// — the content check drops it.
func TestAniRenaSearchPreflightNotMetainfoDropped(t *testing.T) {
	t.Parallel()

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>error page</body></html>"))
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

	feed := anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - wall", anirenaDesc("1.0 GiB"), html.URL+"/download/1.torrent"),
		anirenaItemXML("[Anime > RAW] Show - good", anirenaDesc("1.0 GiB"), live.URL+"/download/2.torrent"),
	)
	p := newAniRenaLuaTorrent(t, anirenaServer(t, feed))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/download/2.torrent" {
		t.Fatalf("results = %v, want only the live link", results)
	}
}

// TestAniRenaSearchPreflightSlowHostDropped: the per-URL budget is short
// (~10s in production); a host stalling past it is dropped while the
// rest of the surface still surfaces.
func TestAniRenaSearchPreflightSlowHostDropped(t *testing.T) {
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

	feed := anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - slow", anirenaDesc("1.0 GiB"), slow.URL+"/download/1.torrent"),
		anirenaItemXML("[Anime > RAW] Show - good", anirenaDesc("1.0 GiB"), live.URL+"/download/2.torrent"),
	)
	p := newAniRenaLuaTorrent(t, anirenaServer(t, feed))
	p.preflightBudget = 50 * time.Millisecond

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/download/2.torrent" {
		t.Fatalf("results = %v, want only the fast live link", results)
	}
}

// TestAniRenaSearchPreflightLogsTypedReason: drops are logged with the
// URL and the typed failure reason, never silent (the registry's
// logger seam routes through the adapter's SetLogger forward).
func TestAniRenaSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/download/1.torrent"
	feed := anirenaFeed(anirenaItemXML("[Anime > RAW] Show - dead", anirenaDesc("1.0 GiB"), deadURL))

	logBuf := &bytes.Buffer{}
	inner := luaProviderWithLogger(t, "anirena", anirenaServer(t, feed), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := newLuaTorrent(inner)
	p.SetLogger(slog.New(slog.NewTextHandler(logBuf, nil)))
	p.SetEngine(newOfflineTestEngine(t))

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

// TestAniRenaGetEpisodesDelegatesToEpisodesWait: the adapter's
// GetEpisodes rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func TestAniRenaGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	inner := luaProvider(t, "anirena", anirenaServer(t, anirenaFeed()))
	p := newLuaTorrent(inner)
	p.SetEngine(newOfflineTestEngine(t))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	eps, err := p.GetEpisodes(ctx, dead)
	if err == nil {
		t.Fatal("GetEpisodes on unreachable metadata must fail loud")
	}
	if len(eps) != 0 {
		t.Errorf("episodes = %v, want none", eps)
	}
}

// --- helpers ---

// anirenaServer serves body on every request (the RSS search endpoint).
func anirenaServer(t *testing.T, body string) string {
	t.Helper()
	return anirenaServerCounted(t, body, nil)
}

// anirenaServerCounted is anirenaServer with a request counter.
func anirenaServerCounted(t *testing.T, body string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// anirenaServerStatus serves the bare status code on every request.
func anirenaServerStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// anirenaItemXML renders one RSS item fragment: title, description
// CDATA (size/category source), optional enclosure URL.
func anirenaItemXML(title, desc, enclosure string) string {
	var b strings.Builder
	b.WriteString("<item><title>" + title + "</title>" +
		"<link>https://www.anirena.com/torrents/x</link>" +
		"<description><![CDATA[" + desc + "]]></description>")
	if enclosure != "" {
		b.WriteString(`<enclosure url="` + enclosure + `" type="application/x-bittorrent" length="0"/>`)
	}
	b.WriteString("</item>")
	return b.String()
}

// anirenaEnclosure is a fixture-shaped .torrent URL for an id (the
// production site host — fixture strings only, never fetched here).
func anirenaEnclosure(id string) string {
	return "https://www.anirena.com/torrents/019d5df0-0000-7000-8000-0000000000" + id + ".torrent"
}

// anirenaDesc renders a description CDATA body like the live feed's.
func anirenaDesc(size string) string {
	return "Size: " + size + " | Uploader: u | Category: Anime > RAW"
}

// anirenaFeed wraps item fragments into the AniRena RSS envelope.
func anirenaFeed(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"
  xmlns:atom="http://www.w3.org/2005/Atom"
  xmlns:media="http://search.yahoo.com/mrss/">
<channel>
  <title>AniRena — show</title>
  <link>https://www.anirena.com/</link>
  <description>AniRena torrent RSS feed</description>` +
		strings.Join(items, "") + `</channel></rss>`
}
