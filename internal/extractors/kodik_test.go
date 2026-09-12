package extractors

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// kodikRot shifts letters by n — the test-side inverse of the ROT-18
// caesar the Python _decrypt_url applies (extractors.py:181-190). The
// shift is normalized to [0,26) first so the byte math stays bounded.
func kodikRot(s string, n int) string {
	shift := byte(((n % 26) + 26) % 26)
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = (c-'A'+shift)%26 + 'A'
		case c >= 'a' && c <= 'z':
			b[i] = (c-'a'+shift)%26 + 'a'
		}
	}
	return string(b)
}

// kodikEncodeSrc builds the encoded src the kodik API returns for a
// plain URL: standard base64 of the URL, then ROT-18 applied to the
// base64 characters — the exact inverse of the Python decode order
// (rot first in _decrypt_url, then b64decode; extractors.py:192-208).
func kodikEncodeSrc(u string) string {
	return kodikRot(base64.StdEncoding.EncodeToString([]byte(u)), -18)
}

// kodikWorld serves the kodik player page, its app.js and the /ftor API.
type kodikWorld struct {
	srv     *httptest.Server
	apiPath string // path the app.js atob resolves to (default /ftor)
	rec     kodikRecorder
}

type kodikRecorder struct {
	mu       sync.Mutex
	form     map[string][]string
	hdr      http.Header
	postPath string
}

func (w *kodikWorld) recorded() (map[string][]string, http.Header, string) {
	w.rec.mu.Lock()
	defer w.rec.mu.Unlock()
	return w.rec.form, w.rec.hdr.Clone(), w.rec.postPath
}

func newKodikWorld(t *testing.T, apiPath string, linksJSON string) *kodikWorld {
	t.Helper()

	w := &kodikWorld{apiPath: apiPath}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kodik/serial/123/abc/720p":
			_, _ = fmt.Fprint(rw, `<!DOCTYPE html><html><head><script src="/assets/js/app.abc123.js"></script></head><body>`+
				`<script>var domain = "kodik-srv.example"; var d_sign = "ds1"; var pd = "pd1"; var pd_sign = "pds";`+
				` var ref = "r1"; var ref_sign = "rs1"; var type = "anime"; var hash = "h123"; var id = "456";`+
				` var player_js_path = "/assets/js/app.abc123.js";</script></body></html>`)
		case "/assets/js/app.abc123.js":
			_, _ = fmt.Fprintf(rw, `$.ajax({url: atob(%q), type: "post"})`,
				base64.StdEncoding.EncodeToString([]byte(apiPath)))
		case apiPath:
			_ = r.ParseForm()
			w.rec.mu.Lock()
			w.rec.form = r.PostForm.Clone()
			w.rec.hdr = r.Header.Clone()
			w.rec.postPath = r.URL.Path
			w.rec.mu.Unlock()
			_, _ = fmt.Fprintf(rw, `{"links": %s}`, linksJSON)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// TestKodikExtractRoundTrip is the full kodik flow (extractors.py:63-208):
// player page var scrape, api path from the app.js atob blob, the
// signed /ftor POST and the ROT-18+base64 link decoding with the
// 720/480.mp4 rename quirk.
func TestKodikExtractRoundTrip(t *testing.T) {
	t.Parallel()

	linksJSON := `{
		"720":  [{"src": ` + fmt.Sprintf("%q", kodikEncodeSrc("//srv-9.example/video/480.mp4")) + `}],
		"360":  [{"src": "https://plain.example/x.m3u8"}],
		"1080": [{"src": ""}]
	}`
	w := newKodikWorld(t, "/ftor", linksJSON)

	ex := &kodikExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), w.srv.URL+"/kodik/serial/123/abc/720p")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// 720: decoded "//srv-9.../480.mp4" absolutized, then the /480.mp4 ->
	// /720.mp4 rename (extractors.py:150-151).
	if src, ok := sources["720"]; !ok || src.URL != "https://srv-9.example/video/720.mp4" {
		t.Errorf("720 = %+v, ok=%v, want https://srv-9.example/video/720.mp4", src, ok)
	}
	// 360: a bare .m3u8 src passes through (extractors.py:193-194).
	if src, ok := sources["360"]; !ok || src.URL != "https://plain.example/x.m3u8" {
		t.Errorf("360 = %+v, ok=%v, want the passthrough m3u8", src, ok)
	}
	// 1080: an empty src contributes nothing (extractors.py:143-144).
	if _, ok := sources["1080"]; ok {
		t.Error("1080 present, want skipped for an empty src")
	}

	form, hdr, postPath := w.recorded()
	if postPath != "/ftor" {
		t.Errorf("api path = %q, want /ftor from the app.js atob blob", postPath)
	}
	for _, want := range []string{"d", "d_sign", "pd", "pd_sign", "ref", "ref_sign", "type", "hash", "id"} {
		if _, ok := form[want]; !ok {
			t.Errorf("POST form missing %q: %v", want, form)
		}
	}
	if got := form["bad_user"]; len(got) != 1 || got[0] != "false" {
		t.Errorf("bad_user = %v, want [false] (extractors.py:120)", got)
	}
	if got := hdr.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest (extractors.py:129)", got)
	}
	if got := hdr.Get("Origin"); got != w.srv.URL {
		t.Errorf("Origin = %q, want %q (extractors.py:126)", got, w.srv.URL)
	}
	if got := hdr.Get("Referer"); got != w.srv.URL+"/kodik/serial/123/abc/720p" {
		t.Errorf("Referer = %q, want the embed URL (extractors.py:127)", got)
	}
}

// TestKodikAPIPathFallback pins the /ftor default when neither the var
// nor the script tag yields a player JS path (extractors.py:84-88,92).
func TestKodikAPIPathFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aniqit/video/9/z/720p":
			// No player_js_path var and no /assets/js/app.*.js script tag:
			// the extractor must fall back to the hardcoded /ftor path.
			_, _ = fmt.Fprint(w, `<html><script>var hash = "h"; var id = "9";</script></html>`)
		case "/ftor":
			_, _ = fmt.Fprint(w, `{"links": {"480": [{"src": "https://p/480.m3u8"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	ex := &kodikExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), srv.URL+"/aniqit/video/9/z/720p")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if src, ok := sources["480"]; !ok || src.URL != "https://p/480.m3u8" {
		t.Errorf("480 = %+v, ok=%v, want the /ftor default response", src, ok)
	}
}

// TestKodikMissingHashOrID pins the guard (extractors.py:77-78) as a
// typed error (Python: silent {}).
func TestKodikMissingHashOrID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	ex := &kodikExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/kodik/serial/1/a/720p")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:kodik") {
		t.Errorf("err = %v, want extractor:kodik context", err)
	}
}

// TestKodikMatches ports the URL gate (extractors.py:67).
func TestKodikMatches(t *testing.T) {
	t.Parallel()

	ex := &kodikExtractor{}
	if !ex.Matches("https://kodik.info/serial/1/a") || !ex.Matches("https://aniqit.com/video/1/b") {
		t.Error("kodik and aniqit URLs must match")
	}
	if ex.Matches("https://example.com/watch") {
		t.Error("unrelated URL must not match")
	}
}
