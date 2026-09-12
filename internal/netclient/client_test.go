package netclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// testConfig builds a config.Network with user-tuned defaults and no proxy.
func testConfig() config.Network {
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	return cfg
}

// newTestClient builds a client with an injected instant sleeper that
// records requested delays, keeping retry tests deterministic and fast.
func newTestClient(t *testing.T, cfg config.Network, opts ...Option) (*Client, *delayRecorder) {
	t.Helper()

	c, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &delayRecorder{}
	c.sleep = rec.sleep
	return c, rec
}

type delayRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *delayRecorder) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delays = append(r.delays, d)
	return nil
}

func (r *delayRecorder) snapshot() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.delays...)
}

// writeBody writes a handler body, ignoring write errors on aborted
// connections (the client hung up during the test).
func writeBody(w http.ResponseWriter, body string) {
	_, _ = fmt.Fprint(w, body)
}

func TestDoRetriesOn503ThenSucceeds(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeBody(w, "try later")
			return
		}
		writeBody(w, "hello")
	}))
	defer srv.Close()

	c, rec := newTestClient(t, testConfig())
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(resp.Body) != "hello" {
		t.Errorf("body = %q, want %q", resp.Body, "hello")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2 (one retry)", got)
	}
	if delays := rec.snapshot(); len(delays) != 1 {
		t.Errorf("backoff sleeps = %v, want exactly one", delays)
	}
}

func TestDoHonorsRetryAfterHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// header value returned with the retriable status
		header string
		// want is the expected recorded sleep delay
		want time.Duration
	}{
		{name: "seconds form", header: "2", want: 2 * time.Second},
		{name: "capped at 10s", header: "60", want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					w.Header().Set("Retry-After", tt.header)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				writeBody(w, "ok")
			}))
			defer srv.Close()

			c, rec := newTestClient(t, testConfig())
			if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL}); err != nil {
				t.Fatalf("Do: %v", err)
			}
			delays := rec.snapshot()
			if len(delays) != 1 {
				t.Fatalf("sleeps = %v, want one", delays)
			}
			if delays[0] != tt.want {
				t.Errorf("sleep = %v, want %v (Retry-After %q)", delays[0], tt.want, tt.header)
			}
		})
	}
}

func TestDoNoRetryOn403(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig(), WithProvider("anilib"))
	_, err := c.Do(context.Background(), Request{
		Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch,
	})
	if err == nil {
		t.Fatal("403 must fail")
	}
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Errorf("err = %v, want contracts.ErrProvider403", err)
	}
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err must be a *contracts.ProviderError, got %T: %v", err, err)
	}
	if pe.Provider != "anilib" || pe.Op != contracts.OpSearch || pe.StatusCode != 403 {
		t.Errorf("ProviderError = %+v, want {anilib %s 403}", pe, contracts.OpSearch)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (no retry on 403)", got)
	}
}

func TestDoMaps404ToNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL})
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("err = %v, want contracts.ErrNotFound", err)
	}
}

func TestDoMapsNonRetriableStatusToStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418: neither retriable nor mapped
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err must wrap *StatusError, got %T: %v", err, err)
	}
	if se.StatusCode != http.StatusTeapot {
		t.Errorf("StatusError.StatusCode = %d, want 418", se.StatusCode)
	}
}

func TestDoRespectsContextCancelWithoutRetry(t *testing.T) {
	// Uses real timing on purpose: the handler blocks until the client
	// aborts; no t.Parallel to keep wall time predictable.
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	c, _ := newTestClient(t, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := c.Do(ctx, Request{Method: http.MethodGet, URL: srv.URL})
	if err == nil {
		t.Fatal("cancelled context must fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (no retry after caller cancel)", got)
	}
}

func TestDoTimeoutMapsToProviderTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeBody(w, "late")
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.RequestTimeout = 50 * time.Millisecond
	c, _ := newTestClient(t, cfg)

	before := time.Now()
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	elapsed := time.Since(before)

	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("err = %v, want contracts.ErrProviderTimeout", err)
	}
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) || pe.Provider != "" || pe.Op != contracts.OpSearch {
		t.Errorf("err = %v, want a ProviderError with op context", err)
	}
	// Instant sleeper: the 3 attempt deadlines (50ms each against a
	// 200ms handler) dominate the wall time.
	if elapsed > 2*time.Second {
		t.Errorf("Do took %v with injected sleeps; attempt budget looks wrong", elapsed)
	}
}

func TestDoBodyLimitEnforced(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig(), WithBodyLimit(1024))
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL})
	if !errors.Is(err, ErrBodyLimit) {
		t.Fatalf("err = %v, want ErrBodyLimit", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (body limit is not retriable)", got)
	}
}

func TestDoSetsUserAgentAndLanguage(t *testing.T) {
	t.Parallel()

	var gotUA, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotLang = r.Header.Get("Accept-Language")
	}))
	defer srv.Close()

	cfg := testConfig()
	c, _ := newTestClient(t, cfg)
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotUA != cfg.UserAgent {
		t.Errorf("User-Agent = %q, want configured %q", gotUA, cfg.UserAgent)
	}
	if want := "ru-RU,ru;q=0.9"; len(gotLang) < len(want) || gotLang[:len(want)] != want {
		t.Errorf("Accept-Language = %q, want it to start with %q (python network.py L42)", gotLang, want)
	}
}

func TestDoRequestHeadersOverrideDefaults(t *testing.T) {
	t.Parallel()

	var gotUA, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCustom = r.Header.Get("X-Custom")
	}))
	defer srv.Close()

	cfg := testConfig()
	c, _ := newTestClient(t, cfg)
	_, err := c.Do(context.Background(), Request{
		Method:  http.MethodGet,
		URL:     srv.URL,
		Headers: map[string]string{"User-Agent": "anicli-ru", "X-Custom": "v"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotUA != "anicli-ru" {
		t.Errorf("per-request User-Agent = %q, want override to win", gotUA)
	}
	if gotCustom != "v" {
		t.Errorf("X-Custom = %q, want %q", gotCustom, "v")
	}
}

func TestGetPostJSONPostForm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(c *Client, url string) (*Response, error)
		// expectations observed server-side
		wantMethod string
		wantCT     string
		wantBody   string
	}{
		{
			name: "Get",
			run: func(c *Client, target string) (*Response, error) {
				return c.Get(context.Background(), target, nil)
			},
			wantMethod: http.MethodGet,
		},
		{
			name: "PostJSON",
			run: func(c *Client, target string) (*Response, error) {
				return c.PostJSON(context.Background(), target, map[string]any{"q": "naruto"}, nil)
			},
			wantMethod: http.MethodPost,
			wantCT:     "application/json",
			wantBody:   `{"q":"naruto"}`,
		},
		{
			name: "PostForm",
			run: func(c *Client, target string) (*Response, error) {
				return c.PostForm(context.Background(), target, url.Values{"q": {"naruto"}}, nil)
			},
			wantMethod: http.MethodPost,
			wantCT:     "application/x-www-form-urlencoded",
			wantBody:   "q=naruto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotCT, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf := make([]byte, 128)
				n, _ := r.Body.Read(buf)
				gotMethod, gotCT, gotBody = r.Method, r.Header.Get("Content-Type"), string(buf[:n])
				writeBody(w, "ok")
			}))
			defer srv.Close()

			c, _ := newTestClient(t, testConfig())
			resp, err := tt.run(c, srv.URL)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if string(resp.Body) != "ok" {
				t.Errorf("body = %q, want ok", resp.Body)
			}
			if gotMethod != tt.wantMethod {
				t.Errorf("method = %s, want %s", gotMethod, tt.wantMethod)
			}
			if tt.wantCT != "" && gotCT != tt.wantCT {
				t.Errorf("content-type = %s, want %s", gotCT, tt.wantCT)
			}
			if tt.wantBody != "" && gotBody != tt.wantBody {
				t.Errorf("body sent = %q, want %q", gotBody, tt.wantBody)
			}
		})
	}
}

func TestPostJSONResultDecodable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeBody(w, `{"status":"ok","count":3}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	resp, err := c.PostJSON(context.Background(), srv.URL, map[string]any{"q": 1}, nil)
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Status != "ok" || payload.Count != 3 {
		t.Errorf("decoded = %+v", payload)
	}
}

func TestProxyOptionConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "no proxy", proxy: ""},
		{name: "http proxy", proxy: "http://127.0.0.1:10809"},
		{name: "socks5 proxy", proxy: "socks5://127.0.0.1:9050"},
		{name: "garbage proxy fails loud", proxy: "://not-a-url", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig()
			cfg.ProxyURL = tt.proxy
			c, err := New(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("New must reject an invalid proxy URL")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c.http == nil {
				t.Fatal("client must wrap a tls-client instance")
			}
		})
	}
}

func TestBackoffEqualJitterBounds(t *testing.T) {
	t.Parallel()

	for attempt := range 8 {
		lo, hi := backoffBounds(attempt)
		for range 200 {
			d := backoffDelay(attempt)
			if d < lo || d > hi {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v]", attempt, d, lo, hi)
			}
		}
	}
	// base 500ms doubling, capped at 5s
	if lo, hi := backoffBounds(0); lo != 250*time.Millisecond || hi != 500*time.Millisecond {
		t.Errorf("backoff(0) bounds = [%v, %v], want [250ms, 500ms]", lo, hi)
	}
	if lo, hi := backoffBounds(10); lo != 2500*time.Millisecond || hi != 5*time.Second {
		t.Errorf("backoff(10) bounds = [%v, %v], want [2.5s, 5s] (cap)", lo, hi)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   time.Duration
	}{
		{header: "2", want: 2 * time.Second},
		{header: "0", want: 0},
		{header: "-5", want: 0},
		{header: "soon", want: 0},
		{header: "", want: 0},
		{header: "9223372036854775808", want: 0}, // overflow
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.header); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestRetriableStatuses(t *testing.T) {
	t.Parallel()

	for _, code := range []int{408, 425, 429, 500, 502, 503, 504} {
		if !retriableStatus(code) {
			t.Errorf("retriableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 301, 400, 401, 403, 404, 418, 501} {
		if retriableStatus(code) {
			t.Errorf("retriableStatus(%d) = true, want false", code)
		}
	}
}

func TestRetryBudgetStopsAtThreeAttempts(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway) // always retriable
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL})
	if err == nil {
		t.Fatal("persistent 502 must fail")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 502 {
		t.Errorf("err = %v, want StatusError 502 after exhausted retries", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3 (max attempts)", got)
	}
}

// TestDoFinalURLTracksRedirects pins the redirect-following behavior the
// kwik extractor depends on: a POST whose response redirects must surface
// the post-redirect URL of the final response, so callers can recover the
// media Location, and the redirect-followed request must keep the
// caller-supplied headers (Referer parity with Python cloudscraper).
func TestDoFinalURLTracksRedirects(t *testing.T) {
	t.Parallel()

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Referer"); got != "https://kwik.cx/" {
			t.Errorf("redirected request Referer = %q, want the caller header to persist", got)
		}
		writeBody(w, "#EXTM3U\n")
	}))
	defer final.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/playlist.m3u8", http.StatusFound) //nolint:gosec // test-owned redirect target
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	resp, err := c.Do(context.Background(), Request{
		Method:  http.MethodPost,
		URL:     srv.URL + "/dl",
		Headers: map[string]string{"Referer": "https://kwik.cx/"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if want := final.URL + "/playlist.m3u8"; resp.FinalURL != want {
		t.Errorf("FinalURL = %q, want %q", resp.FinalURL, want)
	}
}

// TestDoFinalURLEqualsURLWithoutRedirect pins the non-redirect case: a
// plain 200 reports the request URL itself.
func TestDoFinalURLEqualsURLWithoutRedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeBody(w, "ok")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, testConfig())
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL + "/page"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.FinalURL != srv.URL+"/page" {
		t.Errorf("FinalURL = %q, want the request URL", resp.FinalURL)
	}
}
