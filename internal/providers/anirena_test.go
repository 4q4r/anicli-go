package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Fixtures in this file are REAL API captures (the repo convention:
// offline fixtures must carry provenance):
//
//   - testdata/anirena_search_rss.xml — GET
//     https://www.anirena.com/rss?q=black+lagoon, captured with curl on
//     2026-09-23 (13 items: 11 Category "Anime …", 2 "Manga/Manhwa/Comic").
//   - testdata/anirena_search_empty.xml — GET
//     https://www.anirena.com/rss?q=kjwqvxhjwqlkjhzzz, same day (0 items).

func TestAniRenaSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := newAniRenaFixtureAt(t, anirenaServer(t, string(fixture(t, "anirena_search_rss.xml")), nil))
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
	// The <enclosure> is the direct .torrent download URL — the PR66
	// ingestion: preflighted bytes feed the engine, never re-fetched.
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

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotPath = r.URL.Path
		_, _ = w.Write(fixture(t, "anirena_search_rss.xml"))
	}))
	t.Cleanup(srv.Close)
	p := newAniRenaFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "black lagoon"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/rss" {
		t.Errorf("path = %q, want /rss", gotPath)
	}
	if gotQuery.Get("q") != "black lagoon" {
		t.Errorf("q = %q, want the raw query", gotQuery.Get("q"))
	}
	// Live-verified 2026-09-23: the server IGNORES the documented
	// category= parameter (the q= feed spans ALL categories), so the
	// request must not carry it — category filtering is client-side
	// (TestAniRenaSearchParsesRSS pins the effect).
	if gotQuery.Has("category") {
		t.Errorf("category = %q, want absent (server ignores it — filtering is client-side)", gotQuery.Get("category"))
	}
}

func TestAniRenaSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newAniRenaFixtureAt(t, anirenaServer(t, string(fixture(t, "anirena_search_rss.xml")), &hits))
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

	p := newAniRenaFixtureAt(t, anirenaServer(t, string(fixture(t, "anirena_search_empty.xml")), nil))
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
// this provider could surface — dropped like nyaa's linkless items,
// never handed downstream as dead results.
func TestAniRenaSearchSkipsOutOfScopeItems(t *testing.T) {
	t.Parallel()

	feed := anirenaFeed(
		anirenaItemXML("[Manga/Manhwa/Comic] Manga PDF", "Size: 1.0 GB | Uploader: u | Category: Manga/Manhwa/Comic", anirenaEnclosure(t, "m1")),
		anirenaItemXML("[Anime > RAW] Show - 01", "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW", anirenaEnclosure(t, "a1")),
		anirenaItemXML("[Audio] Soundtrack", "Size: 1.0 GB | Uploader: u | Category: Audio", anirenaEnclosure(t, "s1")),
		anirenaItemXML("[?] No category in description", "", anirenaEnclosure(t, "n1")),
		anirenaItemXML("[Anime > RAW] No enclosure", "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW", ""),
	)
	p := newAniRenaFixtureAt(t, anirenaServer(t, feed, nil))
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

func TestAniRenaSearchMalformedXMLTypedError(t *testing.T) {
	t.Parallel()

	p := newAniRenaFixtureAt(t, anirenaServer(t, "this is not xml at all", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed RSS must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anirena" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want an anirena search ProviderError", err)
	}
}

func TestAniRenaSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newAniRenaFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anirena" {
		t.Errorf("error = %v, want an anirena-tagged ProviderError", err)
	}
}

// TestAniRenaSearchCapsResults pins the bounded-surface rule (the
// animetosho AnimeToshoSearchLimit rationale): the feed has no usable
// server-side limit parameter (live-verified 2026-09-23), so Search
// caps the parsed items client-side BEFORE the preflight fan-out —
// one search can never spend unbounded fetches.
func TestAniRenaSearchCapsResults(t *testing.T) {
	t.Parallel()

	items := make([]string, 0, AniRenaSearchLimit+5)
	for i := range AniRenaSearchLimit + 5 {
		items = append(items, anirenaItemXML(
			fmt.Sprintf("[Anime > RAW] Show - %02d", i+1),
			"Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW",
			anirenaEnclosure(t, fmt.Sprintf("cap%02d", i))))
	}
	p := newAniRenaFixtureAt(t, anirenaServer(t, anirenaFeed(items...), nil))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != AniRenaSearchLimit {
		t.Fatalf("results = %d, want the %d-item cap", len(results), AniRenaSearchLimit)
	}
}

// TestAniRenaTitleStripsCategoryPrefix pins the exact prefix rule: a
// leading bracket group naming one of the site categories (optionally
// "Cat > Subcat") is stripped; release-group tags like [SubsPlease]
// and prefix-less titles stay verbatim.
func TestAniRenaTitleStripsCategoryPrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
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
	} {
		if got := anirenaTitle(tc.in); got != tc.want {
			t.Errorf("anirenaTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAniRenaCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newAniRenaFixtureAt(t, anirenaServer(t, string(fixture(t, "anirena_search_rss.xml")), nil))
	if !p.IsTorrent() {
		t.Error("anirena must carry the torrent capability")
	}
	if p.ID() != "anirena" || p.Name() != "AniRena" {
		t.Errorf("id/name = %q/%q", p.ID(), p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP/multilingual releases)", got)
	}
	// PR42: the index matches romaji/english release names only.
	if p.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin", p.NamePreference())
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the site root, not empty")
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

// TestAniRenaGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func TestAniRenaGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	p := newAniRenaWithEngine(t)
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

// TestAniRenaSearchPreflightDropsDeadHosts pins the PR66 owner ruling:
// every surfaced result's .torrent bytes are pre-fetched (bounded,
// short per-URL budget) BEFORE the result surfaces; a dead host drops
// the result. The feed has no seed fields, so nothing is dropped by
// the seedless filter first — the preflight is the only gate here.
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
	p := newAniRena(anirenaServer(t, feed, nil), testClient(t, "anirena"), newOfflineTestEngine(t))

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
	p := newAniRena(anirenaServer(t, anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - 01", anirenaDesc("1.0 GiB"), link)), nil),
		testClient(t, "anirena"), newOfflineTestEngine(t))

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
	p := newAniRena(anirenaServer(t, feed, nil), testClient(t, "anirena"), newOfflineTestEngine(t))

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
	p := newAniRena(anirenaServer(t, feed, nil), testClient(t, "anirena"), newOfflineTestEngine(t))
	p.preflightTimeout = 50 * time.Millisecond

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/download/2.torrent" {
		t.Fatalf("results = %v, want only the fast live link", results)
	}
}

// TestAniRenaSearchPreflightLogsTypedReason: drops are logged with the
// URL and the typed failure reason, never silent.
func TestAniRenaSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/download/1.torrent"
	feed := anirenaFeed(anirenaItemXML("[Anime > RAW] Show - dead", anirenaDesc("1.0 GiB"), deadURL))
	p := newAniRena(anirenaServer(t, feed, nil), testClient(t, "anirena"), newOfflineTestEngine(t))

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

// TestAniRenaSearchNoEngineSkipsPreflight pins the nil-engine rule:
// without the [torrent] engine there is nothing to preflight or feed,
// so Search keeps the legacy behavior (no prefetch requests — this is
// also what keeps hand-built unit tests network-free).
func TestAniRenaSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	feed := anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - 01", anirenaDesc("1.0 GiB"), AniRenaBase+"/torrents/019d5dd9-39ce-7912-a1ba-bb9373c89dd3.torrent"),
		anirenaItemXML("[Anime > RAW] Show - 02", anirenaDesc("1.0 GiB"), AniRenaBase+"/torrents/019d5dd9-39ce-7912-a1ba-bb9373c89dd4.torrent"),
	)
	p := newAniRenaFixtureAt(t, anirenaServer(t, feed, &hits))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (nil engine: no preflight, nothing dropped)", len(results))
	}
	// Exactly ONE request happened: the RSS search itself. No
	// preflight attempted the fixture's download URLs.
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only)", hits)
	}
}

// --- helpers ---

// anirenaServer serves body on every request (the RSS search endpoint);
// hits, when non-nil, counts requests.
func anirenaServer(t *testing.T, body string, hits *int) string {
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

func newAniRenaFixtureAt(t *testing.T, baseURL string) *AniRena {
	t.Helper()
	return newAniRena(baseURL, testClient(t, "anirena"), nil)
}

func newAniRenaWithEngine(t *testing.T) *AniRena {
	t.Helper()
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	return newAniRena(AniRenaBase, testClient(t, "anirena"), eng)
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

// anirenaEnclosure is a fixture-shaped .torrent URL for an id.
func anirenaEnclosure(t *testing.T, id string) string {
	t.Helper()
	return AniRenaBase + "/torrents/019d5df0-0000-7000-8000-0000000000" + id + ".torrent"
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
