package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/crypto"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// anitaku.io is NOT a Python-tree port: the provider was written
// against the live rebranded site (Kohi-den extensions-source issue
// #410) and characterized 2026-09-18: a WordPress DramaStream-theme
// catalog whose search is the POST ts_ac_do_search admin-ajax, whose
// series pages render the .eplister grid newest-first, and whose
// episode pages store every mirror as a base64-encoded iframe HTML in
// a select.mirror option value (the theme's loadMi does atob(value)
// into #pembed).
//
// PR128: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/gogoanime/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Two deviations
// the Lua contract forces, both live-verified 2026-10-06:
//   - the search POST rides the SDK http.post, which carries no custom
//     headers — the Referer the compiled provider sent (a PR5 ruling)
//     is gone; the site answers the ajax identically without it, and
//     the page GETs keep the Referer via http.get opts;
//   - the mirror hydration runs EAGERLY per episode (one
//     bounded-parallel get_batch leg per episode page — the
//     yummy/animedia/anikoto precedent, the Lua contract has no
//     DubsHydrator), while the fresh-sandbox streams() re-fetches the
//     episode page from raw_id and re-parses the mirrors.

// requestLog is a recording test server serving exact-path bodies and
// capturing every request in order (fixtureServer only keeps the last
// one; the eager hydration fans several episode-page legs per listing,
// so the single-slot recorder would race). Exact-path overrides win
// over the built-in prefix routes (net/http ServeMux precedence); a
// default whose exact path is overridden stays unregistered (double
// registration panics).
type requestLog struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []recordedRequest
}

func newRequestLog(t *testing.T, pages map[string]string) *requestLog {
	t.Helper()

	handlers := make(map[string]http.HandlerFunc, len(pages))
	for path, body := range pages {
		handlers[path] = serveBody(body)
	}
	return newRequestLogHandlers(t, handlers)
}

// newRequestLogHandlers is newRequestLog with raw handler overrides
// (the 403 pin needs a status write, not a body; the dead-embed pin
// needs the server URL inside the served body, computed at request
// time).
func newRequestLogHandlers(t *testing.T, handlers map[string]http.HandlerFunc) *requestLog {
	t.Helper()

	l := &requestLog{}
	mux := http.NewServeMux()

	// The search ajax answers the fixture verbatim (the production
	// post_links stay — the pins assert them). Every episode-page leg
	// (hydration batch or fresh-sandbox resolve) answers the mirror
	// fixture.
	defaults := map[string]http.HandlerFunc{
		"/wp-admin/admin-ajax.php": l.record(string(fixture(t, "gogoanime_search.json"))),
	}
	// The series pages serve with the production anitaku.io hrefs
	// rewritten onto the loopback: the eager hydration fetches them
	// straight off the page (the anikoto serve-time origin rewrite
	// precedent). The rewrite needs the server URL, so the handler
	// reads it at request time; the search JSON and episode fixtures
	// keep their production URLs — the pins assert them.
	defaults["/series/"] = func(w http.ResponseWriter, r *http.Request) {
		l.record(strings.ReplaceAll(string(fixture(t, "gogoanime_series.html")),
			"https://anitaku.io", l.srv.URL))(w, r)
	}

	taken := make(map[string]bool, len(handlers))
	for path, h := range handlers {
		mux.HandleFunc(path, h)
		taken[path] = true
	}
	for path, h := range defaults {
		if taken[path] {
			continue
		}
		mux.HandleFunc(path, h)
	}
	// The episode pages have no stable common prefix pattern (ServeMux
	// only treats slash-terminated paths as subtrees), so a catch-all
	// routes them: anything starting with /one-piece-episode- gets the
	// mirror fixture, everything else is an honest 404. More-specific
	// registrations (the exact overrides above, the /series/ subtree)
	// still win — net/http ServeMux precedence.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/one-piece-episode-") {
			l.record(string(fixture(t, "gogoanime_episode.html")))(w, r)
			return
		}
		http.NotFound(w, r)
	})

	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

// record wraps a body-serving handler with request capture.
func (l *requestLog) record(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.capture(r)
		_, _ = w.Write([]byte(body))
	}
}

// serveBody is the standalone capture-free serve helper (override
// routes whose requests need no assertions).
func serveBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}
}

// serveBodyFn serves a body computed at request time (the handler
// needs the server URL, which does not exist at map-build time).
func serveBodyFn(body func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body()))
	}
}

func (l *requestLog) capture(r *http.Request) {
	if err := r.ParseForm(); err != nil {
		return
	}
	l.mu.Lock()
	l.requests = append(l.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Form:   r.PostForm,
	})
	l.mu.Unlock()
}

// hits counts the recorded requests whose path carries prefix.
func (l *requestLog) hits(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, req := range l.requests {
		if strings.HasPrefix(req.Path, prefix) {
			n++
		}
	}
	return n
}

// searchRequest projects the recorded admin-ajax call (nil when the
// ajax was never fetched).
func (l *requestLog) searchRequest() *recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.requests {
		if l.requests[i].Path == "/wp-admin/admin-ajax.php" {
			return &l.requests[i]
		}
	}
	return nil
}

// gogoMirrorOption builds one Anitaku mirror <option>: the site stores
// each server as base64-encoded iframe HTML in the value attribute
// (loadMi does atob(value) into #pembed).
func gogoMirrorOption(t *testing.T, index int, src string) string {
	t.Helper()

	iframe := `<iframe src="` + src + `" frameborder="0" allowfullscreen></iframe>`
	enc := base64.StdEncoding.EncodeToString([]byte(iframe))
	return `<option value="` + enc + `" data-index="` + fmt.Sprint(index) + `"></option>`
}

// gogoSinkWorld serves the classic gogo embed family offline: the
// embedplus page (three AES key markers + encrypted data-value) and the
// encrypt-ajax.php endpoint, crypto-modeled exactly like
// extractors/gogoplay_test.go — the chain the mirrors walk when a
// matching host is live.
type gogoSinkWorld struct {
	srv *httptest.Server
}

func newGogoSinkWorld(t *testing.T, m3u8 string) *gogoSinkWorld {
	t.Helper()

	const (
		keyEnc = "3947103857291746"
		keyIV  = "1029384756102938"
		keyDec = "5647382910473829"
	)
	w := &gogoSinkWorld{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embedplus":
			enc, err := crypto.AESEncrypt("id=content123&alias=naruto", []byte(keyEnc), []byte(keyIV))
			if err != nil {
				t.Errorf("encrypt data-value: %v", err)
				rw.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(rw, `<div class="container-%s" id="videocontent-%s" data-value=%q></div><script>var videocontent-%s;</script>`,
				keyEnc, keyIV, enc, keyDec)
		case "/encrypt-ajax.php":
			sources, _ := json.Marshal(map[string]any{
				"source": []map[string]string{
					{"file": m3u8, "label": "720 P"},
				},
			})
			enc, err := crypto.AESEncrypt(string(sources), []byte(keyDec), []byte(keyIV))
			if err != nil {
				t.Errorf("encrypt ajax payload: %v", err)
				rw.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(rw, `{"data":%q}`, enc)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// formValue reads one POST form field off a recorded request.
func formValue(form map[string][]string, key string) string {
	if vals, ok := form[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// ggProvider loads the bundled gogoanime script against the test
// server (the Lua harness rewrites the production base literal).
func ggProvider(t *testing.T, srv *requestLog) contracts.Provider {
	t.Helper()
	return luaProvider(t, "gogoanime", srv.srv.URL)
}

func TestGogoAnimeSearch(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, nil)
	p := ggProvider(t, srv)

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// [LIVE-VERIFIED 2026-09-18] anitaku.io (the gogoanime rebrand,
	// Kohi-den extensions-source issue #410) dropped the WordPress /?s=
	// search: it 301s to /browse/. The live search is the dramastream
	// ajax endpoint — POST form action/ts_ac_query; the GET form ignores
	// the query and returns recent posts. The SDK http.post cannot
	// carry custom headers, so the PR5 Referer is gone — live-verified
	// 2026-10-06 the ajax answers identically without it.
	rec := srv.searchRequest()
	if rec == nil {
		t.Fatal("the search ajax was never fetched")
	}
	if rec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/wp-admin/admin-ajax.php" {
		t.Errorf("request path = %q, want /wp-admin/admin-ajax.php", rec.Path)
	}
	if got := formValue(rec.Form, "action"); got != "ts_ac_do_search" {
		t.Errorf("form action = %q, want ts_ac_do_search", got)
	}
	if got := formValue(rec.Form, "ts_ac_query"); got != "one piece" {
		t.Errorf("form ts_ac_query = %q, want %q", got, "one piece")
	}
	if got := rec.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form urlencoded media type", got)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (series[0].all entries)", len(results))
	}
	if results[0].Title != "One Piece: Heroines" {
		t.Errorf("Title = %q, want post_title", results[0].Title)
	}
	if results[0].URL != "https://anitaku.io/series/one-piece-heroines/" {
		t.Errorf("URL = %q, want the absolute post_link", results[0].URL)
	}
	if results[0].SourceID != "gogoanime" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if !strings.Contains(results[0].Poster, "wp-content/uploads/") {
		t.Errorf("Poster = %q, want post_image", results[0].Poster)
	}
}

func TestGogoAnimeSearchEmpty(t *testing.T) {
	t.Parallel()

	// [LIVE-VERIFIED 2026-09-18] the zero-hit shape: all:[] plus the
	// render template, verbatim apart from the template whitespace.
	srv := newRequestLog(t, map[string]string{
		"/wp-admin/admin-ajax.php": `{"series":[{"all":[],"template":"<a href=\"{post_link}\">{post_image_html}</a>","title":"Search","class_name":"live-search_item"}]}`,
	})
	p := ggProvider(t, srv)

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 for an empty all[]", len(results))
	}
}

func TestGogoAnimeSearchMalformedJSON(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, map[string]string{
		"/wp-admin/admin-ajax.php": "<html>cloudflare challenge</html>",
	})
	p := ggProvider(t, srv)

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("error = nil, want a typed decode failure")
	}
	if !strings.Contains(err.Error(), "gogoanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

func TestGogoAnimeGetEpisodes(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, nil)
	p := ggProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), srv.srv.URL+"/series/one-piece/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3 (.eplister li)", len(episodes))
	}

	// [LIVE-VERIFIED 2026-09-18] the eplister renders newest-first
	// (1178, 1177, 1176 for One Piece); the provider reverses to
	// ascending like the legacy ajax list did.
	wantNums := []string{"1176", "1177", "1178"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	if episodes[2].Title != "One Piece Episode 1178 English Subbed" {
		t.Errorf("Title = %q, want the .epl-title text", episodes[2].Title)
	}
	// The RawIDs carry the rewritten hrefs the page served (the fixture
	// rewrite keeps the hydration legs on the loopback).
	if episodes[2].RawID != srv.srv.URL+"/one-piece-episode-1178-english-subbed/" {
		t.Errorf("RawID = %q, want the absolute episode href", episodes[2].RawID)
	}

	// The eager hydration fetched every episode page exactly once.
	if got := srv.hits("/one-piece-episode-"); got != 3 {
		t.Errorf("episode-page legs = %d, want 3 (one bounded batch per listing)", got)
	}
}

// TestGogoAnimeEpisodeDubs pins the eager mirror hydration: the Lua
// contract has no DubsHydrator, so every episode surfaces its mirrors
// straight from the listing (the yummy/animedia/anikoto precedent).
// The fixture mirrors keep their production URLs — the batch legs fetch
// the pages, never the embeds.
func TestGogoAnimeEpisodeDubs(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, nil)
	p := ggProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), srv.srv.URL+"/series/one-piece/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	embeds := episodes[0].RawEmbeds
	if len(embeds) != 1 {
		t.Fatalf("RawEmbeds = %v, want 1 server (both options carry blank labels)", embeds)
	}
	urls, ok := embeds["Unknown"]
	if !ok || len(urls) != 2 {
		t.Fatalf("Unknown = %v, want both mirror iframes (placeholder skipped)", urls)
	}
	// [LIVE-VERIFIED 2026-09-18] option 1 is the real One Piece 1178
	// capture, option 2 the real Dandadan S2 ep12 capture.
	if !strings.Contains(urls[0], "blogger.com/video.g?token=") {
		t.Errorf("mirror[0] = %q, want the blogger embed", urls[0])
	}
	if !strings.Contains(urls[1], "megacloud.bloggy.click/stream/") {
		t.Errorf("mirror[1] = %q, want the megacloud embed", urls[1])
	}
}

func TestGogoAnimeGetEpisodesRelativeURL(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, nil)
	p := ggProvider(t, srv)

	if _, err := p.GetEpisodes(context.Background(), "/series/one-piece/"); err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	srv.mu.Lock()
	found := false
	for _, req := range srv.requests {
		if req.Path == "/series/one-piece/" {
			found = true
		}
	}
	srv.mu.Unlock()
	if !found {
		t.Fatal("the base-prefixed series page was never fetched")
	}
}

func TestGogoAnimeGetEpisodesNoEpisodes(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, map[string]string{
		"/series/none": "<html><body>no episode list</body></html>",
	})
	p := ggProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), srv.srv.URL+"/series/none")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without .eplister", len(episodes))
	}
}

func TestGogoAnimeGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv := newRequestLogHandlers(t, map[string]http.HandlerFunc{
		"/series/x": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		},
	})
	p := ggProvider(t, srv)

	_, err := p.GetEpisodes(context.Background(), srv.srv.URL+"/series/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

// TestGogoAnimeResolveStreamRoundTrip walks one mirror through the
// extractor chain: the episode page carries the classic gogo embed host
// (streaming.php family) as a base64 mirror option; the fresh-sandbox
// resolve fetches the page from raw_id itself (no prior listing — the
// Go lazy-fetch semantics survive as the re-fetch) and resolves the
// modeled encrypt-ajax handshake to a 720 m3u8 source.
func TestGogoAnimeResolveStreamRoundTrip(t *testing.T) {
	t.Parallel()

	sink := newGogoSinkWorld(t, "https://h.example/hls/master.m3u8")
	srv := newRequestLogHandlers(t, map[string]http.HandlerFunc{
		"/naruto-episode-1-english-subbed/": serveBodyFn(func() string {
			mirror := gogoMirrorOption(t, 1, sink.srv.URL+"/embedplus?id=content123")
			return `<html><body><select class="mirror" name="mirror" onchange="loadMi(this);">` +
				`<option value="">Select Video Server</option>` + mirror + `</select></body></html>`
		}),
	})
	p := ggProvider(t, srv)
	episode := contracts.Episode{
		Num:   "1",
		RawID: srv.srv.URL + "/naruto-episode-1-english-subbed/",
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Unknown")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Unknown" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry", stream.Links)
	}
	if src.URL != "https://h.example/hls/master.m3u8" {
		t.Errorf("URL = %q, want the modeled hls source", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8", src.Type)
	}
}

// TestGogoAnimeResolveStreamFailsLoudWithoutSources pins the no-silent-
// failure rule: when the only mirror points at a dead embed, the chain
// error surfaces provider-tagged (never an empty success).
func TestGogoAnimeResolveStreamFailsLoudWithoutSources(t *testing.T) {
	t.Parallel()

	srv := newRequestLogHandlers(t, map[string]http.HandlerFunc{
		"/embedplus": serveBody("<html><body>player maintenance</body></html>"),
		// The dead embed rides the same host serving the page (r.Host
		// — the mirror URL is built at request time, which also breaks
		// the handler-needs-server-URL cycle).
		"/episode-1/": func(w http.ResponseWriter, r *http.Request) {
			mirror := gogoMirrorOption(t, 1, "http://"+r.Host+"/embedplus?id=content123")
			//nolint:gosec // G705: a loopback fixture server echoing its own host into fixture HTML for a Lua parser — no browser, no XSS surface
			_, _ = w.Write([]byte(`<html><body><select class="mirror" name="mirror" onchange="loadMi(this);">` +
				`<option value="">Select Video Server</option>` + mirror + `</select></body></html>`))
		},
	})

	p := ggProvider(t, srv)
	episode := contracts.Episode{
		RawID: srv.srv.URL + "/episode-1/",
	}

	_, err := p.ResolveStream(context.Background(), episode, "Unknown")
	if err == nil {
		t.Fatal("error = nil, want the extraction failure of the dead embed")
	}
	if !strings.Contains(err.Error(), "gogoanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

// TestGogoAnimeResolveStreamUnknownDub pins the dub-miss wall: a dub
// key the episode page carries no mirrors for is typed invalid input
// (the compiled provider's plain WrapProvider error, surfaced through
// the anicli.fail invalid_input marker).
func TestGogoAnimeResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, nil)
	p := ggProvider(t, srv)

	episode := contracts.Episode{
		Num:   "1178",
		RawID: srv.srv.URL + "/one-piece-episode-1178-english-subbed/",
	}
	_, err := p.ResolveStream(context.Background(), episode, "Дубляж")
	if err == nil || !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestGogoAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "gogoanime")
	if p.ID() != "gogoanime" || p.Name() != "GogoAnime" || p.BaseURL() != "https://anitaku.io" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// JA audio + EN subs + video (PR23: wanted-language audio → BOTH).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	// ContentLanguage is a duck-typed capability surface (the adapted
	// wrapper carries it when the script declares content_lang).
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the gogoanime lua provider lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ja" {
		t.Errorf("ContentLanguage = %q, want ja", got)
	}
}

// The live smoke query: the Anitaku live-search index no longer surfaces
// the shared probes (2026-09-18: {"all":[]} for черная лагуна / black
// lagoon, eight results for "one piece") — the provider declares its own
// stable broad hit.
func TestGogoAnimeSmokeQuery(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "gogoanime")
	if sq, ok := p.(contracts.SmokeQueryProvider); !ok {
		t.Fatalf("the gogoanime lua provider lost the SmokeQueryProvider surface (%T)", p)
	} else if sq.SmokeQuery() != "one piece" {
		t.Fatalf("SmokeQuery = %q, want one piece", sq.SmokeQuery())
	}
}
