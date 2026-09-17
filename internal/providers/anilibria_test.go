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

func TestAnilibriaSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilibria_search.json"))
	})
	p := newAnilibria(srv.URL, AniLibriaHost, testClient(t, "anilibria"))

	results, err := p.Search(context.Background(), "re:zero kara")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (fixture anilibria_search.json)", len(results))
	}
	first := results[0]
	// Fixture values are verbatim live captures (aniliberty.top
	// /api/v1/app/search/releases?query=dandadan, 2026-09-17).
	if first.Title != "Дандадан" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "dandadan" {
		t.Errorf("URL = %q, want alias", first.URL)
	}
	if first.SourceID != "anilibria" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if id, ok := first.Meta["id"].(json.Number); !ok || id.String() != "9789" {
		t.Errorf("Meta[id] = %#v, want json.Number 9789", first.Meta["id"])
	}

	// The query must arrive URL-encoded (task ruling; Python used a raw
	// f-string interpolation).
	if rec.Path != "/app/search/releases" {
		t.Errorf("request path = %q", rec.Path)
	}
	if want := "query=" + url.QueryEscape("re:zero kara"); rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}
}

func TestAnilibriaSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newAnilibria(srv.URL, AniLibriaHost, testClient(t, "anilibria"))

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anilibria" {
		t.Errorf("error = %v, want ProviderError from anilibria", err)
	}
}

func TestAnilibriaSearchMalformedJSONIsTypedError(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>not json</html>")
	})
	p := newAnilibria(srv.URL, AniLibriaHost, testClient(t, "anilibria"))

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("malformed JSON must fail")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want *contracts.ProviderError", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %v, want a decode context message", err)
	}
}

func TestAnilibriaGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilibria_release.json"))
	})
	p := newAnilibria(srv.URL, AniLibriaHost, testClient(t, "anilibria"))

	episodes, err := p.GetEpisodes(context.Background(), "re-zero-kara-hajimeru-isekai-seikatsu")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if want := "/anime/releases/re-zero-kara-hajimeru-isekai-seikatsu"; rec.Path != want {
		t.Errorf("request path = %q, want %q", rec.Path, want)
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	// The new aniliberty.top API identifies episodes by UUID strings
	// (the old libria API used numeric ids — the rebase in PR37).
	first := episodes[0]
	if first.Num != "1" || first.RawID != "9d289417-5c5c-4c9a-b4c8-144a3368c100" {
		t.Errorf("episode 1 Num/RawID = %q/%q", first.Num, first.RawID)
	}
	raw := first.RawEmbeds["AniLibria"]
	if len(raw) != 1 || !strings.HasPrefix(raw[0], "hls_json:") {
		t.Fatalf("RawEmbeds[AniLibria] = %#v, want one hls_json payload", raw)
	}

	// Episode 1 carries all three qualities with full signed URLs
	// (live capture); episode 2 carries only 720 (its hls_1080/hls_480
	// are blanked in the fixture to keep the empty-value drop
	// coverage; every other value is a verbatim capture).
	var links map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw[0], "hls_json:")), &links); err != nil {
		t.Fatalf("decode hls_json payload: %v", err)
	}
	if len(links) != 3 {
		t.Errorf("episode 1 links = %v, want 3", links)
	}
	if links["1080"] != "https://cache.libria.fun/videos/media/ts/9789/1/1080/572da4181b9e639b2728b5e34ec484b9.m3u8?countryIso=DE&isAuthorized=0&isWithVideoAds=1&isWithVideoAdsAlways=1" {
		t.Errorf("links[1080] = %q", links["1080"])
	}

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
}

func TestAnilibriaResolveStream(t *testing.T) {
	t.Parallel()

	// ResolveStream is offline: it decodes the hls_json payload stashed by
	// GetEpisodes (anicli-py anilibria.py:60-78).
	p := newAnilibria(AniLibriaAPIBase, AniLibriaHost, testClient(t, "anilibria"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"AniLibria": {
				`hls_json:{"1080":"//static-libria.top/v/1/1080.m3u8","480":"/v/1/480.m3u8"}`,
			},
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
	// Protocol-relative and bare-relative URLs gain the https: prefix
	// (Python: not url.startswith("http") -> "https:" + url). The new
	// API emits full https URLs, but the legacy relative shapes stay
	// supported (the Python port handled them the same way).
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

func TestAnilibriaResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newAnilibria(AniLibriaAPIBase, AniLibriaHost, testClient(t, "anilibria"))
	episode := contracts.Episode{
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

func TestAnilibriaProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnilibria(AniLibriaAPIBase, AniLibriaHost, testClient(t, "anilibria"))
	if p.ID() != "anilibria" || p.Name() != "AniLibria" {
		t.Errorf("ID/Name = %q/%q", p.ID(), p.Name())
	}
	if p.BaseURL() != AniLibriaAPIBase {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both (Python SourceCapability.BOTH)", p.SourceType())
	}
}

func TestAnilibriaSearchTimeout(t *testing.T) {
	t.Parallel()

	// A listener whose port is closed: connections are refused, the
	// netclient retry ladder exhausts and maps the final network failure
	// onto ErrProviderTimeout without any real network egress.
	dead := newDeadListener(t)

	cfg := config.Default().Network
	cfg.ProxyURL = ""
	cfg.RequestTimeout = 60 * time.Millisecond
	c, err := netclient.New(cfg, netclient.WithProvider("anilibria"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	p := newAnilibria("http://"+dead.Addr().String(), AniLibriaHost, c)

	_, err = p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("error = %v, want ErrProviderTimeout", err)
	}
}
