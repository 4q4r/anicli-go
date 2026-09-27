package lua

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// newProviderEngine builds an engine with a small invocation budget
// aimed at an httptest backend.
func newProviderEngine(t *testing.T, handler http.Handler) (*Engine, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := DefaultConfig()
	cfg.Timeout = 2 * time.Second
	// Scripts carry their production base_url (https://test.example);
	// the transport redirects that host onto the test server so the
	// absolute URLs the scripts fetch land in the handler.
	cfg.Transport = &rewriteTransport{from: "test.example", to: srv.Listener.Addr().String()}
	return NewEngine(cfg, mustLogger(t)), srv
}

// rewriteTransport maps one host onto another (scheme downgraded to
// http) — the minimal seam making production URLs testable.
type rewriteTransport struct {
	from string
	to   string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == t.from {
		req.URL.Scheme = "http"
		req.URL.Host = t.to
	}
	return http.DefaultTransport.RoundTrip(req)
}

const validScript = `
return {
	id = "testsite",
	name = "Test Site",
	base_url = "https://test.example",
	capabilities = "both",
	search = function(query)
		local r = anicli.http.get(BASE .. "/search?q=" .. anicli.http.query_escape(query))
		local data = anicli.json.decode(r.body)
		local out = {}
		for i, item in ipairs(data.items) do
			out[i] = { title = item.name, url = item.href, poster = item.img }
		end
		return out
	end,
	episodes = function(anime_url)
		local r = anicli.http.get(anime_url)
		local data = anicli.json.decode(r.body)
		local out = {}
		for i, ep in ipairs(data.episodes) do
			out[i] = { num = ep.number, title = ep.name, raw_id = ep.id,
				raw_embeds = { [data.dub] = ep.embeds } }
		end
		return out
	end,
	streams = function(episode_url, dub)
		local r = anicli.http.get(episode_url)
		local data = anicli.json.decode(r.body)
		return {
			dub_name = dub,
			links = {
				["1080"] = { url = data.hls, quality = "1080", type = "m3u8",
					headers = { Referer = "https://test.example/" } },
				["720"] = { url = data.fallback, quality = "720", type = "mp4",
					extra_mpv_opts = { "--referrer=https://test.example/" } },
			},
		}
	end,
}
`

// harness scripts are formatted with BASE (the httptest server URL).
func scriptWithBase(t *testing.T, srv *httptest.Server, body string) string {
	t.Helper()
	return "BASE = " + fmt.Sprintf("%q", srv.URL) + "\n" + body
}

func newTestProvider(t *testing.T, handler http.Handler) (*Provider, *httptest.Server) {
	t.Helper()
	e, srv := newProviderEngine(t, handler)
	p, err := e.LoadProvider("testsite", scriptWithBase(t, srv, validScript))
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}
	return p, srv
}

func TestProviderMetadata(t *testing.T) {
	t.Parallel()

	p, _ := newTestProvider(t, http.NotFoundHandler())

	if p.ID() != "testsite" {
		t.Fatalf("ID = %q", p.ID())
	}
	if p.Name() != "Test Site" {
		t.Fatalf("Name = %q", p.Name())
	}
	if p.BaseURL() != "https://test.example" {
		t.Fatalf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Fatalf("SourceType = %q", p.SourceType())
	}

	// Compile-time contract proof.
	var _ contracts.Provider = p
}

func TestProviderSearch(t *testing.T) {
	t.Parallel()

	var gotQuery string
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items": [
			{"name": "Cowboy Bebop", "href": "/a/bebop", "img": "https://cdn/b.jpg"},
			{"name": "Trigun", "href": "/a/trigun"}
		]}`)
	}))

	results, err := p.Search(context.Background(), "beb op")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "beb op" {
		t.Fatalf("server saw query %q", gotQuery)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	first := results[0]
	if first.Title != "Cowboy Bebop" || first.URL != "/a/bebop" || first.Poster != "https://cdn/b.jpg" {
		t.Fatalf("results[0] = %+v", first)
	}
	if results[1].Title != "Trigun" || results[1].Poster != "" {
		t.Fatalf("results[1] = %+v", results[1])
	}
	if results[0].SourceID != "testsite" {
		t.Fatalf("SourceID = %q, want testsite", results[0].SourceID)
	}
}

func TestProviderGetEpisodes(t *testing.T) {
	t.Parallel()

	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"dub": "AniLibria.TV", "episodes": [
			{"number": "1", "name": "Red Eye", "id": "ep-1", "embeds": ["https://e/1"]},
			{"number": 2, "name": "", "id": "ep-2", "embeds": ["https://e/2a", "https://e/2b"]}
		]}`)
	}))

	eps, err := p.GetEpisodes(context.Background(), "https://test.example/a/bebop")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("len(eps) = %d, want 2", len(eps))
	}
	if eps[0].Num != "1" || eps[0].Title != "Red Eye" || eps[0].RawID != "ep-1" {
		t.Fatalf("eps[0] = %+v", eps[0])
	}
	if got := eps[0].RawEmbeds["AniLibria.TV"]; len(got) != 1 || got[0] != "https://e/1" {
		t.Fatalf("eps[0].RawEmbeds = %v", eps[0].RawEmbeds)
	}
	// A JSON number episode number coerces Lua-style (2 -> "2").
	if eps[1].Num != "2" {
		t.Fatalf("eps[1].Num = %q, want coerced \"2\"", eps[1].Num)
	}
	if got := eps[1].RawEmbeds["AniLibria.TV"]; len(got) != 2 {
		t.Fatalf("eps[1].RawEmbeds = %v", eps[1].RawEmbeds)
	}
}

func TestProviderResolveStream(t *testing.T) {
	t.Parallel()

	var gotPath string
	p, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"hls": "https://cdn/master.m3u8", "fallback": "https://cdn/720.mp4"}`)
	}))

	stream, err := p.ResolveStream(context.Background(),
		contracts.Episode{Num: "1", RawID: "https://test.example/play/ep-1"}, "Дубляж")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if gotPath != "/play/ep-1" {
		t.Fatalf("the script fetched %q, want /play/ep-1 (episode.RawID)", gotPath)
	}
	if stream.DubName != "Дубляж" {
		t.Fatalf("DubName = %q", stream.DubName)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("len(links) = %d, want 2", len(stream.Links))
	}
	hls := stream.Links["1080"]
	if hls.URL != "https://cdn/master.m3u8" || hls.Quality != "1080" || hls.Type != "m3u8" {
		t.Fatalf("links[1080] = %+v", hls)
	}
	if hls.Headers["Referer"] != "https://test.example/" {
		t.Fatalf("links[1080].Headers = %v", hls.Headers)
	}
	fallback := stream.Links["720"]
	if fallback.URL != "https://cdn/720.mp4" || len(fallback.ExtraMPVOpts) != 1 {
		t.Fatalf("links[720] = %+v", fallback)
	}
}

// TestProviderInvalidResultType: a type mismatch in the results names
// the provider, function, index and field.
func TestProviderInvalidResultType(t *testing.T) {
	t.Parallel()

	e, srv := newProviderEngine(t, http.NotFoundHandler())
	src := scriptWithBase(t, srv, `
return {
	id = "brokend",
	search = function(query)
		return { { title = "ok", url = "/u1" }, { title = 42, url = "/u2" } }
	end,
	episodes = function() return {} end,
	streams = function() return { dub_name = "d", links = {} } end,
}
`)
	p, err := e.LoadProvider("brokend", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	_, err = p.Search(context.Background(), "x")
	if err == nil {
		t.Fatal("a number title must fail validation")
	}
	for _, want := range []string{`provider "brokend"`, "search", "results[2].title", "expected string, got number"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// TestProviderMissingFunction: an incomplete provider table is a typed
// validation error naming the function.
func TestProviderMissingFunction(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
return {
	id = "halves",
	search = function(query) return {} end,
}
`
	_, err := e.LoadProvider("halves", src)
	if err == nil {
		t.Fatal("a provider without episodes/streams must fail to load")
	}
	if !errors.Is(err, ErrInvalidScript) {
		t.Fatalf("missing function error not typed: %v", err)
	}
	if !strings.Contains(err.Error(), "episodes") {
		t.Fatalf("error %q must name the missing function", err)
	}
}

// TestProviderIDMismatch: the script's id must equal the directory name.
func TestProviderIDMismatch(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `return { id = "other", search = function() end, episodes = function() end, streams = function() end }`
	_, err := e.LoadProvider("dirname", src)
	if err == nil {
		t.Fatal("id mismatch must fail")
	}
	if !errors.Is(err, ErrInvalidScript) {
		t.Fatalf("id mismatch error not typed: %v", err)
	}
	if !strings.Contains(err.Error(), "dirname") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("error %q must name both ids", err)
	}
}

// TestProviderTimeoutTyped: a script looping forever yields a typed
// provider timeout, not a raw VM error.
func TestProviderTimeoutTyped(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	e.cfg.Timeout = 100 * time.Millisecond
	src := `
return {
	id = "slowpoke",
	search = function(query) while true do end end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`
	p, err := e.LoadProvider("slowpoke", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	_, err = p.Search(context.Background(), "x")
	if err == nil {
		t.Fatal("the infinite loop must time out")
	}
	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("timeout error not typed: %v", err)
	}
}

// TestProviderCallerCancelNotTimeout: an aborted fan-out is reported as
// the context error, never misclassified as a provider timeout.
func TestProviderCallerCancelNotTimeout(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
return {
	id = "napper",
	search = function(query) while true do end end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`
	p, _ := e.LoadProvider("napper", src)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := p.Search(ctx, "x")
	if err == nil {
		t.Fatal("a canceled context must abort the search")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation misclassified: %v", err)
	}
	if errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("caller cancellation must not be typed as provider timeout: %v", err)
	}
}

// TestProviderLuaErrorWrapped: a script runtime error carries the
// provider id, the operation and the script traceback.
func TestProviderLuaErrorWrapped(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
return {
	id = "boom",
	search = function(query) error("kaboom") end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`
	p, _ := e.LoadProvider("boom", src)

	_, err := p.Search(context.Background(), "x")
	if err == nil {
		t.Fatal("the raised error must propagate")
	}
	for _, want := range []string{`provider "boom"`, "search", "kaboom", "providers/boom/main.lua"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q (traceback chunkname)", err, want)
		}
	}
}

// TestProviderScriptSyntaxError: a chunk that does not compile fails
// at load time with the chunkname in the message.
func TestProviderScriptSyntaxError(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	_, err := e.LoadProvider("syntax", `return { id = "syntax", search = function() end,`)
	if err == nil {
		t.Fatal("a syntax error must fail the load")
	}
	if !errors.Is(err, ErrInvalidScript) {
		t.Fatalf("syntax error not typed: %v", err)
	}
	if !strings.Contains(err.Error(), "providers/syntax/main.lua") {
		t.Fatalf("error %q must carry the chunkname", err)
	}
}

// TestProviderNilResult: returning nothing (nil) means zero results,
// not an error, for the listing operations.
func TestProviderNilResult(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
return {
	id = "empty",
	search = function(query) return nil end,
	episodes = function() return end,
	streams = function() return nil end,
}
`
	p, _ := e.LoadProvider("empty", src)

	results, err := p.Search(context.Background(), "x")
	if err != nil || len(results) != 0 {
		t.Fatalf("Search nil = %v, %v; want empty, nil", results, err)
	}
	eps, err := p.GetEpisodes(context.Background(), "u")
	if err != nil || len(eps) != 0 {
		t.Fatalf("GetEpisodes nil = %v, %v; want empty, nil", eps, err)
	}
	// ResolveStream returning nil is an extract failure — a stream was
	// explicitly requested.
	_, err = p.ResolveStream(context.Background(), contracts.Episode{RawID: "r"}, "d")
	if err == nil {
		t.Fatal("streams returning nil must error")
	}
}

// TestProviderDefaults: name and base_url fall back to the id / empty.
func TestProviderDefaults(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
return {
	id = "bare",
	search = function() return {} end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`
	p, err := e.LoadProvider("bare", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}
	if p.Name() != "bare" || p.BaseURL() != "" {
		t.Fatalf("defaults: name=%q baseURL=%q; want bare, empty", p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Fatalf("default SourceType = %q, want both", p.SourceType())
	}
}

// TestProviderCapabilities: the capabilities string maps onto
// contracts.SourceType; unknown values fail the load.
func TestProviderCapabilities(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())

	for capability, want := range map[string]contracts.SourceType{
		"video": contracts.SourceTypeVideo,
		"audio": contracts.SourceTypeAudio,
		"both":  contracts.SourceTypeBoth,
	} {
		src := fmt.Sprintf(`
return {
	id = "caps", capabilities = %q,
	search = function() return {} end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`, capability)
		p, err := e.LoadProvider("caps", src)
		if err != nil {
			t.Fatalf("capabilities %q: %v", capability, err)
		}
		if p.SourceType() != want {
			t.Fatalf("capabilities %q -> SourceType %q, want %q", capability, p.SourceType(), want)
		}
	}

	bad := `
return {
	id = "caps", capabilities = "hdrips",
	search = function() return {} end,
	episodes = function() return {} end,
	streams = function() return {} end,
}
`
	if _, err := e.LoadProvider("caps", bad); err == nil {
		t.Fatal("an unknown capability must fail the load")
	}
}

// TestProviderFreshStatePerCall: module-level state must not leak
// between invocations — every call re-runs the script from scratch.
func TestProviderFreshStatePerCall(t *testing.T) {
	t.Parallel()

	e, _ := newProviderEngine(t, http.NotFoundHandler())
	src := `
COUNTER = COUNTER or 0
COUNTER = COUNTER + 1
return {
	id = "counter",
	search = function(query)
		local r = {}
		r[1] = { title = "call " .. COUNTER, url = "/u" }
		return r
	end,
	episodes = function() return {} end,
	streams = function() return { dub_name = "d", links = {} } end,
}
`
	p, err := e.LoadProvider("counter", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	first, err := p.Search(context.Background(), "x")
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}
	second, err := p.Search(context.Background(), "x")
	if err != nil {
		t.Fatalf("second Search: %v", err)
	}
	if first[0].Title != "call 1" || second[0].Title != "call 1" {
		t.Fatalf("state leaked across invocations: %q then %q", first[0].Title, second[0].Title)
	}
}
