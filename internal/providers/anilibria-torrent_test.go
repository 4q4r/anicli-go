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

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
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
//
// The provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anilibria-torrent/main.lua, the
// PR145 Go→Lua migration — the roster's first torrent script): these
// tests pin the script through the same contracts.Provider surface
// and the same verbatim live-capture fixtures the compiled Go
// implementation was held to. The harness rewrites the script's
// production base_url literal onto the fixture server, so the /api/v1
// prefix rides along in every request pin below. The search-side
// contract is the script's; the episode/stream legs stay on the Go
// TorrentBase (the engine consumes the surfaced magnets unchanged —
// the owner ruling), pinned at the roster level below.

// anilibriaTorrentFixtureServer serves the search fixture on
// /api/v1/app/search/releases and per-release torrent fixtures on
// /api/v1/anime/torrents/release/{id}; it records the request paths
// it saw, in order (the fan-out shape pin).
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
		case "/api/v1/app/search/releases":
			_, _ = w.Write(fixture(t, "anilibria_search.json"))
		case "/api/v1/anime/torrents/release/9789":
			_, _ = w.Write(fixture(t, "anilibria-torrent_release.json"))
		case "/api/v1/anime/torrents/release/10277":
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
	p := luaProvider(t, "anilibria-torrent", srv.URL)

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
	if second.Meta[SearchMetaSeeders] != "42" || second.Meta[SearchMetaLeechers] != "0" {
		t.Errorf("seeders/leechers meta = %v/%v", second.Meta[SearchMetaSeeders], second.Meta[SearchMetaLeechers])
	}
}

func TestAnilibriaTorrentSearchRequestParams(t *testing.T) {
	t.Parallel()

	srv, paths := anilibriaTorrentFixtureServer(t)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

	if _, err := p.Search(context.Background(), "dandadan"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	got := *paths
	if len(got) == 0 {
		t.Fatal("no requests reached the fixture server")
	}
	// First request: the release search, query carried as-is (the
	// /api/v1 prefix derives from the rewritten base_url literal).
	if got[0] != "/api/v1/app/search/releases?query=dandadan" {
		t.Errorf("search request = %q, want /api/v1/app/search/releases?query=dandadan", got[0])
	}
	// Then one torrents fetch per search hit, by numeric release id.
	if !strings.Contains(got[1], "/api/v1/anime/torrents/release/9789") || strings.Contains(got[1], "?") {
		t.Errorf("torrents request = %q, want /api/v1/anime/torrents/release/9789 without stray params", got[1])
	}
	if !strings.Contains(got[2], "/api/v1/anime/torrents/release/10277") {
		t.Errorf("torrents request = %q, want /api/v1/anime/torrents/release/10277 (every search hit is probed)", got[2])
	}
}

func TestAnilibriaTorrentSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilibria_search.json"))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

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
// dozens of releases; only the first six are probed for torrents
// (latency bound on the sequential fan-out — the compiled provider's
// AniLibriaTorrentSearchReleaseLimit, kept by the script).
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
		if strings.HasSuffix(r.URL.Path, "/app/search/releases") {
			_, _ = w.Write(broad)
			return
		}
		releaseHits++
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

	results, err := p.Search(context.Background(), "broad")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if releaseHits != 6 {
		t.Errorf("release fetches = %d, want the cap 6", releaseHits)
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
		case "/api/v1/app/search/releases":
			_, _ = w.Write(broad)
		case "/api/v1/anime/torrents/release/9789":
			_, _ = w.Write(fixture(t, "anilibria-torrent_release.json"))
		case "/api/v1/anime/torrents/release/5555":
			// The geo-hidden release: hidden content answers 404.
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/anime/torrents/release/10277":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

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
	p := luaProvider(t, "anilibria-torrent", srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed JSON must fail loud")
	}
	// The engine's non-marker raise path wraps with the provider/op
	// context (fmt %w, not a *ProviderError — the PR116 taxonomy); the
	// anilibria sibling pins the same containment shape. The typed
	// *ProviderError contract holds on the marker paths (the 403 pin
	// below).
	if !strings.Contains(err.Error(), "provider \"anilibria-torrent\" search:") {
		t.Errorf("error = %v, want the provider/op context", err)
	}
	if !strings.Contains(err.Error(), "invalid json") {
		t.Errorf("error = %v, want the invalid-json decode wall", err)
	}
}

func TestAnilibriaTorrentSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

	_, err := p.Search(context.Background(), "test")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anilibria-torrent" {
		t.Errorf("error = %v, want a provider-tagged error", err)
	}
}

// TestAnilibriaTorrentRosterCapabilityAndIdentity pins the hybrid
// roster shape through the real factory assembly: the bundled script
// serves search and identity, the Go TorrentBase keeps the torrent
// capability (IsTorrent/SetEngine/ingest legs — the engine consumes
// the surfaced magnets unchanged). The base_url divergence from the
// compiled provider is the anilibria-precedent one: the script pins
// the SITE root (the compiled provider reported the API root).
func TestAnilibriaTorrentRosterCapabilityAndIdentity(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var p contracts.Provider
	for _, item := range bare {
		if item.ID() == "anilibria-torrent" {
			p = item
			break
		}
	}
	if p == nil {
		t.Fatal("anilibria-torrent must stay in the roster (the bundled script pins its slot)")
	}
	tp, ok := p.(contracts.TorrentProvider)
	if !ok || !tp.IsTorrent() {
		t.Fatal("the Lua-served anilibria-torrent slot must keep the torrent capability (the smoke's torrent rule rides it)")
	}
	if _, ok := p.(interface{ SetEngine(*torrent.Engine) }); !ok {
		t.Error("the roster provider must accept the registry's engine injection")
	}
	if p.Name() != "АниЛибрия (торренты)" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
	}
	if p.BaseURL() != "https://aniliberty.top" {
		t.Errorf("base URL = %q, want the site root the script pins (the anilibria precedent)", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (RU dub audio, video)", p.SourceType())
	}
	if lc, ok := p.(interface{ ContentLanguage() string }); !ok || lc.ContentLanguage() != "ru" {
		t.Errorf("content language = %v, want ru (AniLibria dubs)", lc)
	}
}

// TestAnilibriaTorrentEpisodesDelegateToTorrentBase pins the leg
// ownership: GetEpisodes rides the Go TorrentBase (ingest → metadata
// wait), never the script's stub — with no engine wired the call
// fails with the base's not-wired error, not a script answer.
func TestAnilibriaTorrentEpisodesDelegateToTorrentBase(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var p contracts.Provider
	for _, item := range bare {
		if item.ID() == "anilibria-torrent" {
			p = item
			break
		}
	}
	if p == nil {
		t.Fatal("anilibria-torrent missing from the roster")
	}

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	_, err = p.GetEpisodes(context.Background(), dead)
	if err == nil {
		t.Fatal("GetEpisodes without a wired engine must fail loud")
	}
	if !strings.Contains(err.Error(), "engine is not wired") {
		t.Errorf("error = %v, want the TorrentBase not-wired failure (the Go leg owns episodes)", err)
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
// rule: without the [torrent] subsystem the provider
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
