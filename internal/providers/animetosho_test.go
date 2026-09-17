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
	// The live item's torznab magneturl is base32 (nekoBT mirror), so
	// the link falls to a magnet built from the 40-hex infohash attr —
	// the engine ingests the infohash directly (nyaa rule).
	wantMagnet := "magnet:?xt=urn:btih:5f32a4ef1e889482acb910b4ce80b939fc29049e&dn=" +
		url.QueryEscape(batch.Title)
	if batch.URL != wantMagnet {
		t.Errorf("url = %q, want magnet %q", batch.URL, wantMagnet)
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

// TestAnimeToshoSearchPrefersHexMagnetAttr: when the magneturl attr
// carries a well-formed 40-hex btih (plus its tr= trackers), it rides
// verbatim — the announces aid peer discovery (anilibria-torrent rule).
// The live feed's magneturl is base32 and must NOT be taken (the engine
// contract is hex; the infohash attr covers that item).
func TestAnimeToshoSearchPrefersHexMagnetAttr(t *testing.T) {
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
</channel></rss>`

	p := newAnimeToshoFixtureAt(t, animeToshoServer(t, body, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].URL != "magnet:?xt=urn:btih:1111111111111111111111111111111111111111&tr=udp://tracker.example.org:1337/announce" {
		t.Errorf("hex magneturl = %q, want it verbatim (trackers ride)", results[0].URL)
	}
	if results[1].URL != "magnet:?xt=urn:btih:2222222222222222222222222222222222222222&dn="+url.QueryEscape("[B32] Show - 02 (720p).mkv") {
		t.Errorf("base32 magneturl item = %q, want the infohash-built magnet", results[1].URL)
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
// metadata fails loud on the caller's deadline (the nyaa contract).
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

// TestAnimeToshoDisabledWhenTorrentOff pins the disabled-table rule
// shared with nyaa: without the [torrent] subsystem the provider
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
