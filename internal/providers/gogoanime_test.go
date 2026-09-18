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

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/crypto"
)

// requestLog is a recording test server serving exact-path bodies and
// capturing every request in order (fixtureServer only keeps the last
// one; the gogoanime resolve chain needs per-hop header assertions).
type requestLog struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []recordedRequest
}

func newRequestLog(t *testing.T, pages map[string]string) *requestLog {
	t.Helper()

	l := &requestLog{}
	mux := http.NewServeMux()
	for path, body := range pages {
		mux.HandleFunc(path, l.record(body))
	}
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

func (l *requestLog) get(path string) (recordedRequest, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, req := range l.requests {
		if req.Path == path {
			return req, true
		}
	}
	return recordedRequest{}, false
}

// record wraps a body-serving handler with request capture.
func (l *requestLog) record(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
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
		_, _ = w.Write([]byte(body))
	}
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

func TestGogoAnimeSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wp-admin/admin-ajax.php" {
			_, _ = w.Write(fixture(t, "gogoanime_search.json"))
			return
		}
		http.NotFound(w, r)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// [LIVE-VERIFIED 2026-09-18] anitaku.io (the gogoanime rebrand,
	// Kohi-den extensions-source issue #410) dropped the WordPress /?s=
	// search: it 301s to /browse/. The live search is the dramastream
	// ajax endpoint — POST form action/ts_ac_query; the GET form ignores
	// the query and returns recent posts.
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

func TestGogoAnimeSearchSendsReferer(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"series":[{"all":[]}]}`))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// PR5 task ruling kept: the embed-heavy site expects the Referer.
	if got := rec.Header.Get("Referer"); got != srv.URL {
		t.Errorf("Referer = %q, want the site root", got)
	}
}

func TestGogoAnimeSearchEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// [LIVE-VERIFIED 2026-09-18] the zero-hit shape: all:[] plus the
		// render template, verbatim apart from the template whitespace.
		_, _ = w.Write([]byte(`{"series":[{"all":[],"template":"<a href=\"{post_link}\">{post_image_html}</a>","title":"Search","class_name":"live-search_item"}]}`))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>cloudflare challenge</html>"))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/series/one-piece/" {
			_, _ = w.Write(fixture(t, "gogoanime_series.html"))
			return
		}
		http.NotFound(w, r)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/series/one-piece/")
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
	if episodes[2].RawID != "https://anitaku.io/one-piece-episode-1178-english-subbed/" {
		t.Errorf("RawID = %q, want the absolute episode href", episodes[2].RawID)
	}
	if len(episodes[0].RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (servers are fetched lazily)", episodes[0].RawEmbeds)
	}
}

func TestGogoAnimeGetEpisodesRelativeURL(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_series.html"))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	if _, err := p.GetEpisodes(context.Background(), "/series/one-piece/"); err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if rec.Path != "/series/one-piece/" {
		t.Errorf("request path = %q, want the base-prefixed series path", rec.Path)
	}
}

func TestGogoAnimeGetEpisodesNoEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>no episode list</body></html>"))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/series/none")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without .eplister", len(episodes))
	}
}

func TestGogoAnimeGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/series/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestGogoAnimeFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/one-piece-episode-1178-english-subbed/" {
			_, _ = w.Write(fixture(t, "gogoanime_episode.html"))
			return
		}
		http.NotFound(w, r)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episode := contracts.Episode{
		Num:   "1178",
		RawID: srv.URL + "/one-piece-episode-1178-english-subbed/",
	}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if rec.Path != "/one-piece-episode-1178-english-subbed/" {
		t.Errorf("request path = %q, want the episode page", rec.Path)
	}

	embeds := got.RawEmbeds
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

// TestGogoAnimeResolveStreamRoundTrip walks one mirror through the
// extractor chain: the classic gogo embed host (streaming.php) resolves
// via the modeled encrypt-ajax handshake to a 720 m3u8 source.
func TestGogoAnimeResolveStreamRoundTrip(t *testing.T) {
	t.Parallel()

	sink := newGogoSinkWorld(t, "https://h.example/hls/master.m3u8")
	p := newGogoAnime("https://anitaku.io", testClient(t, "gogoanime"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "https://anitaku.io/naruto-episode-1-english-subbed/",
		RawEmbeds: map[string][]string{
			"Unknown": {sink.srv.URL + "/embedplus?id=content123"},
		},
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

// TestGogoAnimeResolveStreamLazyFetchesDubs keeps the Python behavior
// (gogoanime.py:124-125): an episode without embeds gets its server list
// fetched on demand inside resolve.
func TestGogoAnimeResolveStreamLazyFetchesDubs(t *testing.T) {
	t.Parallel()

	sink := newGogoSinkWorld(t, "https://h.example/hls/lazy.m3u8")

	mirror := gogoMirrorOption(t, 1, sink.srv.URL+"/embedplus?id=content123")
	episodePage := `<html><body><select class="mirror" name="mirror" onchange="loadMi(this);">` +
		`<option value="">Select Video Server</option>` + mirror + `</select></body></html>`

	site := newRequestLog(t, map[string]string{
		"/naruto-episode-1-english-subbed/": episodePage,
	})
	p := newGogoAnime(site.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		Num:       "1",
		RawID:     site.srv.URL + "/naruto-episode-1-english-subbed/",
		RawEmbeds: map[string][]string{},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Unknown")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if _, ok := stream.Links["720"]; !ok {
		t.Fatalf("Links = %v, want a 720 entry via the lazy dub fetch", stream.Links)
	}
}

// TestGogoAnimeResolveStreamFailsLoudWithoutSources pins the no-silent-
// failure rule: when no embed resolves anything, the chain error
// surfaces provider-tagged.
func TestGogoAnimeResolveStreamFailsLoudWithoutSources(t *testing.T) {
	t.Parallel()

	srv := newRequestLog(t, map[string]string{
		"/embedplus": "<html><body>player maintenance</body></html>",
	})

	p := newGogoAnime(srv.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		RawID: srv.srv.URL + "/episode-1/",
		RawEmbeds: map[string][]string{
			"Unknown": {srv.srv.URL + "/embedplus?id=content123"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Unknown")
	if err == nil {
		t.Fatal("error = nil, want the extraction failure of the dead embed")
	}
	if !strings.Contains(err.Error(), "gogoanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

func TestGogoAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := newGogoAnime(GogoAnimeBase, testClient(t, "gogoanime"))
	if p.ID() != "gogoanime" || p.Name() != "GogoAnime" || p.BaseURL() != GogoAnimeBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if GogoAnimeBase != "https://anitaku.io" {
		t.Errorf("GogoAnimeBase = %q, want the Anitaku rebrand domain (issue #410)", GogoAnimeBase)
	}
	// JA audio + EN subs + video (PR23: wanted-language audio → BOTH).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %q, want ja", p.ContentLanguage())
	}
}
