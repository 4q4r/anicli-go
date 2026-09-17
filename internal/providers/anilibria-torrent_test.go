package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// Fixture provenance (PR37): anilibria-torrent_release.json carries
// verbatim live captures of GET /api/v1/anime/torrents/release/9789
// (aniliberty.top, 2026-09-17), trimmed of the huge nested release
// payload down to its identifying stub. Two constructed variations,
// both on the real item shape: item 2 has its magnet blanked to
// exercise the infohash fallback, item 3 is an unusable entry (no
// magnet, no hash) that must be skipped. The search responses reuse
// anilibria_search.json — the torrent provider consumes the same
// release-search endpoint and shape as the stream provider.

// newAnilibriaTorrentFixtureAt builds the provider against a fixture
// server serving the release search plus per-release torrent lists.
func newAnilibriaTorrentFixtureAt(t *testing.T, baseURL string) *AniLibriaTorrent {
	t.Helper()
	return newAnilibriaTorrent(baseURL, testClient(t, "anilibria-torrent"), nil)
}

// anilibriaTorrentFixtureServer serves the search fixture on
// /app/search/releases and per-release torrent fixtures on
// /anime/torrents/release/{id}; it records the request paths it saw.
func anilibriaTorrentFixtureServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		switch r.URL.Path {
		case "/app/search/releases":
			_, _ = w.Write(fixture(t, "anilibria_search.json"))
		case "/anime/torrents/release/9789":
			_, _ = w.Write(fixture(t, "anilibria-torrent_release.json"))
		case "/anime/torrents/release/10277":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

func TestAnilibriaTorrentSearchExpandsReleaseTorrents(t *testing.T) {
	t.Parallel()

	srv, _ := anilibriaTorrentFixtureServer(t)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Release 9789 carries two usable torrents; its third entry (no
	// magnet, no hash) must be skipped; release 10277 has no torrents
	// at all and contributes nothing.
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}

	// Verbatim live values (capture 2026-09-17): the API magnet rides
	// as-is — its tr= tracker announce URLs are peer-discovery value.
	first := results[0]
	if first.Title != "Dandadan - AniLiberty.TOP [WEBRip 1080p][AVC][1-12]" {
		t.Errorf("title = %q", first.Title)
	}
	if first.SourceID != "anilibria-torrent" {
		t.Errorf("source id = %q", first.SourceID)
	}
	wantMagnet := "magnet:?xt=urn:btih:b451a6b9b67383787be3273ec1a2a8e54cea3380&dn=Dandadan+-+AniLibria+%5BWEBRip+1080p%5D&xl=17448944888&tr=http://tr.libria.fun:2710/announce&tr=http://retracker.local/announce"
	if first.URL != wantMagnet {
		t.Errorf("url = %q, want the API magnet verbatim", first.URL)
	}
	if got := first.Meta[SearchMetaSize]; got != "16.3 GiB" {
		t.Errorf("size meta = %v, want 16.3 GiB (17448944888 bytes)", got)
	}
	if first.Meta[SearchMetaSeeders] != "22" || first.Meta[SearchMetaLeechers] != "1" {
		t.Errorf("seeders/leechers meta = %v/%v", first.Meta[SearchMetaSeeders], first.Meta[SearchMetaLeechers])
	}
	if first.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the API's 1080p", first.Meta[SearchMetaQuality])
	}

	// A blank magnet falls back to a magnet built from the API hash.
	second := results[1]
	wantFallback := "magnet:?xt=urn:btih:f3a14b1c76f3681442cad4c3cd397144c04e218f&dn=" +
		url.QueryEscape("Dandadan - AniLiberty.TOP [WEBRip 1080p][HEVC][1-12]")
	if second.URL != wantFallback {
		t.Errorf("fallback url = %q, want %q", second.URL, wantFallback)
	}
	if got := second.Meta[SearchMetaSize]; got != "3.3 GiB" {
		t.Errorf("size meta = %v, want 3.3 GiB (3549699018 bytes)", got)
	}
}

func TestAnilibriaTorrentSearchRequestParams(t *testing.T) {
	t.Parallel()

	srv, paths := anilibriaTorrentFixtureServer(t)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "dandadan"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	got := *paths
	if len(got) == 0 {
		t.Fatal("no requests reached the fixture server")
	}
	// First request: the release search, query carried as-is.
	if got[0] != "/app/search/releases?query=dandadan" {
		t.Errorf("search request = %q, want /app/search/releases?query=dandadan", got[0])
	}
	// Then one torrents fetch per search hit, by numeric release id.
	if !strings.Contains(got[1], "/anime/torrents/release/9789") || strings.Contains(got[1], "?") {
		t.Errorf("torrents request = %q, want /anime/torrents/release/9789 without stray params", got[1])
	}
	if !strings.Contains(got[2], "/anime/torrents/release/10277") {
		t.Errorf("torrents request = %q, want /anime/torrents/release/10277 (every search hit is probed)", got[2])
	}
}

func TestAnilibriaTorrentSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(fixture(t, "anilibria_search.json"))
	}))
	t.Cleanup(srv.Close)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

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

// TestAnilibriaTorrentSearchCapsReleases: a broad query can match
// dozens of releases; only the first TorrentSearchReleaseLimit are
// probed for torrents (latency bound on the sequential fan-out).
func TestAnilibriaTorrentSearchCapsReleases(t *testing.T) {
	t.Parallel()

	var many []map[string]any
	for i := range 9 {
		many = append(many, map[string]any{
			"id":    1000 + i,
			"alias": fmt.Sprintf("release-%d", i),
			"name":  map[string]any{"main": fmt.Sprintf("Release %d", i)},
		})
	}
	broad, err := json.Marshal(many)
	if err != nil {
		t.Fatalf("marshal broad fixture: %v", err)
	}

	releaseHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/app/search/releases" {
			_, _ = w.Write(broad)
			return
		}
		releaseHits++
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	results, err := p.Search(context.Background(), "broad")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if releaseHits != AniLibriaTorrentSearchReleaseLimit {
		t.Errorf("release fetches = %d, want the cap %d", releaseHits, AniLibriaTorrentSearchReleaseLimit)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 (all capped releases are torrent-less)", len(results))
	}
}

// TestAnilibriaTorrentSearchToleratesGeoHiddenRelease404: the API
// geo-hides content per requester IP, so a release stub can pass the
// search while its torrent list answers 404 (live-verified: Dandadan
// from a RU exit). That one release must contribute nothing while the
// other hits' torrents survive — the search must not fail.
func TestAnilibriaTorrentSearchToleratesGeoHiddenRelease404(t *testing.T) {
	t.Parallel()

	var search []map[string]any
	for _, id := range []int{9789, 5555, 10277} {
		search = append(search, map[string]any{
			"id":    id,
			"alias": fmt.Sprintf("release-%d", id),
			"name":  map[string]any{"main": fmt.Sprintf("Release %d", id)},
		})
	}
	broad, err := json.Marshal(search)
	if err != nil {
		t.Fatalf("marshal search fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/search/releases":
			_, _ = w.Write(broad)
		case "/anime/torrents/release/9789":
			_, _ = w.Write(fixture(t, "anilibria-torrent_release.json"))
		case "/anime/torrents/release/5555":
			// The geo-hidden release: hidden content answers 404.
			w.WriteHeader(http.StatusNotFound)
		case "/anime/torrents/release/10277":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	results, err := p.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search: %v (the geo-hidden release must not fail the search)", err)
	}
	// The two usable torrents of release 9789 survive the 404 of the
	// middle hit; the empty 10277 contributes nothing.
	if len(results) != 2 {
		t.Fatalf("results = %d, want the 2 surviving torrents of the healthy release", len(results))
	}
	if results[0].Title != "Dandadan - AniLiberty.TOP [WEBRip 1080p][AVC][1-12]" {
		t.Errorf("title = %q", results[0].Title)
	}
}

func TestAnilibriaTorrentSearchMalformedJSONTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	t.Cleanup(srv.Close)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed JSON must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anilibria-torrent" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want an anilibria-torrent search ProviderError", err)
	}
}

func TestAnilibriaTorrentSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anilibria-torrent" {
		t.Errorf("error = %v, want a provider-tagged error", err)
	}
}

// TestAnilibriaTorrentGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait; unreachable
// metadata fails loud on the caller's deadline (the nyaa contract).
func TestAnilibriaTorrentGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	eng := newOfflineTestEngineCfg(t, cfg.Torrent)
	p := newAnilibriaTorrent(AniLibriaAPIBase, testClient(t, "anilibria-torrent"), eng)

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
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

func TestAnilibriaTorrentCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	srv, _ := anilibriaTorrentFixtureServer(t)
	p := newAnilibriaTorrentFixtureAt(t, srv.URL)
	if !p.IsTorrent() {
		t.Error("anilibria-torrent must carry the torrent capability")
	}
	if p.ID() != "anilibria-torrent" {
		t.Errorf("id = %q", p.ID())
	}
	if p.Name() != "АниЛибрия (торренты)" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (RU dub audio, video)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ru" {
		t.Errorf("content language = %q, want ru (AniLibria dubs)", got)
	}
	if p.BaseURL() != srv.URL {
		t.Errorf("base URL = %q, want the fixture base", p.BaseURL())
	}
}

// TestAnilibriaTorrentNotUnconfiguredByDefault pins the no-credentials
// convention: nothing to configure — never in the disabled table with
// default settings (the [torrent] gating is the disabled-table rule).
func TestAnilibriaTorrentNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "anilibria-torrent" {
			t.Fatalf("anilibria-torrent must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestAnilibriaTorrentDisabledWhenTorrentOff pins the disabled-table
// rule shared with nyaa: without the [torrent] subsystem the provider
// cannot play anything, so it is not registered at all.
func TestAnilibriaTorrentDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false

	var found *DisabledProvider
	for _, d := range UnconfiguredProviders(cfg) {
		if d.ID == "anilibria-torrent" {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatal("anilibria-torrent must be unconfigured when [torrent] is disabled")
	}
	if !strings.Contains(found.Reason, "[torrent]") {
		t.Errorf("reason = %q, want the torrent-subsystem wording", found.Reason)
	}
}

// TestAnilibriaTorrentSizeFormatting pins the human size meta on the
// byte values the API reports (binary units, one decimal — the TUI
// torrent suffix convention).
func TestAnilibriaTorrentSizeFormatting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{3549699018, "3.3 GiB"},
		{17448944888, "16.3 GiB"},
	} {
		if got := humanBytes(tc.bytes); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
