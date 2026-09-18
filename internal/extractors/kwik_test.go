package extractors

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// kwikEncode is the test-side oracle inverse of crypto.KwikDecrypt: each
// plaintext rune becomes ord(r)+v1 encoded in base v2 with digit d
// rendered as key[d], and segments are delimited by key[v2] — exactly the
// transform anicli-py anicli/core/crypto.py:44-78 reverses. Round-trip
// parity with the frozen Python kwik_decrypt is pinned by the golden
// tests in internal/crypto (kwik_test.go).
func kwikEncode(plaintext, key string, v1, v2 int) string {
	var b strings.Builder
	for _, r := range plaintext {
		n := int(r) + v1
		var digits []int
		for n > 0 {
			digits = append(digits, n%v2)
			n /= v2
		}
		if len(digits) == 0 {
			digits = []int{0}
		}
		for i := len(digits) - 1; i >= 0; i-- {
			b.WriteByte(key[digits[i]])
		}
		b.WriteByte(key[v2])
	}
	return b.String()
}

// kwik form HTML handed to kwikEncode: one action attribute and one
// value attribute, mirroring what the decrypted kwik payload carries
// (extractors.py:615-616 scrapes both with lazy regexes).
func kwikForm(actionURL string) string {
	return `<form method="POST" action="` + actionURL + `">` +
		`<input type="hidden" name="_token" value="tok123"/></form>`
}

// kwikPageHTML builds the packed kwik.cx embed page for plaintext around
// the parameter call the Python extractor scrapes
// (extractors.py:604): \("(\w+)",\d+,"(\w+)",(\d+),(\d+),\d+\) — payload,
// alphabet, v1, v2, then arbitrary trailing integers.
func kwikPageHTML(plaintext string) string {
	const (
		key = "zpcmtfvk" // \w-only alphabet; delimiter key[4]='t'
		v1  = 30
		v2  = 4
	)
	return `<!DOCTYPE html><html><head><title>kwik</title></head><body>` +
		`<script>eval(function(p,a,c,k,e,d){e=function(c){return c};return p}` +
		`("` + kwikEncode(plaintext, key, v1, v2) +
		`",9,"` + key + `",` + strconv.Itoa(v1) + `,` + strconv.Itoa(v2) + `,0)</script></body></html>`
}

// kwikWorld serves the kwik embed page and the POST endpoint that
// (optionally) 302-redirects to the media playlist. The embed page is
// stored in an atomic so the test can bake the concrete server URL into
// the decrypted form action. Every request is recorded for header and
// form assertions.
type kwikWorld struct {
	srv        *httptest.Server
	page       atomic.Value // []byte
	rec        kwikRecorder
	playlistOK *atomic.Bool
}

type kwikRecorder struct {
	mu          sync.Mutex
	getReferer  string
	postForm    map[string][]string
	postReferer string
}

func (r *kwikRecorder) snapshot() (getReferer string, postForm map[string][]string, postReferer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getReferer, r.postForm, r.postReferer
}

func newKwikWorld(t *testing.T, actionURL string, redirect bool) *kwikWorld {
	t.Helper()

	w := &kwikWorld{playlistOK: &atomic.Bool{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kwik/e/abc123XYZ":
			w.rec.mu.Lock()
			w.rec.getReferer = r.Header.Get("Referer")
			w.rec.mu.Unlock()
			_, _ = rw.Write(w.page.Load().([]byte)) //nolint:forcetypeassert // stored as []byte below
		case "/dl":
			_ = r.ParseForm()
			w.rec.mu.Lock()
			w.rec.postForm = r.PostForm.Clone()
			w.rec.postReferer = r.Header.Get("Referer")
			w.rec.mu.Unlock()
			if redirect {
				//nolint:gosec // test-owned redirect target
				http.Redirect(rw, r, "http://"+r.Host+"/playlist.m3u8", http.StatusFound)
				return
			}
			_, _ = rw.Write([]byte("no redirect body"))
		case "/playlist.m3u8":
			w.playlistOK.Store(true)
			_, _ = rw.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"))
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.srv.Close)
	w.page.Store([]byte(kwikPageHTML(kwikForm(actionURL))))
	return w
}

// embedURL returns the kwik embed URL on the fake server (the path
// carries the "kwik" substring the matcher keys on).
func (w *kwikWorld) embedURL() string { return w.srv.URL + "/kwik/e/abc123XYZ" }

// mediaURL returns the redirect Location the extractor must recover.
func (w *kwikWorld) mediaURL() string { return w.srv.URL + "/playlist.m3u8" }

// TestKwikExtractRoundTrip is the full kwik flow the frozen Python
// original stubbed out (extractors.py:588-650): embed page fetch with the
// animepahe Referer, packed-param scrape, KwikDecrypt, action/_token
// assembly, and the POST whose 302 Location is the media URL (recovered
// via the netclient's redirect following instead of the body).
func TestKwikExtractRoundTrip(t *testing.T) {
	t.Parallel()

	w := newKwikWorld(t, "PLACEHOLDER", true)
	// Bake the concrete action URL into the decrypted form.
	w.page.Store([]byte(kwikPageHTML(kwikForm(w.srv.URL + "/dl"))))

	ex := &kwikExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), w.embedURL())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("sources = %v, want a 1080 entry", sources)
	}
	if src.URL != w.mediaURL() {
		t.Errorf("URL = %q, want the redirect Location", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8", src.Type)
	}
	if src.Quality != "1080" {
		t.Errorf("Quality = %q, want 1080", src.Quality)
	}
	if src.Headers["Referer"] != kwikReferer {
		t.Errorf("Referer header = %q, want %q (extractors.py:626 comment)", src.Headers["Referer"], kwikReferer)
	}
	if !w.playlistOK.Load() {
		t.Error("master playlist never fetched after the kwik redirect")
	}

	getReferer, postForm, postReferer := w.rec.snapshot()
	// PR49: the site's serving origin is animepahe.pw today (providers.
	// AnimePaheBase); the embed Referer follows the serving origin.
	if getReferer != "https://animepahe.pw" {
		t.Errorf("embed page Referer = %q, want https://animepahe.pw (extractors.py:600)", getReferer)
	}
	if got := postForm["_token"]; len(got) != 1 || got[0] != "tok123" {
		t.Errorf("POST form _token = %v, want [tok123] (value=\"...\" scrape, extractors.py:616-620)", got)
	}
	if postReferer != kwikReferer {
		t.Errorf("POST Referer = %q, want %q", postReferer, kwikReferer)
	}
}

// TestKwikExtractPageShapeMismatch pins the typed error for a kwik page
// without the packed-param call.
func TestKwikExtractPageShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>plain page, no packer</html>"))
	}))
	t.Cleanup(srv.Close)

	ex := &kwikExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/kwik/e/xyz")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:kwik") {
		t.Errorf("err = %v, want extractor:kwik context", err)
	}
}

// TestKwikExtractNoRedirect pins the error when the POST does not yield
// a media redirect (the Location cannot be recovered from the body).
func TestKwikExtractNoRedirect(t *testing.T) {
	t.Parallel()

	w := newKwikWorld(t, "PLACEHOLDER", false)
	w.page.Store([]byte(kwikPageHTML(kwikForm(w.srv.URL + "/dl"))))

	ex := &kwikExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), w.embedURL())
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed for a non-redirecting POST", err)
	}
	if !strings.Contains(err.Error(), "extractor:kwik") {
		t.Errorf("err = %v, want extractor:kwik context", err)
	}
	if w.playlistOK.Load() {
		t.Error("playlist must not be fetched when the POST does not redirect")
	}
}

// TestKwikMatches ports the Python URL gate (extractors.py:594).
func TestKwikMatches(t *testing.T) {
	t.Parallel()

	ex := &kwikExtractor{}
	if !ex.Matches("https://kwik.cx/e/abc") {
		t.Error("kwik.cx URL must match")
	}
	if ex.Matches("https://example.com/watch") {
		t.Error("unrelated URL must not match")
	}
}

// testHTTPClient builds a netclient against an httptest-compatible
// config (mirrors the providers test helper; extractor tests never touch
// the real network).
func testHTTPClient(t *testing.T) *netclient.Client {
	t.Helper()
	c, err := netclient.New(testNetConfig(), netclient.WithProvider("extractors-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	return c
}
