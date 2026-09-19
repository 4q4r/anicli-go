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

func TestNyaaSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), nil))
	results, err := p.Search(context.Background(), "test show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}

	batch := results[0]
	if batch.Title != "[SubsPlease] Test Show (01-12) (1080p) [AAC].mkv" {
		t.Errorf("title = %q", batch.Title)
	}
	if batch.SourceID != "nyaa" {
		t.Errorf("source id = %q, want nyaa", batch.SourceID)
	}
	// The RSS <link> is the direct .torrent download URL — PR66
	// ingestion rides the preflighted bytes, never the tracker-less
	// synthesized magnet (the DHT-only metadata path that timed out
	// the PR52 smoke).
	if batch.URL != "https://nyaa.si/download/1111111.torrent" {
		t.Errorf("url = %q, want the RSS <link> .torrent URL", batch.URL)
	}
	if batch.Meta[SearchMetaSize] != "7.4 GiB" {
		t.Errorf("size meta = %v", batch.Meta[SearchMetaSize])
	}
	if batch.Meta[SearchMetaSeeders] != "421" || batch.Meta[SearchMetaLeechers] != "33" {
		t.Errorf("seeders/leechers meta = %v/%v", batch.Meta[SearchMetaSeeders], batch.Meta[SearchMetaLeechers])
	}
	if batch.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", batch.Meta[SearchMetaQuality])
	}

	// No <link> on the wire → the infoHash-built magnet is the
	// fallback link (kept unprobed — the engine owns magnets).
	fallback := results[1]
	if fallback.URL != "https://nyaa.si/download/2222222.torrent" {
		t.Errorf("fallback url = %q, want the RSS <link>", fallback.URL)
	}
	if fallback.Meta[SearchMetaQuality] != "2160p" {
		t.Errorf("fallback quality meta = %v, want the PR35 badge 2160p", fallback.Meta[SearchMetaQuality])
	}
}

func TestNyaaSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotPath = r.URL.Path
		_, _ = w.Write(fixture(t, "nyaa_search_rss.xml"))
	}))
	t.Cleanup(srv.Close)
	p := newNyaaFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "fate stay night"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want the site root", gotPath)
	}
	if gotQuery.Get("page") != "rss" {
		t.Errorf("page = %q, want rss", gotQuery.Get("page"))
	}
	if gotQuery.Get("q") != "fate stay night" {
		t.Errorf("q = %q, want the raw query", gotQuery.Get("q"))
	}
	// Controller-verified defaults: English-translated anime, no
	// filter, seeded-first ordering.
	if gotQuery.Get("c") != "1_2" {
		t.Errorf("c = %q, want 1_2 (anime english-translated)", gotQuery.Get("c"))
	}
	if gotQuery.Get("f") != "0" {
		t.Errorf("f = %q, want 0 (no filter)", gotQuery.Get("f"))
	}
	if gotQuery.Get("s") != "seeders" || gotQuery.Get("o") != "desc" {
		t.Errorf("s/o = %q/%q, want seeders/desc", gotQuery.Get("s"), gotQuery.Get("o"))
	}
}

func TestNyaaSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), &hits))
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

// TestNyaaSearchSkipsLinklessItems: an item with neither a usable
// infoHash nor a <link> has no torrent link at all — it is dropped
// like an empty title, never handed downstream as a dead result.
func TestNyaaSearchSkipsLinklessItems(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:nyaa="https://nyaa.si/xmlns/nyaa">
  <channel>
    <item>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <link>https://nyaa.si/download/3333333.torrent</link>
      <nyaa:infoHash>abcdef0123456789abcdef0123456789abcdef01</nyaa:infoHash>
      <nyaa:size>1.0 GiB</nyaa:size>
    </item>
    <item>
      <title>[Broken] No hash no link</title>
      <nyaa:size>1.0 GiB</nyaa:size>
    </item>
  </channel>
</rss>`

	p := newNyaaFixtureAt(t, nyaaServer(t, body, nil))
	results, err := p.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the linkless item must be skipped)", len(results))
	}
	if results[0].URL == "" {
		t.Error("the kept result must carry its magnet link")
	}
}

func TestNyaaSearchMalformedRSSTypedError(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, "this is not xml at all", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed RSS must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "nyaa" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a nyaa search ProviderError", err)
	}
}

func TestNyaaSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newNyaaFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "nyaa" {
		t.Errorf("error = %v, want a nyaa-tagged ProviderError", err)
	}
}

// TestNyaaGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func TestNyaaGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	p := newNyaaWithEngine(t)
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

func TestNyaaCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), nil))
	if !p.IsTorrent() {
		t.Error("nyaa must carry the torrent capability")
	}
	if p.ID() != "nyaa" || p.Name() != "Nyaa" {
		t.Errorf("id/name = %q/%q", p.ID(), p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP audio with subs)", got)
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the site root, not empty")
	}
}

// TestNyaaNotUnconfiguredByDefault pins the no-credentials convention:
// nyaa has nothing to configure, so it must never appear in the
// disabled table with default settings.
func TestNyaaNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "nyaa" {
			t.Fatalf("nyaa must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// --- helpers ---

// nyaaServer serves body on every request (the RSS search endpoint);
// hits, when non-nil, counts requests.
func nyaaServer(t *testing.T, body string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newNyaaFixtureAt(t *testing.T, baseURL string) *Nyaa {
	t.Helper()
	return newNyaa(baseURL, testClient(t, "nyaa"), nil)
}

func newNyaaWithEngine(t *testing.T) *Nyaa {
	t.Helper()
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	return newNyaa(NyaaBase, testClient(t, "nyaa"), eng)
}

// --- PR66: .torrent-bytes ingestion (the tokyotosho PR53 pattern).
// The search result link is the RSS <link> .torrent download URL; a
// search-time preflight pre-fetches every result's bytes (bounded,
// short per-URL budget) and drops the dead ones BEFORE they surface
// (owner standing rule), handing the survivors' bytes to the engine —
// the tracker-less synthesized magnet (DHT-only metadata, the PR52
// smoke killer) is only the no-<link> fallback now.

// nyaaItemXML renders one RSS item fragment: title, optional <link>,
// optional nyaa:infoHash, the seeder count (the PR44 filter keys on
// it) and a fixed size.
func nyaaItemXML(title, link, infoHash, seeders string) string {
	var b strings.Builder
	b.WriteString("<item><title>" + title + "</title>")
	if link != "" {
		b.WriteString("<link>" + link + "</link>")
	}
	if infoHash != "" {
		b.WriteString("<nyaa:infoHash>" + infoHash + "</nyaa:infoHash>")
	}
	b.WriteString("<nyaa:seeders>" + seeders + "</nyaa:seeders>" +
		"<nyaa:size>1.0 GiB</nyaa:size></item>")
	return b.String()
}

// nyaaFeed wraps item fragments into the nyaa RSS envelope.
func nyaaFeed(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:nyaa="https://nyaa.si/xmlns/nyaa"><channel><title>Nyaa - search results</title>` +
		strings.Join(items, "") + `</channel></rss>`
}

// TestNyaaSearchMagnetFallbackWithoutLink pins the reversed link
// preference: an item with no <link> falls back to the infoHash-built
// magnet (kept unprobed by the preflight — the engine owns magnets).
func TestNyaaSearchMagnetFallbackWithoutLink(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:nyaa="https://nyaa.si/xmlns/nyaa">
  <channel>
    <item>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <nyaa:infoHash>abcdef0123456789abcdef0123456789abcdef01</nyaa:infoHash>
      <nyaa:seeders>5</nyaa:seeders>
      <nyaa:size>1.0 GiB</nyaa:size>
    </item>
  </channel>
</rss>`

	p := newNyaaFixtureAt(t, nyaaServer(t, body, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	want := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01&dn=" +
		url.QueryEscape("[Good] Show - 01 (1080p).mkv")
	if results[0].URL != want {
		t.Errorf("url = %q, want the infoHash-built magnet fallback %q", results[0].URL, want)
	}
}

// TestNyaaSearchPreflightDropsDeadHosts pins the PR66 owner ruling:
// every surfaced result's .torrent bytes are pre-fetched (bounded,
// short per-URL budget) BEFORE the result surfaces; a dead host drops
// the result. Seedless items are filtered first and never probed.
func TestNyaaSearchPreflightDropsDeadHosts(t *testing.T) {
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

	feed := nyaaFeed(
		nyaaItemXML("Show - 01", live.URL+"/download/1.torrent", "", "10"),
		nyaaItemXML("Show - dead", "http://"+dead.Addr().String()+"/download/2.torrent", "", "10"),
		nyaaItemXML("Show - seedless", live.URL+"/download/3.torrent", "", "0"),
		nyaaItemXML("Show - 03", live.URL+"/download/4.torrent", "", "7"),
	)
	p := newNyaa(nyaaServer(t, feed, nil), testClient(t, "nyaa"), newOfflineTestEngine(t))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host and the seedless item dropped)", len(results))
	}
	if results[0].URL != live.URL+"/download/1.torrent" || results[1].URL != live.URL+"/download/4.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in feed order", results[0].URL, results[1].URL)
	}
	// The seedless item was filtered before the preflight: only the
	// two live results were fetched, each exactly once.
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestNyaaSearchPreflightFeedsIngestionNoRefetch: bytes that PASSED
// the preflight are handed to the engine right away, so the later
// GetEpisodes (the resolve leg) must NOT re-fetch the .torrent — the
// cache-reuse assertion of the ruling.
func TestNyaaSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
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
	p := newNyaa(nyaaServer(t, nyaaFeed(nyaaItemXML("Show - 01", link, "", "10")), nil),
		testClient(t, "nyaa"), newOfflineTestEngine(t))

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

// TestNyaaSearchPreflightNotMetainfoDropped: a host that answers
// HTTP 200 with NON-metainfo content (an HTML error page from the
// nyaa.si 504 flaps) is dead too — the content check drops it.
func TestNyaaSearchPreflightNotMetainfoDropped(t *testing.T) {
	t.Parallel()

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>504 Gateway Time-out</body></html>"))
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

	feed := nyaaFeed(
		nyaaItemXML("Show - wall", html.URL+"/download/1.torrent", "", "10"),
		nyaaItemXML("Show - good", live.URL+"/download/2.torrent", "", "10"),
	)
	p := newNyaa(nyaaServer(t, feed, nil), testClient(t, "nyaa"), newOfflineTestEngine(t))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/download/2.torrent" {
		t.Fatalf("results = %v, want only the live link", results)
	}
}

// TestNyaaSearchPreflightSlowHostDropped: the per-URL budget is short
// (~10s in production); a host stalling past it is dropped while the
// rest of the surface still surfaces.
func TestNyaaSearchPreflightSlowHostDropped(t *testing.T) {
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

	feed := nyaaFeed(
		nyaaItemXML("Show - slow", slow.URL+"/download/1.torrent", "", "10"),
		nyaaItemXML("Show - good", live.URL+"/download/2.torrent", "", "10"),
	)
	p := newNyaa(nyaaServer(t, feed, nil), testClient(t, "nyaa"), newOfflineTestEngine(t))
	p.preflightTimeout = 50 * time.Millisecond

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/download/2.torrent" {
		t.Fatalf("results = %v, want only the fast live link", results)
	}
}

// TestNyaaSearchPreflightLogsTypedReason: drops are logged with the
// URL and the typed failure reason, never silent.
func TestNyaaSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/download/1.torrent"
	feed := nyaaFeed(nyaaItemXML("Show - dead", deadURL, "", "10"))
	p := newNyaa(nyaaServer(t, feed, nil), testClient(t, "nyaa"), newOfflineTestEngine(t))

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

// TestNyaaSearchNoEngineSkipsPreflight pins the nil-engine rule:
// without the [torrent] engine there is nothing to preflight or feed,
// so Search keeps the legacy behavior (no prefetch requests — this is
// also what keeps hand-built unit tests network-free).
func TestNyaaSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	feed := nyaaFeed(
		nyaaItemXML("Show - 01", "https://nyaa.si/download/1.torrent", "abcdef0123456789abcdef0123456789abcdef01", "10"),
		nyaaItemXML("Show - 02", "https://nyaa.si/download/2.torrent", "", "10"),
	)
	p := newNyaaFixtureAt(t, nyaaServer(t, feed, &hits))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (nil engine: no preflight, nothing dropped)", len(results))
	}
	// Exactly ONE request happened: the RSS search itself. No
	// preflight attempted the fixture's nyaa.si download URLs.
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only)", hits)
	}
}
