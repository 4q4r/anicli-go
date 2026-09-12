package extractors

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

// GogoPlay keys are 16-digit strings scraped as (container|videocontent)-NNN
// and used as raw AES key/iv bytes (extractors.py:463-469).
var (
	gogoKeyEnc = "3947103857291746" // keys_match[0]: encryption key
	gogoKeyIV  = "1029384756102938" // keys_match[1]: iv
	gogoKeyDec = "5647382910473829" // keys_match[2]: decryption key
)

// gogoPlayWorld serves the embedplus embed page and the encrypt-ajax.php
// endpoint, decrypting exactly like the player does (AES via the ported
// internal/crypto — the same primitives the frozen Python
// anicli.core.crypto provided, extractors.py:478-498).
type gogoPlayWorld struct {
	srv *httptest.Server

	// embedParams is the decrypted data-value payload ("id=...&alias=...").
	embedParams string
	// ajaxSources is the decrypted source JSON the ajax endpoint returns.
	ajaxSources string

	rec gogoPlayRecorder
}

type gogoPlayRecorder struct {
	mu        sync.Mutex
	ajaxHdr   http.Header
	ajaxQuery string
}

func (w *gogoPlayWorld) recorded() (http.Header, string) {
	w.rec.mu.Lock()
	defer w.rec.mu.Unlock()
	return w.rec.ajaxHdr.Clone(), w.rec.ajaxQuery
}

func newGogoPlayWorld(t *testing.T, embedParams, ajaxSources string) *gogoPlayWorld {
	t.Helper()

	w := &gogoPlayWorld{embedParams: embedParams, ajaxSources: ajaxSources}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embedplus":
			// The encrypted data-value is the AES-CBC of embedParams under
			// key/iv — regenerated per world so the vectors stay
			// oracle-consistent with internal/crypto goldens. The container
			// marker must appear BEFORE the videocontent one: the extractor
			// assigns keys in scan order (extractors.py:466-469).
			enc, err := crypto.AESEncrypt(embedParams, []byte(gogoKeyEnc), []byte(gogoKeyIV))
			if err != nil {
				t.Errorf("encrypt data-value: %v", err)
				rw.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(rw, `<!DOCTYPE html><html><body><div class="container-%s" id="videocontent-%s" `+
				`data-value=%q></div><script>var videocontent-%s; var container-%s;</script></body></html>`,
				gogoKeyEnc, gogoKeyIV, enc, gogoKeyDec, gogoKeyDec)
		case "/encrypt-ajax.php":
			w.rec.mu.Lock()
			w.rec.ajaxHdr = r.Header.Clone()
			w.rec.ajaxQuery = r.URL.RawQuery
			w.rec.mu.Unlock()

			enc, err := crypto.AESEncrypt(ajaxSources, []byte(gogoKeyDec), []byte(gogoKeyIV))
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

// gogoSourceJSON builds the decrypted source payload shape
// (extractors.py:499-515): source[] with file+label, source_bk[] backup.
func gogoSourceJSON(entries ...[2]string) string {
	type src struct {
		File  string `json:"file"`
		Label string `json:"label"`
	}
	out := struct {
		Source   []src `json:"source"`
		SourceBk []src `json:"source_bk"`
	}{}
	for i, e := range entries {
		s := src{File: e[0], Label: e[1]}
		if i < len(entries)-1 {
			out.Source = append(out.Source, s)
		} else {
			out.SourceBk = append(out.SourceBk, s)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestGogoPlayExtractRoundTrip is the full gogoplay flow
// (extractors.py:441-520): embed page keys + data-value scrape, AES
// decrypt of the params, AES-encrypted id, encrypt-ajax.php component
// assembly and the final decrypted source list.
func TestGogoPlayExtractRoundTrip(t *testing.T) {
	t.Parallel()

	w := newGogoPlayWorld(t,
		"id=content123&alias=naruto&ts=1",
		gogoSourceJSON(
			[2]string{"https://h.example/hls/master.m3u8", "1080 P"},
			[2]string{"https://h.example/hls/720.m3u8", "720 P"},
			[2]string{"https://bk.example/720.m3u8", "Default"},
		))

	ex := &gogoPlayExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), w.srv.URL+"/embedplus?id=content123")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if src, ok := sources["1080"]; !ok || src.URL != "https://h.example/hls/master.m3u8" || src.Type != "m3u8" {
		t.Errorf("1080 = %+v, ok=%v, want master.m3u8 type m3u8", src, ok)
	}
	if src, ok := sources["720"]; !ok || src.URL != "https://h.example/hls/720.m3u8" {
		t.Errorf("720 = %+v, ok=%v, want the labeled 720 source (label digits, extractors.py:506-507)", src, ok)
	}

	hdr, query := w.recorded()
	if got := hdr.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("ajax X-Requested-With = %q, want XMLHttpRequest (extractors.py:494)", got)
	}
	// Component = decrypted params + &id=<encrypted> + &alias=<content id>
	// (extractors.py:486-492). The base64 id may URL-mangle, so assert
	// the stable text pieces.
	for _, want := range []string{"alias=naruto", "&alias=content123"} {
		if !strings.Contains(query, want) {
			t.Errorf("ajax query = %q, want it to contain %q", query, want)
		}
	}
	if !strings.Contains(query, "id=") {
		t.Errorf("ajax query = %q, want an id= parameter", query)
	}
}

// TestGogoPlaySourceBkFills720 pins the backup rule (extractors.py:511-515):
// source_bk lands under quality 720 only when 720 is still absent, and a
// label without digits defaults to 720 (extractors.py:506-507).
func TestGogoPlaySourceBkFills720(t *testing.T) {
	t.Parallel()

	w := newGogoPlayWorld(t, "id=zzz",
		gogoSourceJSON(
			[2]string{"https://h.example/hls/master.m3u8", "1080 P"},
			[2]string{"https://bk.example/backup.m3u8", "Default"},
		))

	ex := &gogoPlayExtractor{http: testHTTPClient(t)}
	sources, err := ex.Extract(context.Background(), w.srv.URL+"/embedplus?id=zzz")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if src, ok := sources["720"]; !ok || src.URL != "https://bk.example/backup.m3u8" {
		t.Errorf("720 = %+v, ok=%v, want the source_bk backup link", src, ok)
	}
}

// TestGogoPlayShapeMismatch pins the typed error when the embed page
// carries no AES key markers. Python returned {} silently here
// (extractors.py:465); the port fails loud per the task ruling.
func TestGogoPlayShapeMismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>no keys here</html>"))
	}))
	t.Cleanup(srv.Close)

	ex := &gogoPlayExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), srv.URL+"/embedplus?id=content123")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "extractor:gogoplay") {
		t.Errorf("err = %v, want extractor:gogoplay context", err)
	}
}

// TestGogoPlayMissingContentID pins the missing-id gate. Python returned
// {} (extractors.py:456-457); the port surfaces the typed error instead.
func TestGogoPlayMissingContentID(t *testing.T) {
	t.Parallel()

	ex := &gogoPlayExtractor{http: testHTTPClient(t)}
	_, err := ex.Extract(context.Background(), "https://gogoplay.example/embedplus")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed for an id-less embed URL", err)
	}
	if !strings.Contains(err.Error(), "extractor:gogoplay") {
		t.Errorf("err = %v, want extractor:gogoplay context", err)
	}
}

// TestGogoPlayMatches ports the six-way URL rule (extractors.py:448).
// "turbovid" hosts are deliberately absent: the frozen Python factory
// has no rule for them, so such URLs match nothing upstream.
func TestGogoPlayMatches(t *testing.T) {
	t.Parallel()

	ex := &gogoPlayExtractor{}
	for _, u := range []string{
		"https://gogoplay.io/embedplus?id=1",
		"https://playtaku.net/streaming.php?id=1",
		"https://playgo.one/e/1",
		"https://goload.pro/streaming.php?id=1",
		"https://x.example/streaming.php?id=1",
		"https://x.example/embedplus?id=1",
	} {
		if !ex.Matches(u) {
			t.Errorf("Matches(%q) = false, want true (six-way rule)", u)
		}
	}
	for _, u := range []string{
		"https://turbovid.example/e/1",
		"https://example.com/watch",
	} {
		if ex.Matches(u) {
			t.Errorf("Matches(%q) = true, want false", u)
		}
	}
}

// TestGogoPlayComponentBase64RoundTrip sanity-pins the AES vectors the
// world generates against the ported crypto (belt-and-braces next to the
// internal/crypto goldens).
func TestGogoPlayComponentBase64RoundTrip(t *testing.T) {
	t.Parallel()

	enc, err := crypto.AESEncrypt("id=abc", []byte(gogoKeyEnc), []byte(gogoKeyIV))
	if err != nil {
		t.Fatalf("AESEncrypt: %v", err)
	}
	dec, err := crypto.AESDecrypt(enc, []byte(gogoKeyEnc), []byte(gogoKeyIV))
	if err != nil {
		t.Fatalf("AESDecrypt: %v", err)
	}
	if dec != "id=abc" {
		t.Errorf("round trip = %q, want id=abc", dec)
	}
	if _, err := base64.StdEncoding.DecodeString(enc); err != nil {
		t.Errorf("encrypted value is not standard base64: %v", err)
	}
}
