package lua

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// TestSDKHTTPNotFoundRaisesTypedMarker pins the typed-miss contract the
// animevost port (the first provider whose live API answers misses with
// HTTP 404) rides: a netclient 404 — classified contracts.ErrNotFound by
// the transport's status map — must raise under the anicli:not_found:
// marker so classifyVMError re-attaches the sentinel for consumers
// (errors.Is branches hold for Lua providers like they do for the
// compiled ones). Before the mapping the marker was lost and the miss
// degraded to an untyped transport error.
func TestSDKHTTPNotFoundRaisesTypedMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"error":"Ничего не найдено"}`)
	}))
	t.Cleanup(srv.Close)

	log, _ := testLogger(t)
	cfg := DefaultConfig()
	cfg.Timeout = 2 * time.Second
	client, err := netclient.New(config.Default().Network)
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	cfg.HTTP = client
	e := NewEngine(cfg, log)

	_, evalErr := evalSDK(t, e, fmt.Sprintf(`anicli.http.get(%q)`, srv.URL+"/search"))
	if evalErr == nil {
		t.Fatal("error = nil, want the typed 404 failure")
	}
	if !strings.Contains(evalErr.Error(), "anicli:not_found:") {
		t.Fatalf("error = %v, want the anicli:not_found: marker (the typed-miss contract)", evalErr)
	}
}

// The PR116 SDK extensions: http.get opts (custom headers),
// http.get_batch (bounded-parallel fan-out with per-URL soft failure)
// and anicli.extract (the Go extractor factory surfaced to scripts).

func TestSDKHTTPGetHeaders(t *testing.T) {
	var gotReferer, gotXProbe string
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		gotXProbe = r.Header.Get("X-Probe")
		_, _ = fmt.Fprint(w, "ok")
	}))

	_, err := evalSDK(t, e, fmt.Sprintf(`
		anicli.http.get(%q, { headers = { Referer = "https://ref.example/", ["X-Probe"] = "1" } })
		return "done"
	`, srv.URL+"/page"))
	if err != nil {
		t.Fatal(err)
	}
	if gotReferer != "https://ref.example/" || gotXProbe != "1" {
		t.Fatalf("server saw Referer=%q X-Probe=%q, want the script headers", gotReferer, gotXProbe)
	}
}

// TestSDKHTTPPostHeaders pins the POST opts extension (PR130): the
// anizone Livewire continuation POSTs JSON with X-CSRF-TOKEN /
// X-Requested-With / X-Livewire headers — http.post grows the same
// optional opts table http.get has had since PR116, symmetrically as
// the trailing argument. The content-type keeps its positional slot
// (the existing three-argument call sites stay verbatim).
func TestSDKHTTPPostHeaders(t *testing.T) {
	var gotMethod, gotCT, gotCSRF, gotXRW, gotBody string
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotCSRF = r.Header.Get("X-CSRF-TOKEN")
		gotXRW = r.Header.Get("X-Requested-With")
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		_, _ = fmt.Fprint(w, "ok")
	}))

	_, err := evalSDK(t, e, fmt.Sprintf(`
		anicli.http.post(%q, '{"components":[]}', "application/json",
			{ headers = { ["X-CSRF-TOKEN"] = "tok-1", ["X-Requested-With"] = "XMLHttpRequest" } })
		return "done"
	`, srv.URL+"/livewire/update"))
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want the positional argument", gotCT)
	}
	if gotCSRF != "tok-1" || gotXRW != "XMLHttpRequest" {
		t.Fatalf("server saw X-CSRF-TOKEN=%q X-Requested-With=%q, want the opts headers (the Livewire POST needs them)", gotCSRF, gotXRW)
	}
	if gotBody != `{"components":[]}` {
		t.Errorf("body = %q, want the script body verbatim", gotBody)
	}
}

// TestSDKHTTPGetBatch pins the bounded-parallel fan-out: every URL
// resolves to its index-aligned result, a dead URL carries the error
// field (soft per-URL failure — one dead leg never kills the batch)
// and max_parallel bounds the in-flight window.
func TestSDKHTTPGetBatch(t *testing.T) {
	var inflight, maxInflight int32
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxInflight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInflight, old, cur) {
				break
			}
		}
		_, _ = fmt.Fprintf(w, "body-%s", html.EscapeString(r.URL.Path))
		atomic.AddInt32(&inflight, -1)
	}))

	base := srv.URL
	dead := "http://127.0.0.1:1/dead" // port 1: nothing listens
	got, err := evalSDK(t, e, fmt.Sprintf(`
		local rs = anicli.http.get_batch({ %q.."/a", %q, %q.."/c" }, 2)
		local out = {}
		for i, r in ipairs(rs) do
			if r.error then out[i] = "ERR" else out[i] = r.status .. ":" .. r.body end
		end
		return out[1] .. "|" .. out[2] .. "|" .. out[3]
	`, base, dead, base))
	if err != nil {
		t.Fatal(err)
	}
	if got != "200:body-/a|ERR|200:body-/c" {
		t.Fatalf("get_batch = %q", got)
	}
	if max := atomic.LoadInt32(&maxInflight); max > 2 {
		t.Fatalf("max in-flight = %d, want ≤ 2 (the bound)", max)
	}
}

// TestSDKExtractKodik pins anicli.extract through the real extractor
// factory: a kodik-shaped embed resolves to its quality-keyed source
// table (the same fixture shape the provider round-trip tests use).
func TestSDKExtractKodik(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))

	got, err := evalSDK(t, e, fmt.Sprintf(`
		local links = anicli.extract({ %q.."/kodik/seria/1340414/e8652ad443b3ceb2057d17f8aa0b35d7/720p" })
		for q, src in pairs(links) do
			return q .. ":" .. src.url .. ":" .. tostring(src.quality)
		end
		return "empty"
	`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if got != "720:https://plain.example/x/720.m3u8:720" {
		t.Fatalf("extract = %q", got)
	}
}

// TestSDKExtractMP4FastPath pins the direct-media fast path: a URL
// ending in .mp4/.m3u8 resolves to quality-720 without any extractor
// hop (the resolveEmbeds suffix-first semantics).
func TestSDKExtractMP4FastPath(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no network may happen for a direct media URL")
	}))

	got, err := evalSDK(t, e, `
		local links = anicli.extract("https://cdn.example/video/all.mp4")
		return links["720"].url .. ":" .. links["720"].quality
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cdn.example/video/all.mp4:720" {
		t.Fatalf("extract mp4 = %q", got)
	}
}

// TestSDKExtractTotalFailureRaises pins the no-silent-failure rule:
// when NOTHING resolved and an error was captured, extract raises
// (the script surfaces it; extract must never answer a silent empty).
func TestSDKExtractTotalFailureRaises(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>no player data</html>")
	}))

	_, err := evalSDK(t, e, fmt.Sprintf(`
		anicli.extract({ %q.."/kodik/seria/1/h/720p" })
		return "unreachable"
	`, srv.URL))
	if err == nil {
		t.Fatal("error = nil, want the extract failure raise")
	}
}
