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

// Fixture provenance (PR38): animetosho_search.xml carries verbatim
// live captures of the newznab search API (GET
// https://feed.animetosho.org/api?t=search&q=dandadan&limit=5&offset=0,
// 2026-09-17, anonymous 200), trimmed to two items. The third item is
// constructed on the real element order with every attr stripped — it
// exercises the enclosure (.torrent URL) fallback and fail-soft meta.

func newAnimeToshoFixtureAt(t *testing.T, baseURL string) *AnimeTosho {
	t.Helper()
	return newAnimeTosho(baseURL, testClient(t, "animetosho"), nil)
}

func animeToshoServer(t *testing.T, body string, hits *int) string {
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

func TestAnimeToshoSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, string(fixture(t, "animetosho_search.xml")), nil))
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}

	batch := results[0]
	if batch.Title != "[ED3N] DAN DA DAN (Season 1) (BD 1080p AV1) [Dual Audio]" {
		t.Errorf("title = %q", batch.Title)
	}
	if batch.SourceID != "animetosho" {
		t.Errorf("source id = %q, want animetosho", batch.SourceID)
	}
	// The item's <enclosure> is the direct .torrent URL on AT's own
	// storage — PR66 ingestion rides the preflighted bytes, never the
	// tracker-less magnet built from the infohash attr (the DHT-only
	// metadata path that timed out the PR52 smoke; the live magneturl
	// is base32 and is not taken anyway).
	wantEnclosure := "https://storage.animetosho.org/torrent/5f32a4ef1e889482acb910b4ce80b939fc29049e/%5BED3N%5D%20DAN%20DA%20DAN%20%28Season%201%29%20%28BD%201080p%20AV1%29%20%5BDual%20Audio%5D.torrent"
	if batch.URL != wantEnclosure {
		t.Errorf("url = %q, want the enclosure .torrent URL %q", batch.URL, wantEnclosure)
	}
	if batch.Meta[SearchMetaSize] != "21.6 GiB" {
		t.Errorf("size meta = %v, want 21.6 GiB (23175675801 bytes)", batch.Meta[SearchMetaSize])
	}
	if batch.Meta[SearchMetaSeeders] != "10" || batch.Meta[SearchMetaLeechers] != "1" {
		t.Errorf("seeders/leechers meta = %v/%v", batch.Meta[SearchMetaSeeders], batch.Meta[SearchMetaLeechers])
	}
	if batch.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", batch.Meta[SearchMetaQuality])
	}

	second := results[1]
	if second.Title != "[Okay-Subs] Dan Da Dan S1 (BD 1080p Dual-Audio)" {
		t.Errorf("second title = %q", second.Title)
	}
	wantSecond := "https://storage.animetosho.org/torrent/551d254951545609236617066627b25cba78155b/%5BOkay-Subs%5D%20Dan%20Da%20Dan%20S1%20%28BD%201080p%20Dual-Audio%29.torrent"
	if second.URL != wantSecond {
		t.Errorf("second url = %q, want the enclosure .torrent URL %q", second.URL, wantSecond)
	}
	if second.Meta[SearchMetaSize] != "54.7 GiB" {
		t.Errorf("second size meta = %v, want 54.7 GiB (58776096237 bytes)", second.Meta[SearchMetaSize])
	}
	if second.Meta[SearchMetaSeeders] != "331" || second.Meta[SearchMetaLeechers] != "6" {
		t.Errorf("second seeders/leechers meta = %v/%v", second.Meta[SearchMetaSeeders], second.Meta[SearchMetaLeechers])
	}

	// No attrs on the wire → the <enclosure> .torrent URL is the link.
	fallback := results[2]
	if fallback.URL != "https://storage.animetosho.org/torrent/abcdef0123456789abcdef0123456789abcdef01/%5BConstructed%5D%20No%20Attrs%20Show%20%281080p%29.torrent" {
		t.Errorf("fallback url = %q, want the enclosure .torrent URL", fallback.URL)
	}
	if fallback.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("fallback quality meta = %v, want the PR35 badge from the title", fallback.Meta[SearchMetaQuality])
	}
}

// TestAnimeToshoSearchPrefersEnclosureTorrent pins the PR66 link
// preference: the <enclosure> .torrent URL on AT's own storage wins —
// it is the preflightable, bytes-ingestible link (the PR53 pattern).
// Magnets only serve as the no-enclosure fallback: a hex magneturl
// rides verbatim (its tr= announces aid peer discovery), otherwise the
// magnet is built from the 40-hex infohash attr. The live feed's
// magneturl is base32 and must NOT be taken (the engine contract is
// hex; the enclosure covers that item).
func TestAnimeToshoSearchPrefersEnclosureTorrent(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
    <item>
      <title>[Hex] Show - 01 (1080p).mkv</title>
      <link>https://animetosho.org/view/hex.n1</link>
      <enclosure url="https://storage.animetosho.org/torrent/1/x.torrent" type="application/x-bittorrent" length="0"/>
      <torznab:attr name="infohash" value="1111111111111111111111111111111111111111"/>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:1111111111111111111111111111111111111111&amp;tr=udp://tracker.example.org:1337/announce"/>
      <torznab:attr name="size" value="1073741824"/>
    </item>
    <item>
      <title>[B32] Show - 02 (720p).mkv</title>
      <link>https://animetosho.org/view/b32.n2</link>
      <enclosure url="https://storage.animetosho.org/torrent/2/y.torrent" type="application/x-bittorrent" length="0"/>
      <torznab:attr name="infohash" value="2222222222222222222222222222222222222222"/>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:KZLR34YVXTPNFGFXDDFZETUHGG67DUMD&amp;tr=udp://tracker.example.org:1337/announce"/>
    </item>
    <item>
      <title>[NoEnc] Show - 03 (1080p).mkv</title>
      <link>https://animetosho.org/view/noenc.n3</link>
      <torznab:attr name="infohash" value="3333333333333333333333333333333333333333"/>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:3333333333333333333333333333333333333333&amp;tr=udp://tracker.example.org:1337/announce"/>
    </item>
    <item>
      <title>[NoEncNoMagnet] Show - 04 (1080p).mkv</title>
      <link>https://animetosho.org/view/fallback.n4</link>
      <torznab:attr name="infohash" value="4444444444444444444444444444444444444444"/>
    </item>
</channel></rss>`

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, body, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].URL != "https://storage.animetosho.org/torrent/1/x.torrent" {
		t.Errorf("hex magneturl item = %q, want the enclosure .torrent URL (own storage, preflightable)", results[0].URL)
	}
	if results[1].URL != "https://storage.animetosho.org/torrent/2/y.torrent" {
		t.Errorf("base32 magneturl item = %q, want the enclosure .torrent URL", results[1].URL)
	}
	if results[2].URL != "magnet:?xt=urn:btih:3333333333333333333333333333333333333333&tr=udp://tracker.example.org:1337/announce" {
		t.Errorf("no-enclosure hex magneturl = %q, want it verbatim (trackers ride)", results[2].URL)
	}
	if results[3].URL != "magnet:?xt=urn:btih:4444444444444444444444444444444444444444&dn="+url.QueryEscape("[NoEncNoMagnet] Show - 04 (1080p).mkv") {
		t.Errorf("no-enclosure no-magneturl = %q, want the infohash-built magnet", results[3].URL)
	}
}

func TestAnimeToshoSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotPath = r.URL.Path
		_, _ = w.Write(fixture(t, "animetosho_search.xml"))
	}))
	t.Cleanup(srv.Close)
	p := newAnimeToshoFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "fate stay night"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/api" {
		t.Errorf("path = %q, want the newznab api endpoint", gotPath)
	}
	if gotQuery.Get("t") != "search" {
		t.Errorf("t = %q, want search", gotQuery.Get("t"))
	}
	if gotQuery.Get("q") != "fate stay night" {
		t.Errorf("q = %q, want the raw query", gotQuery.Get("q"))
	}
	if gotQuery.Get("cat") != "5070" {
		t.Errorf("cat = %q, want 5070 (anime)", gotQuery.Get("cat"))
	}
	if gotQuery.Get("offset") != "0" {
		t.Errorf("offset = %q, want 0 (first page)", gotQuery.Get("offset"))
	}
	if gotQuery.Get("limit") != "30" {
		t.Errorf("limit = %q, want the bounded page size 30", gotQuery.Get("limit"))
	}
}

func TestAnimeToshoSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, string(fixture(t, "animetosho_search.xml")), &hits))
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

// TestAnimeToshoSearchSkipsUnusableItems: an item without a title, or
// with a title but none of magneturl/infohash/enclosure, has nothing
// the engine could ingest — dropped, never a dead result.
func TestAnimeToshoSearchSkipsUnusableItems(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
    <item>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <link>https://animetosho.org/view/good.n1</link>
      <enclosure url="https://storage.animetosho.org/torrent/3/z.torrent" type="application/x-bittorrent" length="0"/>
    </item>
    <item>
      <title></title>
      <link>https://animetosho.org/view/nameless.n2</link>
    </item>
    <item>
      <title>[Broken] No link no attrs</title>
      <torznab:attr name="size" value="1024"/>
    </item>
</channel></rss>`

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, body, nil))
	results, err := p.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the usable item)", len(results))
	}
	if results[0].URL != "https://storage.animetosho.org/torrent/3/z.torrent" {
		t.Errorf("url = %q, want the enclosure", results[0].URL)
	}
}

func TestAnimeToshoSearchMalformedXMLTypedError(t *testing.T) {
	t.Parallel()

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, "this is not xml at all", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed XML must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "animetosho" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want an animetosho search ProviderError", err)
	}
}

func TestAnimeToshoSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newAnimeToshoFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "animetosho" {
		t.Errorf("error = %v, want an animetosho-tagged ProviderError", err)
	}
}

// TestAnimeToshoGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait; unreachable
// metadata fails loud on the caller's deadline (the TorrentBase contract).
func TestAnimeToshoGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	p := newAnimeTosho(AnimeToshoFeedBase, testClient(t, "animetosho"), eng)

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

func TestAnimeToshoCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, string(fixture(t, "animetosho_search.xml")), nil))
	if !p.IsTorrent() {
		t.Error("animetosho must carry the torrent capability")
	}
	if p.ID() != "animetosho" {
		t.Errorf("id = %q", p.ID())
	}
	if p.Name() != "AnimeTosho" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP audio with subs)", got)
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the feed host, not empty")
	}
}

// TestAnimeToshoNotUnconfiguredByDefault pins the no-credentials
// convention: nothing to configure — never in the disabled table with
// default settings (the [torrent] gating is the disabled-table rule).
func TestAnimeToshoNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "animetosho" {
			t.Fatalf("animetosho must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestAnimeToshoDisabledWhenTorrentOff pins the disabled-table rule:
// without the [torrent] subsystem the provider
// cannot play anything, so it is not registered at all.
func TestAnimeToshoDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false

	var found *DisabledProvider
	for _, d := range UnconfiguredProviders(cfg) {
		if d.ID == "animetosho" {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatal("animetosho must be unconfigured when [torrent] is disabled")
	}
	if !strings.Contains(found.Reason, "[torrent]") {
		t.Errorf("reason = %q, want the torrent-subsystem wording", found.Reason)
	}
}

// --- PR66: .torrent-bytes ingestion (the tokyotosho PR53 pattern).
// The search result link is the <enclosure> .torrent URL on AT's own
// storage; a search-time preflight pre-fetches every result's bytes
// (bounded, short per-URL budget) and drops the dead ones BEFORE they
// surface (owner standing rule), handing the survivors' bytes to the
// engine — the tracker-less synthesized magnet (DHT-only metadata, the
// PR52 smoke killer) is only the no-enclosure fallback now.

// atItemXML renders one newznab item fragment: title, optional
// <enclosure> .torrent URL, optional magneturl/infohash attrs (values
// must be pre-escaped for an XML attribute), the seeder count (the
// PR44 filter keys on it).
func atItemXML(title, enclosure, magnetURL, infoHash, seeders string) string {
	var b strings.Builder
	b.WriteString("<item><title>" + title + "</title>")
	if enclosure != "" {
		b.WriteString(`<enclosure url="` + enclosure + `" type="application/x-bittorrent" length="0"/>`)
	}
	if magnetURL != "" {
		b.WriteString(`<torznab:attr name="magneturl" value="` + magnetURL + `"/>`)
	}
	if infoHash != "" {
		b.WriteString(`<torznab:attr name="infohash" value="` + infoHash + `"/>`)
	}
	b.WriteString(`<torznab:attr name="seeders" value="` + seeders + `"/></item>`)
	return b.String()
}

// atFeed wraps item fragments into the newznab RSS envelope.
func atFeed(items ...string) string {
	return `<?xml version="1.0" encoding="utf-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>` +
		strings.Join(items, "") + `</channel></rss>`
}

// TestAnimeToshoSearchPreflightDropsDeadHosts pins the PR66 owner
// ruling: every surfaced result's .torrent bytes are pre-fetched
// (bounded, short per-URL budget) BEFORE the result surfaces; a dead
// host drops the result. Seedless items are filtered first and never
// probed.
func TestAnimeToshoSearchPreflightDropsDeadHosts(t *testing.T) {
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

	feed := atFeed(
		atItemXML("Show - 01", live.URL+"/storage/1.torrent", "", "", "10"),
		atItemXML("Show - dead", "http://"+dead.Addr().String()+"/storage/2.torrent", "", "", "10"),
		atItemXML("Show - seedless", live.URL+"/storage/3.torrent", "", "", "0"),
		atItemXML("Show - 03", live.URL+"/storage/4.torrent", "", "", "7"),
	)
	p := newAnimeTosho(animeToshoServer(t, feed, nil), testClient(t, "animetosho"), newOfflineTestEngine(t))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host and the seedless item dropped)", len(results))
	}
	if results[0].URL != live.URL+"/storage/1.torrent" || results[1].URL != live.URL+"/storage/4.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in feed order", results[0].URL, results[1].URL)
	}
	// The seedless item was filtered before the preflight: only the
	// two live results were fetched, each exactly once.
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestAnimeToshoSearchPreflightFeedsIngestionNoRefetch: bytes that
// PASSED the preflight are handed to the engine right away, so the
// later GetEpisodes (the resolve leg) must NOT re-fetch the .torrent —
// the cache-reuse assertion of the ruling.
func TestAnimeToshoSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
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
	link := live.URL + "/storage/1.torrent"
	p := newAnimeTosho(animeToshoServer(t, atFeed(atItemXML("Show - 01", link, "", "", "10")), nil),
		testClient(t, "animetosho"), newOfflineTestEngine(t))

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

// TestAnimeToshoSearchPreflightLogsTypedReason: drops are logged with
// the URL and the typed failure reason, never silent.
func TestAnimeToshoSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/storage/1.torrent"
	p := newAnimeTosho(animeToshoServer(t, atFeed(atItemXML("Show - dead", deadURL, "", "", "10")), nil),
		testClient(t, "animetosho"), newOfflineTestEngine(t))

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

// TestAnimeToshoSearchNoEngineSkipsPreflight pins the nil-engine rule:
// without the [torrent] engine there is nothing to preflight or feed,
// so Search keeps the legacy behavior (no prefetch requests — this is
// also what keeps hand-built unit tests network-free).
func TestAnimeToshoSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, string(fixture(t, "animetosho_search.xml")), &hits))
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (nil engine: no preflight, nothing dropped)", len(results))
	}
	// Exactly ONE request happened: the newznab search itself. No
	// preflight attempted the fixture's storage.animetosho.org URLs.
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only)", hits)
	}
}

// TestAnimeToshoHumanBytesAttr pins the byte-count attribute parse:
// strict integers convert to the TUI convention, anything else —
// including numeric-prefixed garbage, which a lenient prefix parse
// would silently truncate — rides verbatim (fail-soft).
func TestAnimeToshoHumanBytesAttr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ raw, want string }{
		{"23175675801", "21.6 GiB"},
		{"0", "0 B"},
		{"  1024  ", "1.0 KiB"},
		{"abc", "abc"},
		{"123abc", "123abc"},
		{"", ""},
	} {
		if got := humanBytesAttr(tc.raw); got != tc.want {
			t.Errorf("humanBytesAttr(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
