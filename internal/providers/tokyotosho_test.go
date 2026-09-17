package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

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

func newTokyoToshoFixtureAt(t *testing.T, baseURL string) *TokyoTosho {
	t.Helper()
	return newTokyoTosho(baseURL, testClient(t, "tokyotosho"), nil)
}

func tokyoToshoServer(t *testing.T, body string, hits *int) string {
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

func TestTokyoToshoSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, string(fixture(t, "tokyotosho_search.xml")), nil))
	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 4 fixture items → 2 results: the non-Anime item is filtered by
	// category, the link-less constructed item is dropped (nyaa rule).
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
	// The RSS <link> is a direct .torrent URL (here a nyaa mirror of
	// the release) — the engine downloads it itself (URL ingest).
	if first.URL != "https://www.anirena.com/dl/200716" {
		t.Errorf("url = %q, want the RSS <link> .torrent URL verbatim", first.URL)
	}
	if got := first.Meta[SearchMetaSize]; got != "1.66GB" {
		t.Errorf("size meta = %v, want the feed's own 1.66GB", got)
	}
	if got := first.Meta[SearchMetaQuality]; got != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", got)
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

// TestTokyoToshoSearchFiltersToAnimeCategory: the search feed's
// type=1 filter is soft (live capture: 74 Anime + 60 Raws + others on
// an anime query), so the provider keeps only exact Anime-category
// items — an anime-search provider must not hand the user raws,
// manga or hentai (live-verified 2026-09-17).
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

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, body, nil))
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

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, body, nil))
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

func TestTokyoToshoSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotPath = r.URL.Path
		_, _ = w.Write(fixture(t, "tokyotosho_search.xml"))
	}))
	t.Cleanup(srv.Close)
	p := newTokyoToshoFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "fate stay night"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/rss.php" {
		t.Errorf("path = %q, want the search RSS endpoint", gotPath)
	}
	if gotQuery.Get("terms") != "fate stay night" {
		t.Errorf("terms = %q, want the raw query (live-verified param — NOT `search=`, which the feed ignores, nor `q=`)", gotQuery.Get("terms"))
	}
	if gotQuery.Get("type") != "1" {
		t.Errorf("type = %q, want 1 (anime category)", gotQuery.Get("type"))
	}
}

func TestTokyoToshoSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, string(fixture(t, "tokyotosho_search.xml")), &hits))
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
// fixture (tokyotosho_empty.xml, live curl 2026-09-17:
// "</channel>\n</rss>\n"). That is the site's own zero-result shape:
// it must settle as empty results, never leak a raw "XML syntax
// error on line 1: unexpected end element </channel>".
func TestTokyoToshoSearchEmptyFooterIsZeroResults(t *testing.T) {
	t.Parallel()

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, string(fixture(t, "tokyotosho_empty.xml")), nil))
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
	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, truncated, nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("a feed broken mid-stream must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "tokyotosho" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a tokyotosho search ProviderError", err)
	}
}

func TestTokyoToshoSearchMalformedXMLTypedError(t *testing.T) {
	t.Parallel()

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, "this is not xml at all", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed XML must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "tokyotosho" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a tokyotosho search ProviderError", err)
	}
}

func TestTokyoToshoSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newTokyoToshoFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "tokyotosho" {
		t.Errorf("error = %v, want a tokyotosho-tagged ProviderError", err)
	}
}

// TestTokyoToshoGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait; unreachable
// metadata fails loud on the caller's deadline (the nyaa contract).
func TestTokyoToshoGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	p := newTokyoTosho(TokyoToshoBase, testClient(t, "tokyotosho"), eng)

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

func TestTokyoToshoCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newTokyoToshoFixtureAt(t, tokyoToshoServer(t, string(fixture(t, "tokyotosho_search.xml")), nil))
	if !p.IsTorrent() {
		t.Error("tokyotosho must carry the torrent capability")
	}
	if p.ID() != "tokyotosho" {
		t.Errorf("id = %q", p.ID())
	}
	if p.Name() != "TokyoTosho" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
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
// shared with nyaa: without the [torrent] subsystem the provider
// cannot play anything, so it is not registered at all.
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
