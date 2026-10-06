package providers

// The tokyotosho provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/tokyotosho/main.lua, the PR147 Go→Lua
// migration — the thirtieth and final one: with this slot no compiled
// factory remains in the roster). These tests pin the script through
// the same contracts.Provider surface and the same verbatim live
// captures the compiled Go implementation was held to. The torrent
// plumbing (the PR66 .torrent preflight, episodes and streams) stays
// GO: the roster slot is wrapped by the luaTorrent adapter
// (luatorrent.go) — the adapter's own contract lives in
// lua_torrent_test.go, the adapter×script integration pins in the
// second half of this file.
//
// Fixture provenance (PR38): tokyotosho_search.xml carries verbatim
// live captures of the site's own search RSS (GET
// https://www.tokyo-tosho.net/rss.php?terms=dandadan&type=1 — the
// "RSS Feed of these results" link the search page renders, 2026-09-17,
// anonymous 200), trimmed to two Anime items plus one real non-Anime
// item. The fourth item is constructed on the real element order with
// an empty <link> — it exercises the drop rule. Live-capture caveat
// baked into the parser: the feed's `type=1` filter is SOFT (the
// capture mixes Anime with Raws/Manga/Hentai), so the provider filters
// on <category>Anime</category> itself.

import (
	"bytes"
	"context"
	"errors"
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

func TestTokyoToshoSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, string(fixture(t, "tokyotosho_search.xml"))))
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 4 fixture items → 2 results: the non-Anime item is filtered by
	// category, the link-less constructed item is dropped (the TorrentBase rule).
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}

	first := results[0]
	if first.Title != "DanDaDan (ダンダダン) - S02E12 [END][1080p][x264 10bits][AAC][Multiple Subtitles]-NeoLX.mkv" {
		t.Errorf("title = %q", first.Title)
	}
	if first.SourceID != "tokyotosho" {
		t.Errorf("source id = %q, want tokyotosho", first.SourceID)
	}
	// The RSS <link> is a direct .torrent URL (here the capture's
	// cross-posted mirror of the release) — the engine downloads it itself (URL ingest).
	if first.URL != "https://www.anirena.com/dl/200716" {
		t.Errorf("url = %q, want the RSS <link> .torrent URL verbatim", first.URL)
	}
	if got := first.Meta[SearchMetaSize]; got != "1.66GB" {
		t.Errorf("size meta = %v, want the feed's own 1.66GB", got)
	}
	if got := first.Meta[SearchMetaQuality]; got != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", got)
	}
	// The TT feed carries NO seed fields at all: the seed meta key
	// must be absent so filterSeedless keeps the item (the fail-soft
	// "no field, no filter" contract).
	if _, ok := first.Meta[SearchMetaSeeders]; ok {
		t.Errorf("seeders meta = %v, want absent (the feed has no seed fields)", first.Meta[SearchMetaSeeders])
	}

	second := results[1]
	if second.Title != "[SubsPlease] Dandadan - 24 (1080p) [AD3DEA4E].mkv" {
		t.Errorf("second title = %q", second.Title)
	}
	if second.URL != "https://nyaa.si/view/2021168/torrent" {
		t.Errorf("second url = %q, want the RSS <link> .torrent URL", second.URL)
	}
	if got := second.Meta[SearchMetaSize]; got != "1.35GB" {
		t.Errorf("second size meta = %v, want the feed's own 1.35GB", got)
	}
}

func TestTokyoToshoSearchRequestParams(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "tokyotosho_search.xml"))
	})
	p := luaProvider(t, "tokyotosho", srv.URL)

	if _, err := p.Search(context.Background(), "fate stay night"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rec.Path != "/rss.php" {
		t.Errorf("path = %q, want the search RSS endpoint", rec.Path)
	}
	// The raw query rides terms= URL-encoded (the Go url.Values.Encode
	// shape the compiled provider sent: space → '+', keys sorted) and
	// type=1 is the anime category. terms= is the live-verified
	// parameter — NOT `search=`, which the feed ignores, nor `q=`.
	if want := "terms=fate+stay+night&type=1"; rec.Query != want {
		t.Errorf("query = %q, want %q", rec.Query, want)
	}
}

// TestTokyoToshoSearchFiltersToAnimeCategory: the search feed's
// type=1 filter is soft (live capture: 74 Anime + 60 Raws + others on
// an anime query), so the provider keeps only exact Anime-category
// items — an anime-search provider must not hand the user raws,
// manga or hentai (live-verified 2026-09-17; re-verified live through
// the proxy 2026-10-06: a «black lagoon» feed mixes 45 Hentai Manga
// and 24 Music items into the 21 Anime ones).
func TestTokyoToshoSearchFiltersToAnimeCategory(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Tokyo Toshokan</title>
    <item>
      <category>Anime</category>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <link><![CDATA[https://mirror.example/good.torrent]]></link>
      <description><![CDATA[Size: 700.00MB<br />]]></description>
    </item>
    <item>
      <category>Raws</category>
      <title>Show raw - 01 (1080p).ts</title>
      <link><![CDATA[https://mirror.example/raw.torrent]]></link>
      <description><![CDATA[Size: 700.00MB<br />]]></description>
    </item>
    <item>
      <category>Anime</category>
      <title>[Good] Show - 02 (720p).mkv</title>
      <link><![CDATA[https://mirror.example/nocat.torrent]]></link>
      <description><![CDATA[Size: 350.00MB<br />]]></description>
    </item>
</channel></rss>`

	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, body))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %v, want the 2 Anime items only", results)
	}
	if results[0].Title != "[Good] Show - 01 (1080p).mkv" {
		t.Errorf("title = %q", results[0].Title)
	}
}

// TestTokyoToshoSearchSkipsLinklessItems: an item without a usable
// <link> has nothing the engine could ingest — dropped, never a dead
// result. Size text is fail-soft: its absence does not drop an item.
func TestTokyoToshoSearchSkipsLinklessItems(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Tokyo Toshokan</title>
    <item>
      <category>Anime</category>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <link><![CDATA[https://mirror.example/good.torrent]]></link>
      <description><![CDATA[No size line at all]]></description>
    </item>
    <item>
      <category>Anime</category>
      <title>[Broken] Empty link</title>
      <link><![CDATA[]]></link>
      <description><![CDATA[Size: 700.00MB<br />]]></description>
    </item>
    <item>
      <category>Anime</category>
      <title>[Broken] No link element</title>
      <description><![CDATA[Size: 700.00MB<br />]]></description>
    </item>
</channel></rss>`

	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, body))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the usable item)", len(results))
	}
	if results[0].Meta[SearchMetaSize] != "" {
		t.Errorf("size meta = %v, want empty (fail-soft, feed owns the text)", results[0].Meta[SearchMetaSize])
	}
}

// TestTokyoToshoSearchEmptyQueryFailsLoud: the empty-query guard is a
// caller-bug wall, zero network.
func TestTokyoToshoSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := tokyotoshoServerCounted(t, string(fixture(t, "tokyotosho_search.xml")), &hits)
	p := luaProvider(t, "tokyotosho", srv)
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

// TestTokyoToshoSearchEmptyFooterIsZeroResults pins the PR42 root
// cause fix: TT's search RSS answers an unmatched query (Cyrillic
// among them — the feed indexes latin release names only) with HTTP
// 200 and a bare feed FOOTER — the captured real bytes ride the
// fixture (tokyotosho_empty.xml, live curl 2026-09-17, re-verified
// live 2026-10-06: "</channel>\n</rss>\n"). That is the site's own
// zero-result shape: it must settle as empty results, never leak a
// raw "XML syntax error on line 1: unexpected end element </channel>".
func TestTokyoToshoSearchEmptyFooterIsZeroResults(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, string(fixture(t, "tokyotosho_empty.xml"))))
	results, err := p.Search(context.Background(), "Пираты «Чёрной лагуны»")
	if err != nil {
		t.Fatalf("zero-result footer must not error, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none", results)
	}
}

// TestTokyoToshoSearchMidStreamTruncationStaysTypedError guards the
// classification boundary: a body that OPENS a real feed and breaks
// mid-stream is a provider malfunction — it stays a typed
// ProviderError, never silently degrades to "no results".
func TestTokyoToshoSearchMidStreamTruncationStaysTypedError(t *testing.T) {
	t.Parallel()

	const truncated = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Tokyo Toshokan</title>
    <item>
      <category>Anime`
	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, truncated))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("a feed broken mid-stream must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "tokyotosho" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a tokyotosho search ProviderError", err)
	}
}

func TestTokyoToshoSearchMalformedEnvelopeTypedError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "tokyotosho", tokyotoshoServer(t, "this is not xml at all"))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed RSS must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "tokyotosho" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a tokyotosho search ProviderError", err)
	}
}

// TestTokyoToshoSearchHTTPErrorFailsLoud pins the HTTP failure wall
// (the anirena migration precedent): the Lua transport surfaces the
// netclient sentinel classes through the typed anicli:<kind>: markers
// — 503 carries the plain StatusError class — so the pin here is the
// loud, provider-tagged failure.
func TestTokyoToshoSearchHTTPErrorFailsLoud(t *testing.T) {
	t.Parallel()

	srv := tokyotoshoServerStatus(t, http.StatusServiceUnavailable)
	p := luaProvider(t, "tokyotosho", srv)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	if !strings.Contains(err.Error(), "tokyotosho") {
		t.Errorf("error = %v, want the provider-tagged message", err)
	}
}

// TestTokyoToshoCapabilityDeclarations pins the script-declared
// capability surfaces (the Adapt composite): the JA content language,
// the latin-only index routing (PR42) and the both-type catalog
// assessment — the exact declarations the compiled provider carried
// in Go code.
func TestTokyoToshoCapabilityDeclarations(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "tokyotosho")
	if p.Name() != "TokyoTosho" {
		t.Errorf("name = %q, want TokyoTosho (the TUI display name)", p.Name())
	}
	if p.BaseURL() != "https://www.tokyo-tosho.net" {
		t.Errorf("base url = %q, want the site root (www host live-verified)", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the adapted provider lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP audio with subs)", got)
	}
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the adapted provider lost the NamePreference surface")
	}
	if np.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin (PR42 latin-only index)", np.NamePreference())
	}
}

// TestTokyoToshoNotUnconfiguredByDefault pins the no-credentials
// convention: nothing to configure — never in the disabled table with
// default settings (the [torrent] gating is the disabled-table rule).
func TestTokyoToshoNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "tokyotosho" {
			t.Fatalf("tokyotosho must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestTokyoToshoDisabledWhenTorrentOff pins the disabled-table rule
// shared across the family: without the [torrent] subsystem the
// provider cannot play anything, so it is not registered at all. The
// gate is script-independent (the PR142 doctrine): the engine is Go
// infrastructure no script replaces, so the migration left the
// unconfigured rule in place.
func TestTokyoToshoDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false

	var found *DisabledProvider
	for _, d := range UnconfiguredProviders(cfg) {
		if d.ID == "tokyotosho" {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatal("tokyotosho must be unconfigured when [torrent] is disabled")
	}
	if !strings.Contains(found.Reason, "[torrent]") {
		t.Errorf("reason = %q, want the torrent-subsystem wording", found.Reason)
	}
}

// --- the luaTorrent adapter × the bundled script (the compiled
// provider's torrent contract, ported) ---

// TestTokyoToshoTorrentSlotInRegistry pins the migration-safety parity:
// through the REAL registry the tokyotosho slot keeps every surface it
// carried as a compiled provider — the torrent capability (the
// parity smoke's torrent leg routes on it), the engine-injection
// duck, the JA content language and the latin index routing.
func TestTokyoToshoTorrentSlotInRegistry(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	torrentFound := false
	for _, id := range reg.TorrentProviderIDs() {
		if id == "tokyotosho" {
			torrentFound = true
		}
	}
	if !torrentFound {
		t.Fatal("tokyotosho lost the torrent capability in the registry (the luaTorrent adapter wrap is missing?)")
	}
	p, ok := reg.Get("tokyotosho")
	if !ok {
		t.Fatal("tokyotosho is not registered")
	}
	// The wrapper layers peel the same way the registry's own wiring
	// does (the engine injection runs on the bare list BEFORE the
	// delegator wraps).
	if _, ok := bareProvider(p).(interface{ SetEngine(*torrent.Engine) }); !ok {
		t.Fatal("the tokyotosho slot lost the engine-injection surface")
	}
	if got := reg.ContentLanguage("tokyotosho"); got != "ja" {
		t.Errorf("content language = %q, want ja through the adapter", got)
	}
	if got := reg.NamePreference("tokyotosho"); got != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin through the adapter", got)
	}
}

// TestTokyoToshoGetEpisodesDelegatesToEpisodesWait: the adapter's
// GetEpisodes rides the base's bounded metadata wait; unreachable
// metadata fails loud on the caller's deadline (the TorrentBase
// contract).
func TestTokyoToshoGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	inner := luaProvider(t, "tokyotosho", tokyotoshoServer(t, tokyotoshoFeed()))
	p := newLuaTorrent(inner, nil, nil)
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
	if !strings.Contains(err.Error(), "торренты") {
		t.Errorf("error = %v, want the torrent-base wait failure", err)
	}
}

// newTokyoToshoLuaTorrent builds the migration shape: the bundled
// script (production base re-pointed at baseURL) wrapped in the
// torrent adapter with an offline engine wired.
func newTokyoToshoLuaTorrent(t *testing.T, baseURL string) *luaTorrent {
	t.Helper()
	inner := luaProvider(t, "tokyotosho", baseURL)
	a := newLuaTorrent(inner, nil, nil)
	a.SetEngine(newOfflineTestEngine(t))
	return a
}

// TestTokyoToshoSearchPreflightDropsDeadHosts pins the PR53 owner
// ruling through the adapter: every surfaced result's .torrent bytes
// are pre-fetched (bounded, short per-URL budget) BEFORE the result
// surfaces; a dead host (HTTP error, refused dial) drops the result.
// Survivors keep feed order and metadata. The feed has no seed
// fields, so nothing is dropped by the seedless filter first — the
// preflight is the only gate here.
func TestTokyoToshoSearchPreflightDropsDeadHosts(t *testing.T) {
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

	feed := tokyotoshoFeed(
		tokyotoshoItem("Anime", "[Good] Show - 01", live.URL+"/good1.torrent", "Size: 700.00MB<br />"),
		tokyotoshoItem("Anime", "[Good] Show - dead", "http://"+dead.Addr().String()+"/dead.torrent", "Size: 700.00MB<br />"),
		tokyotoshoItem("Anime", "[Good] Show - 03", live.URL+"/good2.torrent", "Size: 700.00MB<br />"),
	)
	p := newTokyoToshoLuaTorrent(t, tokyotoshoServer(t, feed))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host dropped)", len(results))
	}
	if results[0].URL != live.URL+"/good1.torrent" || results[1].URL != live.URL+"/good2.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in feed order", results[0].URL, results[1].URL)
	}
	// The preflight fetched each .torrent exactly once.
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestTokyoToshoSearchPreflightFeedsIngestionNoRefetch: bytes that
// PASSED the preflight are handed to the engine right away, so the
// later GetEpisodes (the resolve leg) must NOT re-fetch the .torrent —
// the cache-reuse assertion of the ruling.
func TestTokyoToshoSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
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
	link := live.URL + "/good.torrent"
	p := newTokyoToshoLuaTorrent(t, tokyotoshoServer(t, tokyotoshoFeed(
		tokyotoshoItem("Anime", "[Good] Show - 01", link, "Size: 700.00MB<br />"))))

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

// TestTokyoToshoSearchPreflightNotMetainfoDropped: a host that answers
// HTTP 200 with NON-metainfo content (login wall, parked page) is dead
// too — the content check drops it.
func TestTokyoToshoSearchPreflightNotMetainfoDropped(t *testing.T) {
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

	feed := tokyotoshoFeed(
		tokyotoshoItem("Anime", "[Good] Show - wall", html.URL+"/wall.torrent", "Size: 700.00MB<br />"),
		tokyotoshoItem("Anime", "[Good] Show - good", live.URL+"/good.torrent", "Size: 700.00MB<br />"),
	)
	p := newTokyoToshoLuaTorrent(t, tokyotoshoServer(t, feed))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/good.torrent" {
		t.Fatalf("results = %v, want only the live link", results)
	}
}

// TestTokyoToshoSearchPreflightSlowHostDropped: the per-URL budget is
// short (~10s in production); a host stalling past it is dropped while
// the rest of the surface still surfaces.
func TestTokyoToshoSearchPreflightSlowHostDropped(t *testing.T) {
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

	feed := tokyotoshoFeed(
		tokyotoshoItem("Anime", "[Good] Show - slow", slow.URL+"/slow.torrent", "Size: 700.00MB<br />"),
		tokyotoshoItem("Anime", "[Good] Show - good", live.URL+"/good.torrent", "Size: 700.00MB<br />"),
	)
	p := newTokyoToshoLuaTorrent(t, tokyotoshoServer(t, feed))
	p.preflightTimeout = 50 * time.Millisecond

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].URL != live.URL+"/good.torrent" {
		t.Fatalf("results = %v, want only the fast live link", results)
	}
}

// TestTokyoToshoSearchPreflightLogsTypedReason: drops are logged with
// the URL and the typed failure reason, never silent (the registry's
// logger seam routes through the adapter's SetLogger forward).
func TestTokyoToshoSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/dead.torrent"
	feed := tokyotoshoFeed(tokyotoshoItem("Anime", "[Good] Show - dead", deadURL, "Size: 700.00MB<br />"))

	logBuf := &bytes.Buffer{}
	inner := luaProviderWithLogger(t, "tokyotosho", tokyotoshoServer(t, feed), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := newLuaTorrent(inner, nil, nil)
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

// --- helpers ---

// torrentFixtureServer serves real metainfo bytes as a .torrent host
// and counts how often its .torrent path was fetched (atomic: the
// preflight fans out concurrently). Shared with the sibling torrent
// suites (animetosho, anirena, rutor, the adapter pins).
func torrentFixtureServer(t *testing.T, torrentBytes []byte, fetches *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches != nil {
			fetches.Add(1)
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrentBytes)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tokyotoshoServer serves body on every request (the RSS search
// endpoint).
func tokyotoshoServer(t *testing.T, body string) string {
	t.Helper()
	return tokyotoshoServerCounted(t, body, nil)
}

// tokyotoshoServerCounted is tokyotoshoServer with a request counter.
func tokyotoshoServerCounted(t *testing.T, body string, hits *int) string {
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

// tokyotoshoServerStatus serves the bare status code on every request.
func tokyotoshoServerStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// tokyotoshoItem renders one RSS item fragment on the real element
// order (category/title/link/description).
func tokyotoshoItem(category, title, link, desc string) string {
	var b strings.Builder
	b.WriteString("<item>\n<category>" + category + "</category>\n" +
		"<title>" + title + "</title>\n" +
		"<link><![CDATA[" + link + "]]></link>\n" +
		"<description><![CDATA[" + desc + "]]></description>\n</item>")
	return b.String()
}

// tokyotoshoFeed wraps item fragments into the TT RSS envelope.
func tokyotoshoFeed(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Tokyo Toshokan</title>` +
		strings.Join(items, "") + `</channel></rss>`
}
