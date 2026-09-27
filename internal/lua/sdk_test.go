package lua

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSDKEngine builds a sandboxed engine wired to a captured logger
// with small budgets, plus the httptest server the SDK calls.
func newSDKEngine(t *testing.T, handler http.Handler) (*Engine, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	log, buf := testLogger(t)
	cfg := DefaultConfig()
	cfg.Timeout = 2 * time.Second
	cfg.BodyLimit = 1024
	e := NewEngine(cfg, log)
	return e, srv, buf
}

func evalSDK(t *testing.T, e *Engine, src string) (string, error) {
	t.Helper()
	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	ls := e.NewState(sctx)
	defer ls.Close()
	if err := ls.DoString(src); err != nil {
		return "", err
	}
	return ls.Get(-1).String(), nil
}

func TestSDKHTTPGet(t *testing.T) {
	var gotPath, gotMethod string
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "hello lua")
	}))

	got, err := evalSDK(t, e, fmt.Sprintf(`
		local r = anicli.http.get(%q)
		return r.status .. ":" .. r.body .. ":" .. tostring(r.headers["Content-Type"])
	`, srv.URL+"/page"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "200:hello lua:text/plain" {
		t.Fatalf("http.get = %q", got)
	}
	if gotPath != "/page" || gotMethod != "GET" {
		t.Fatalf("server saw %s %s", gotMethod, gotPath)
	}
}

func TestSDKHTTPGetJSON(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"a": 1, "list": ["x"]}`)
	}))
	got, err := evalSDK(t, e, fmt.Sprintf(`
		local t = anicli.http.get_json(%q)
		return tostring(t.a) .. ":" .. t.list[1]
	`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if got != "1:x" {
		t.Fatalf("get_json = %q", got)
	}

	// Invalid JSON surfaces a decode error, not a nil table.
	_, err = evalSDK(t, e, fmt.Sprintf(`return anicli.http.get_json(%q).a`, srv.URL))
	if err == nil {
		_ = err // second call hits the same payload — craft a broken server below
	}
}

func TestSDKHTTPGetJSONInvalid(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{not json`)
	}))
	_, err := evalSDK(t, e, fmt.Sprintf(`return anicli.http.get_json(%q)`, srv.URL))
	if err == nil || !strings.Contains(err.Error(), "json") {
		t.Fatalf("invalid JSON must error with a json message, got: %v", err)
	}
}

func TestSDKHTTPPost(t *testing.T) {
	var gotMethod, gotCT, gotBody string
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("X-Answer", "42")
		_, _ = fmt.Fprint(w, "posted")
	}))

	got, err := evalSDK(t, e, fmt.Sprintf(`
		local r = anicli.http.post(%q, "q=1", "application/x-www-form-urlencoded")
		return r.status .. ":" .. r.body .. ":" .. tostring(r.headers["X-Answer"])
	`, srv.URL+"/submit"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "200:posted:42" {
		t.Fatalf("http.post = %q", got)
	}
	if gotMethod != "POST" || gotCT != "application/x-www-form-urlencoded" || gotBody != "q=1" {
		t.Fatalf("server saw %s ct=%q body=%q", gotMethod, gotCT, gotBody)
	}
}

// TestSDKHTTPBodyCap: response bodies larger than the budget never
// reach a script (issue #521 mitigation).
func TestSDKHTTPBodyCap(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	_, err := evalSDK(t, e, fmt.Sprintf(`return anicli.http.get(%q).body`, srv.URL))
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("an oversized body must hit the cap, got: %v", err)
	}
}

// TestSDKHTTPDeadlineCarried: the VM context (engine budget) bounds
// the HTTP call — a slow server fails fast.
func TestSDKHTTPDeadlineCarried(t *testing.T) {
	e, srv, _ := newSDKEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = fmt.Fprint(w, "late")
	}))
	e.cfg.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := evalSDK(t, e, fmt.Sprintf(`return anicli.http.get(%q).body`, srv.URL))
	if err == nil {
		t.Fatal("a slow server must hit the context deadline")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the deadline took %v to fire", time.Since(start))
	}
}

func TestSDKQueryEscape(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `return anicli.http.query_escape("a b/c")`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a+b%2Fc" {
		t.Fatalf("query_escape = %q", got)
	}
}

func TestSDKJSONDecodeEncode(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `
		local t = anicli.json.decode('{"k": 1, "tags": ["a"]}')
		local s = anicli.json.encode({k = 2})
		return tostring(t.k) .. ":" .. t.tags[1] .. ":" .. s
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1:a:{\"k\":2}" {
		t.Fatalf("json round-trip = %q", got)
	}
}

func TestSDKHTMLParse(t *testing.T) {
	const page = `[[<html><body>
		<div id="m"><a href="/x">Name</a><a href="/y">Other</a></div>
	</body></html>]]`
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())

	for _, tc := range []struct{ src, want string }{
		{`local d = anicli.html.parse(PAGE); local a = d:find("a"); return tostring(a:len())`, "2"},
		{`local d = anicli.html.parse(PAGE); return d:find("a"):text()`, "Name"},
		{`local d = anicli.html.parse(PAGE); return d:find("a"):attr("href")`, "/x"},
		{`local d = anicli.html.parse(PAGE); local out = {}
		  d:find("a"):each(function(i, s) out[i] = s:attr("href") end)
		  return out[1] .. "|" .. out[2]`, "/x|/y"},
		{`local d = anicli.html.parse(PAGE); return tostring(d:find("div#m"):attr("id"))`, "m"},
	} {
		src := "local PAGE = " + page + "\n" + tc.src
		got, err := evalSDK(t, e, src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.src, got, tc.want)
		}
	}
}

func TestSDKRegexpMatch(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `
		local m = anicli.regexp.match("id (\\d+)-(\\d+)", "x id 12-34 y")
		return m[1] .. ":" .. m[2] .. ":" .. m[3]
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "id 12-34:12:34" {
		t.Fatalf("regexp.match = %q (want full match + captures)", got)
	}

	noMatch, err := evalSDK(t, e, `local m = anicli.regexp.match("zzz", "abc"); return tostring(m)`)
	if err != nil || noMatch != "nil" {
		t.Fatalf("no-match = %q, %v; want nil", noMatch, err)
	}

	if _, err := evalSDK(t, e, `return anicli.regexp.match("(", "x")`); err == nil {
		t.Fatal("an invalid pattern must error")
	}
}

func TestSDKBase64(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `
		local enc = anicli.base64.encode("hi lua")
		return enc .. ":" .. anicli.base64.decode(enc)
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "aGkgbHVh:") || !strings.HasSuffix(got, ":hi lua") {
		t.Fatalf("base64 = %q", got)
	}
	if _, err := evalSDK(t, e, `return anicli.base64.decode("!!not base64!!")`); err == nil {
		t.Fatal("invalid base64 must error")
	}
}

func TestSDKTimeNow(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `return anicli.time.now()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("time.now = %q is not ISO 8601: %v", got, err)
	}
}

func TestSDKLog(t *testing.T) {
	e, _, buf := newSDKEngine(t, http.NotFoundHandler())
	if _, err := evalSDK(t, e, `
		anicli.log.info("an info")
		anicli.log.warn("a warn")
		anicli.log.error("an error")
		return "ok"
	`); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	for _, want := range []string{"an info", "a warn", "an error"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("%q missing from the log: %s", want, logged)
		}
	}
}

func TestSDKVersion(t *testing.T) {
	e, _, _ := newSDKEngine(t, http.NotFoundHandler())
	got, err := evalSDK(t, e, `return anicli.version`)
	if err != nil {
		t.Fatal(err)
	}
	if got != SDKVersion {
		t.Fatalf("anicli.version = %q, want %q", got, SDKVersion)
	}
}
