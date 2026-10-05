package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// testClient builds a netclient against a test config. Tests never touch
// the real network: every base URL is an httptest server.
func testClient(t *testing.T, providerID string) *netclient.Client {
	t.Helper()

	cfg := config.Default().Network
	cfg.ProxyURL = ""
	c, err := netclient.New(cfg, netclient.WithProvider(providerID))
	if err != nil {
		t.Fatalf("netclient.New(%s): %v", providerID, err)
	}
	return c
}

// fixture loads a testdata file; failure to read is a test setup error.
func fixture(t *testing.T, name string) []byte {
	t.Helper()

	// Constant fixture directory; name is test-controlled.
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // trusted testdata path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// recordedRequest captures what the test server observed.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Form   map[string][]string
}

// fixtureServer serves fixture bodies and records the last request seen
// per handler. Handlers must be fast and side-effect free.
func fixtureServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *recordedRequest) {
	t.Helper()

	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.Query = r.URL.RawQuery
		rec.Header = r.Header.Clone()
		rec.Form = r.PostForm
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// The anilibria provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anilibria/main.lua, the PR120
// Go→Lua migration): these tests pin the script through the same
// contracts.Provider surface and the same verbatim live-capture
// fixtures the compiled Go implementation was held to (aniliberty.top
// captures, 2026-09-17). The harness rewrites the script's production
// base_url literal onto the fixture server, so the /api/v1 path
// prefix rides along in every request pin below.

// TestAnilibriaSearch pins the release search against the real captured
// «дандадан» answer: two results in document order, title from
// name.main, the alias as the result URL and the numeric release id in
// meta (the torrent sibling reuses it).
func TestAnilibriaSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilibria_search.json"))
	})
	p := luaProvider(t, "anilibria", srv.URL)

	results, err := p.Search(context.Background(), "re:zero kara")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (fixture anilibria_search.json)", len(results))
	}
	first := results[0]
	if first.Title != "Дандадан" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "dandadan" {
		t.Errorf("URL = %q, want alias", first.URL)
	}
	if first.SourceID != "anilibria" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	// The script-level number converts back into a json.Number (the
	// meta bridge), so the compiled-era assertion holds verbatim.
	if id, ok := first.Meta["id"].(json.Number); !ok || id.String() != "9789" {
		t.Errorf("Meta[id] = %#v, want json.Number 9789", first.Meta["id"])
	}

	// The request shape: the v1 release search with the query
	// URL-encoded (the Go port's task ruling, kept by the script).
	if rec.Path != "/api/v1/app/search/releases" {
		t.Errorf("request path = %q", rec.Path)
	}
	if want := "query=" + url.QueryEscape("re:zero kara"); rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}
}

// TestAnilibriaSearchProvider403 pins the 403 mapping: the netclient
// sentinel surfaces through the Lua transport layer unchanged.
func TestAnilibriaSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := luaProvider(t, "anilibria", srv.URL)

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anilibria" {
		t.Errorf("error = %v, want ProviderError from anilibria", err)
	}
}

// TestAnilibriaSearchMalformedJSONIsTypedError pins the decode wall: an
// HTML error page must fail loudly (the get_json bridge raises; the
// adapter types it), never answer an empty success.
func TestAnilibriaSearchMalformedJSONIsTypedError(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>not json</html>")
	})
	p := luaProvider(t, "anilibria", srv.URL)

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("malformed JSON must fail")
	}
	if !strings.Contains(err.Error(), "invalid json") {
		t.Errorf("error = %v, want the invalid-json context message", err)
	}
}

// TestAnilibriaGetEpisodes pins the release-detail parse: the
// /api/v1/anime/releases/{alias} request, episodes keyed by UUID
// ordinals, and the per-episode quality payload with the empty-tier
// drop (the per-requester tiering: hls_1080 arrives null/blank
// without auth or the right exit).
func TestAnilibriaGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilibria_release.json"))
	})
	p := luaProvider(t, "anilibria", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "re-zero-kara-hajimeru-isekai-seikatsu")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if want := "/api/v1/anime/releases/re-zero-kara-hajimeru-isekai-seikatsu"; rec.Path != want {
		t.Errorf("request path = %q, want %q", rec.Path, want)
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" {
		t.Errorf("episode 1 Num = %q", first.Num)
	}

	// The fresh-sandbox state carrier: raw_id IS the quality payload
	// (the compiled Go implementation stashed the episode UUID there
	// and rode RawEmbeds into ResolveStream; the Lua contract passes
	// streams(raw_id, dub) strings only, so the links JSON moves into
	// raw_id — the anitokyo {n,u} precedent).
	var links map[string]string
	if err := json.Unmarshal([]byte(first.RawID), &links); err != nil {
		t.Fatalf("decode raw_id payload: %v", err)
	}
	if len(links) != 3 {
		t.Errorf("episode 1 links = %v, want 3", links)
	}
	if links["1080"] != "https://cache.libria.fun/videos/media/ts/9789/1/1080/572da4181b9e639b2728b5e34ec484b9.m3u8?countryIso=DE&isAuthorized=0&isWithVideoAds=1&isWithVideoAdsAlways=1" {
		t.Errorf("links[1080] = %q", links["1080"])
	}

	// The dub catalog still rides raw_embeds under the hls_json:
	// convention the API surface (and the torrent sibling's dub
	// listing) documents.
	raw := first.RawEmbeds["AniLibria"]
	if len(raw) != 1 || !strings.HasPrefix(raw[0], "hls_json:") {
		t.Fatalf("RawEmbeds[AniLibria] = %#v, want one hls_json payload", raw)
	}
	var embedLinks map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw[0], "hls_json:")), &embedLinks); err != nil {
		t.Fatalf("decode hls_json payload: %v", err)
	}
	if len(embedLinks) != 3 {
		t.Errorf("episode 1 embed links = %v, want 3", embedLinks)
	}

	// Episode 2 carries only 720 (its hls_1080/hls_480 are blanked in
	// the fixture): the empty-value drop must hold on BOTH carriers.
	raw2 := episodes[1].RawEmbeds["AniLibria"][0]
	var links2 map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw2, "hls_json:")), &links2); err != nil {
		t.Fatalf("decode hls_json payload: %v", err)
	}
	if _, has := links2["1080"]; has {
		t.Errorf("episode 2 links = %v, empty hls_1080 must be dropped", links2)
	}
	if _, has := links2["720"]; !has {
		t.Errorf("episode 2 links = %v, want 720 present", links2)
	}
	if _, has := links2["480"]; has {
		t.Errorf("episode 2 links = %v, empty hls_480 must be dropped", links2)
	}
	var rawIDLinks map[string]string
	if err := json.Unmarshal([]byte(episodes[1].RawID), &rawIDLinks); err != nil {
		t.Fatalf("decode episode 2 raw_id payload: %v", err)
	}
	if len(rawIDLinks) != 1 || rawIDLinks["720"] == "" {
		t.Errorf("episode 2 raw_id links = %v, want only 720", rawIDLinks)
	}
}

// TestAnilibriaResolveStream pins the resolve branch: the raw_id
// payload decodes into per-quality sources, protocol-relative and
// bare-relative URLs gain the https: prefix (the Python string
// concatenation, quirk included), and every source carries the site
// Referer the CDN gates playback on.
func TestAnilibriaResolveStream(t *testing.T) {
	t.Parallel()

	// ResolveStream is offline: the raw_id payload is self-contained.
	p := luaProviderAtProduction(t, "anilibria")
	episode := contracts.Episode{
		Num:   "1",
		RawID: `{"1080":"//static-libria.top/v/1/1080.m3u8","480":"/v/1/480.m3u8"}`,
		RawEmbeds: map[string][]string{
			"AniLibria": {`hls_json:{"1080":"//static-libria.top/v/1/1080.m3u8","480":"/v/1/480.m3u8"}`},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AniLibria")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "AniLibria" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("Links = %v, want 2", stream.Links)
	}

	hd, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links missing 1080: %v", stream.Links)
	}
	if hd.URL != "https://static-libria.top/v/1/1080.m3u8" {
		t.Errorf("1080 URL = %q", hd.URL)
	}
	if hd.Quality != "1080" {
		t.Errorf("1080 Quality = %q", hd.Quality)
	}
	if hd.Headers["Referer"] != "https://aniliberty.top" {
		t.Errorf("1080 Referer = %q, want the aniliberty host", hd.Headers["Referer"])
	}

	sd, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links missing 480: %v", stream.Links)
	}
	if sd.URL != "https:/v/1/480.m3u8" {
		t.Errorf("480 URL = %q, want python-style https:+path concatenation", sd.URL)
	}
}

// TestAnilibriaResolveStreamUnknownDubIsEmpty pins the unknown-dub
// contract: no payload routes under a foreign dub id, the stream comes
// back empty (never an error, never another dub's links).
func TestAnilibriaResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anilibria")
	episode := contracts.Episode{
		RawID:     `{"1080":"//x/1.m3u8"}`,
		RawEmbeds: map[string][]string{"AniLibria": {`hls_json:{"1080":"//x/1.m3u8"}`}},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for unknown dub", stream.Links)
	}
	if stream.DubName != "NoSuchDub" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestAnilibriaProviderMeta pins the identity block: the SITE root as
// BaseURL (the compiled Go provider reported the API root; the Lua
// contract's single base_url literal is the site host the harness
// rewrites and the Referer derives from), RU content language,
// SourceTypeBoth.
func TestAnilibriaProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anilibria")
	if p.ID() != "anilibria" || p.Name() != "AniLibria" {
		t.Errorf("ID/Name = %q/%q", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://aniliberty.top" {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok || lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %v, want ru", lc)
	}
}

// TestAnilibriaNamePreferenceDefault pins the search routing: the RU
// catalog stays in the default (RU) name-preference group, Go parity.
func TestAnilibriaNamePreferenceDefault(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anilibria")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefDefault {
		t.Errorf("NamePreference = %v, want NamePrefDefault (the RU group)", got)
	}
}

// TestAnilibriaSmokeQueryUndeclared pins the smoke routing: the
// provider declares no probe of its own, so the parity smoke falls
// back to the shared RU query (Go parity — the compiled provider
// implemented no SmokeQueryProvider either).
func TestAnilibriaSmokeQueryUndeclared(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anilibria")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the content_lang adapter must keep the capability surface assertions-stable")
	}
	if got := sq.SmokeQuery(); got != "" {
		t.Errorf("SmokeQuery = %q, want empty (the shared RU probe applies)", got)
	}
}

// TestAnilibriaSearchTimeout pins the dead-endpoint path: the netclient
// retry ladder exhausts and the failure maps onto ErrProviderTimeout
// through the Lua transport markers, without any real network egress.
func TestAnilibriaSearchTimeout(t *testing.T) {
	t.Parallel()

	// A listener whose port is closed: connections are refused.
	dead := newDeadListener(t)

	cfg := config.Default().Network
	cfg.RequestTimeout = 60 * time.Millisecond
	p := luaProviderWithNet(t, "anilibria", "http://"+dead.Addr().String(), cfg)

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("error = %v, want ErrProviderTimeout", err)
	}
}
