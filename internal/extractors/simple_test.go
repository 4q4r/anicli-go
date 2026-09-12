package extractors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// --- Aniboom ---

// TestAniboomDataParameters pins the data-parameters branch
// (extractors.py:226-252) on an attribute shape the Python unescape
// chain can actually parse: the chain's `&quot;}` → `}` replace EATS the
// closing quote of any value touching a closing brace, so the canonical
// `{...&quot;}}` tail decodes to invalid JSON upstream too (Python
// swallows the json.loads error into {}). Only shapes whose last quoted
// value is separated from the closing braces (here: by a space) survive
// — the fixture models that shape and the Go port stays bug-compatible.
func TestAniboomDataParameters(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><body><div id="player" data-parameters="{&quot;hls&quot;:{&quot;src&quot;:&quot;https:\/\/h.example\/master.m3u8&quot; }}"></div></body></html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &aniboomExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/aniboom/1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want 1080", sources)
	}
	if src.URL != "https://h.example/master.m3u8" {
		t.Errorf("URL = %q, want the unescaped m3u8", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8 (extractors.py:244)", src.Type)
	}
	for k, want := range map[string]string{
		"Referer":         "https://animego.org/",
		"Origin":          "https://aniboom.one",
		"Accept-Language": "ru-RU",
	} {
		if got := src.Headers[k]; got != want {
			t.Errorf("header %s = %q, want %q (extractors.py:217-222)", k, got, want)
		}
	}
}

// TestAniboomCanonicalAttributeIsChainBroken pins the upstream chain bug:
// the canonical attribute shape (last value's quote flush against the
// closing braces) decodes to invalid JSON — Python swallowed this into
// {}; the Go port surfaces the typed shape error instead.
func TestAniboomCanonicalAttributeIsChainBroken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><body><div data-parameters="{&quot;hls&quot;:{&quot;src&quot;:&quot;https:\/\/h.example\/master.m3u8&quot;}}"></div></body></html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &aniboomExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/aniboom/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed (the chain eats the closing quote upstream too)", err)
	}
	if !strings.Contains(err.Error(), "extractor:aniboom") {
		t.Errorf("err = %v, want extractor:aniboom context", err)
	}
}

// TestAniboomDashOverridesHLS pins the dash-mpd overwrite order
// (extractors.py:246-250): when both branches decode, dash lands under
// the same 1080 key last. Both objects carry the space-separated
// closing shape the chain can parse.
func TestAniboomDashOverridesHLS(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><div data-parameters="{&quot;hls&quot;:{&quot;src&quot;:&quot;https:\/\/h.example\/m.m3u8&quot; },&quot;dash&quot;:{&quot;src&quot;:&quot;https:\/\/d.example\/stream.mpd&quot; }}"></div></html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &aniboomExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/aniboom/1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want 1080", sources)
	}
	if src.URL != "https://d.example/stream.mpd" {
		t.Errorf("URL = %q, want the dash mpd to overwrite hls", src.URL)
	}
	if src.Type != "" {
		t.Errorf("Type = %q, want empty (dash branch passes no type)", src.Type)
	}
}

// TestAniboomHLSFallback pins the no-data-parameters hls regex fallback
// (extractors.py:227-231).
func TestAniboomHLSFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><script>var player = new Player({"hls":{"src":"https:\/\/f.example\/y.m3u8"}});</script></html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &aniboomExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/aniboom/1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want 1080 from the hls fallback", sources)
	}
	if src.URL != "https://f.example/y.m3u8" {
		t.Errorf("URL = %q", src.URL)
	}
	if src.Type != "" {
		t.Errorf("Type = %q, want empty (fallback branch passes no type, extractors.py:230)", src.Type)
	}
}

// TestAniboomShapeMismatch pins the typed error when neither marker
// exists (Python: silent {}, extractors.py:231).
func TestAniboomShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>nothing recognizable</html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &aniboomExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/aniboom/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:aniboom") {
		t.Errorf("err = %v, want extractor:aniboom context", err)
	}
}

// --- Alloha ---

// TestAllohaRoundTrip pins the alloha flow (extractors.py:328-391): id
// and token scrape off the player page, the /movie/<id> POST and the
// hlsSource quality map with the " or " split and "Object" skip.
func TestAllohaRoundTrip(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		form     map[string][]string
		postPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alloha/777":
			_, _ = fmt.Fprint(w, `<html><script>var x = {"id": "777", "token": "tok-abc"};</script></html>`)
		case "/movie/777":
			_ = r.ParseForm()
			mu.Lock()
			form = r.PostForm.Clone()
			postPath = r.URL.Path
			mu.Unlock()
			_, _ = fmt.Fprint(w, `{"hlsSource":[{"quality":{"1080":"https://h.example/1080.m3u8",`+
				`"720":"https://h.example/720a.m3u8 or https://h.example/720b.m3u8",`+
				`"Object":"https://skip.me/x.m3u8"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ex := &allohaExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/alloha/777")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if src, ok := sources["1080"]; !ok || src.URL != "https://h.example/1080.m3u8" {
		t.Errorf("1080 = %+v, ok=%v", src, ok)
	}
	// " or " split keeps the first alternative (extractors.py:379).
	if src, ok := sources["720"]; !ok || src.URL != "https://h.example/720a.m3u8" {
		t.Errorf("720 = %+v, ok=%v, want the first alternative of the \" or \" split", src, ok)
	}
	if _, ok := sources["Object"]; ok {
		t.Error("Object key must be skipped (extractors.py:378)")
	}
	if src, ok := sources["1080"]; ok && src.Headers["Origin"] != srv.URL {
		t.Errorf("Origin header = %q, want %q (extractors.py:361-365)", src.Headers["Origin"], srv.URL)
	}

	mu.Lock()
	defer mu.Unlock()
	if postPath != "/movie/777" {
		t.Errorf("POST path = %q, want /movie/777", postPath)
	}
	if got := form["token"]; len(got) != 1 || got[0] != "tok-abc" {
		t.Errorf("form token = %v, want [tok-abc]", got)
	}
	if got := form["av1"]; len(got) != 1 || got[0] != "0" {
		t.Errorf("form av1 = %v, want [0] (extractors.py:355)", got)
	}
}

// TestAllohaShapeMismatch pins the no-id guard (extractors.py:341).
func TestAllohaShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>no id</html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &allohaExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/alloha/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:alloha") {
		t.Errorf("err = %v, want extractor:alloha context", err)
	}
}

// TestAllohaMatches ports the URL gate including the "all." quirk
// (extractors.py:331).
func TestAllohaMatches(t *testing.T) {
	t.Parallel()

	ex := &allohaExtractor{}
	if !ex.Matches("https://alloha.tv/1") || !ex.Matches("https://all4all.example/w") {
		t.Error("alloha and all. URLs must match")
	}
	if ex.Matches("https://example.com/watch") {
		t.Error("unrelated URL must not match")
	}
}

// --- Sibnet ---

// TestSibnetRoundTrip pins the sibnet flow (extractors.py:34-60): the
// src: mp4 scrape, the video.sibnet.ru absolutization and the 480
// quality with the embed Referer.
func TestSibnetRoundTrip(t *testing.T) {
	t.Parallel()

	var gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		_, _ = fmt.Fprint(w, `<html><script>player.init(); src: "/videos/2026/01/123.mp4"</script></html>`)
	}))
	t.Cleanup(srv.Close)

	embed := srv.URL + "/sibnet/shell.php?videoid=1"
	ex := &sibnetExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), embed)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["480"]
	if !ok {
		t.Fatalf("sources = %v, want 480", sources)
	}
	if src.URL != "https://video.sibnet.ru/videos/2026/01/123.mp4" {
		t.Errorf("URL = %q, want the absolutized video.sibnet.ru link (extractors.py:48-52)", src.URL)
	}
	if src.Quality != "480" {
		t.Errorf("Quality = %q, want 480", src.Quality)
	}
	if src.Headers["Referer"] != embed {
		t.Errorf("Referer = %q, want the embed URL (extractors.py:55)", src.Headers["Referer"])
	}
	if gotReferer != embed {
		t.Errorf("page fetch Referer = %q, want the embed URL (extractors.py:44)", gotReferer)
	}
}

// TestSibnetAbsoluteSrcKept pins the http-passthrough branch
// (extractors.py:50-52).
func TestSibnetAbsoluteSrcKept(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<script>src: "https://mirror.example/v/9.mp4"</script>`)
	}))
	t.Cleanup(srv.Close)

	ex := &sibnetExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/sibnet/1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if src, ok := sources["480"]; !ok || src.URL != "https://mirror.example/v/9.mp4" {
		t.Errorf("480 = %+v, ok=%v, want the absolute passthrough", src, ok)
	}
}

// TestSibnetShapeMismatch pins the missing-src guard.
func TestSibnetShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>no video</html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &sibnetExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/sibnet/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:sibnet") {
		t.Errorf("err = %v, want extractor:sibnet context", err)
	}
}

// --- StreamTape ---

// TestStreamTapeRoundTrip pins the robotlink assembly
// (extractors.py:523-535): https: + group1 + "xcd" + group2.
func TestStreamTapeRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<script>document.getElementById('robotlink').innerHTML = '//get.example.com/xyz'+ ('xcd654321abcdef');</script>`)
	}))
	t.Cleanup(srv.Close)

	ex := &streamTapeExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/streamtape/e/1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want 1080", sources)
	}
	if src.URL != "https://get.example.com/xyzxcd654321abcdef" {
		t.Errorf("URL = %q, want the xcd-stitched link (extractors.py:531)", src.URL)
	}
	if src.Type != "mp4" {
		t.Errorf("Type = %q, want mp4", src.Type)
	}
}

// TestStreamTapeShapeMismatch pins the missing-robotlink guard.
func TestStreamTapeShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>nothing</html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &streamTapeExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/streamtape/e/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:streamtape") {
		t.Errorf("err = %v, want extractor:streamtape context", err)
	}
}

// --- DoodStream ---

// TestDoodRoundTrip pins the dood flow (extractors.py:538-558): the
// pass_md5 path + token scrape, the md5 endpoint fetch and the final
// token-stitched URL. The base divergence (embed origin instead of the
// hardcoded https://dood.la) is documented in dood.go.
func TestDoodRoundTrip(t *testing.T) {
	t.Parallel()

	var passMD5Referer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dood/e/abc123":
			_, _ = fmt.Fprint(w, `<script>dsplayer.play('/pass_md5/0123456789abcdef/'+'#');MDToken='?token=abc123def';</script>`)
		case "/pass_md5/0123456789abcdef/":
			passMD5Referer = r.Header.Get("Referer")
			_, _ = w.Write([]byte("hashstring9876"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ex := &doodExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/dood/e/abc123")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want 1080", sources)
	}
	// final = <md5 body> + <full "?token=..." match> + "&expiry=<ms>"
	// (extractors.py:554). group(0) keeps the "?token=" prefix.
	wantPrefix := "hashstring9876?token=abc123def&expiry="
	if !strings.HasPrefix(src.URL, wantPrefix) {
		t.Errorf("URL = %q, want prefix %q", src.URL, wantPrefix)
	}
	expiry := strings.TrimPrefix(src.URL, wantPrefix)
	if len(expiry) != 13 || strings.Trim(expiry, "0123456789") != "" {
		t.Errorf("expiry = %q, want a 13-digit millisecond timestamp", expiry)
	}
	if src.Type != "mp4" {
		t.Errorf("Type = %q, want mp4", src.Type)
	}
	if src.Headers["Referer"] != srv.URL {
		t.Errorf("Referer = %q, want the embed origin %q", src.Headers["Referer"], srv.URL)
	}
	if passMD5Referer != srv.URL+"/dood/e/abc123" {
		t.Errorf("pass_md5 Referer = %q, want the embed URL (extractors.py:552)", passMD5Referer)
	}
}

// TestDoodShapeMismatch pins the missing pass_md5/token guard.
func TestDoodShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>nothing</html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &doodExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/dood/e/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:dood") {
		t.Errorf("err = %v, want extractor:dood context", err)
	}
}

// --- CdnVideoHub ---

// TestCdnVideoHubRoundTrip pins the cdn-iframe flow (extractors.py:258-325):
// data attribute scrape, path-part dubbing/season/episode targeting, the
// playlist API lookup and the quality map projection.
func TestCdnVideoHubRoundTrip(t *testing.T) {
	t.Parallel()

	var playlistQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path arrives percent-decoded; the encoded "Studio%20X"
		// path part is "Studio X" here.
		switch r.URL.Path {
		case "/cdn-iframe/55/Studio X/1/2":
			_, _ = fmt.Fprint(w, `<html><body data-title-id="777" data-publisher-id="55" data-aggregator="aggr1"></body></html>`)
		case "/api/v1/player/sv/playlist":
			playlistQuery = r.URL.Query()
			_, _ = fmt.Fprint(w, `{"items":[`+
				`{"episode":9,"season":9,"voiceStudio":"Other","vkId":"wrong"},`+
				`{"episode":2,"season":1,"voiceStudio":"Studio X","vkId":"v192"}]}`)
		case "/api/v1/player/sv/video/v192":
			_, _ = fmt.Fprint(w, `{"sources":{"mpegHighUrl":"https://v.example/720.mp4",`+
				`"hlsUrl":"https://v.example/master.m3u8","mpegTinyUrl":""}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ex := &cdnVideoHubExtractor{http: testHTTPClient(t), apiBase: srv.URL}
	sources, err := ex.Extract(context.Background(), srv.URL+"/cdn-iframe/55/Studio%20X/1/2")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if src, ok := sources["720"]; !ok || src.URL != "https://v.example/720.mp4" {
		t.Errorf("720 = %+v, ok=%v, want mpegHighUrl", src, ok)
	}
	if src, ok := sources["1080"]; !ok || src.URL != "https://v.example/master.m3u8" {
		t.Errorf("1080 = %+v, ok=%v, want hlsUrl", src, ok)
	}
	if _, ok := sources["144"]; ok {
		t.Error("empty mpegTinyUrl must be skipped (extractors.py:317)")
	}
	if got := playlistQuery.Get("pub"); got != "55" {
		t.Errorf("playlist pub = %q, want 55 (extractors.py:282-287)", got)
	}
	if got := playlistQuery.Get("aggr"); got != "aggr1" {
		t.Errorf("playlist aggr = %q, want aggr1", got)
	}
	if got := playlistQuery.Get("id"); got != "777" {
		t.Errorf("playlist id = %q, want 777", got)
	}
}

// TestCdnVideoHubNoMatchingItem pins the empty-not-error outcome when
// the playlist carries no entry for the targeted episode/season/dubber
// (Python returns {} without an exception, extractors.py:299-300).
func TestCdnVideoHubNoMatchingItem(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn-iframe/55/None/1/2":
			_, _ = fmt.Fprint(w, `<body data-title-id="777" data-publisher-id="55" data-aggregator="aggr1"></body>`)
		case "/api/v1/player/sv/playlist":
			_, _ = fmt.Fprint(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ex := &cdnVideoHubExtractor{http: testHTTPClient(t), apiBase: srv.URL}
	sources, err := ex.Extract(context.Background(), srv.URL+"/cdn-iframe/55/None/1/2")
	if err != nil {
		t.Fatalf("Extract: %v, want nil for an unmatched playlist entry", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want empty", sources)
	}
}
