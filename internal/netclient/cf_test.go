package netclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// challengeHandler answers with a Cloudflare challenge exactly
// `challenges` times, then 200; every response is recorded with the
// request's Cookie and User-Agent headers.
type challengeHandler struct {
	mu          sync.Mutex
	challenges  atomic.Int64
	served      []servedRequest
	uaOnSuccess string
}

type servedRequest struct {
	cookies string
	ua      string
}

func (h *challengeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cookieHdr := ""
	for _, c := range r.Cookies() {
		if c.Name == "cf_clearance" {
			cookieHdr = c.Value
		}
	}
	h.mu.Lock()
	h.served = append(h.served, servedRequest{cookies: cookieHdr, ua: r.Header.Get("User-Agent")})
	h.mu.Unlock()

	if h.challenges.Add(-1) >= 0 {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><head><title>Just a moment...</title>
<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script></head>`))
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<html><body>real content</body></html>"))
}

func (h *challengeHandler) requests() []servedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]servedRequest(nil), h.served...)
}

// fakeCFSolver counts solves and hands out a fixed clearance.
type fakeCFSolver struct {
	solves      atomic.Int64
	invalidated atomic.Int64
	fail        bool
}

func (f *fakeCFSolver) SolveChallenge(_ context.Context, _ string) (CFClearance, error) {
	f.solves.Add(1)
	if f.fail {
		return CFClearance{}, errors.New("solve exploded")
	}
	return CFClearance{
		Cookies: []CFCookie{
			{Name: "cf_clearance", Value: "clr-777", Domain: "", Path: "/"},
		},
		UserAgent:      "StealthUA/146.0",
		AcceptLanguage: "ru-RU,ru;q=0.9",
	}, nil
}

func (f *fakeCFSolver) InvalidateHost(string) { f.invalidated.Add(1) }

func newCFTestClient(t *testing.T, solver CFSolver) *Client {
	t.Helper()
	cfg := config.Default().Network
	cfg.RequestTimeout = 5 * time.Second
	client, err := New(cfg, WithProvider("cf-test"), WithCFSolver(solver))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestChallengeSolvedOnceCookiesAndUAApplied(t *testing.T) {
	handler := &challengeHandler{}
	handler.challenges.Store(1) // challenge once, then clear
	srv := httptest.NewServer(handler)
	defer srv.Close()

	solver := &fakeCFSolver{}
	client := newCFTestClient(t, solver)

	resp, err := client.Get(context.Background(), srv.URL+"/page", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after solve", resp.StatusCode)
	}
	if got := solver.solves.Load(); got != 1 {
		t.Errorf("exactly one solve expected, got %d", got)
	}
	if got := solver.invalidated.Load(); got != 1 {
		t.Errorf("stale clearance must be invalidated (refresh-on-403), got %d", got)
	}
	reqs := handler.requests()
	if len(reqs) != 2 {
		t.Fatalf("exactly two requests (challenge + retried) expected, got %d", len(reqs))
	}
	if reqs[1].cookies != "clr-777" {
		t.Errorf("retried request must carry cf_clearance, got %q", reqs[1].cookies)
	}
	if reqs[1].ua != "StealthUA/146.0" {
		t.Errorf("retried request must use the clearance UA, got %q", reqs[1].ua)
	}
	if reqs[0].ua == "StealthUA/146.0" {
		t.Errorf("first request must use the configured UA, got %q", reqs[0].ua)
	}
}

func TestChallengeWithoutSolverTypedError(t *testing.T) {
	handler := &challengeHandler{}
	handler.challenges.Store(1 << 30) // never clears
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newCFTestClient(t, nil)
	_, err := client.Get(context.Background(), srv.URL+"/page", nil)
	if err == nil {
		t.Fatal("expected challenge error")
	}
	var cfErr *CFChallengeError
	if !errors.As(err, &cfErr) {
		t.Fatalf("want *CFChallengeError, got %T: %v", err, err)
	}
	// Provider context must survive the taxonomy wrap.
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("challenge error must stay wrapped in the taxonomy: %v", err)
	}
}

func TestPlain403PassesThroughUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body>boring access denied</body></html>"))
	}))
	defer srv.Close()

	solver := &fakeCFSolver{}
	client := newCFTestClient(t, solver)
	_, err := client.Get(context.Background(), srv.URL+"/page", nil)
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("plain 403 must map onto ErrProvider403 as before, got %v", err)
	}
	if solver.solves.Load() != 0 {
		t.Errorf("plain 403 must never trigger a solve, got %d", solver.solves.Load())
	}
}

func TestChallenge503BodyDetected(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<title>Just a moment...</title>
<div class="cf-turnstile" data-sitekey="x"></div>`))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	solver := &fakeCFSolver{}
	client := newCFTestClient(t, solver)
	resp, err := client.Get(context.Background(), srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if solver.solves.Load() != 1 {
		t.Errorf("503 challenge must trigger exactly one solve, got %d", solver.solves.Load())
	}
}

func TestSolverFailureSurfacesTyped(t *testing.T) {
	handler := &challengeHandler{}
	handler.challenges.Store(1 << 30)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	solver := &fakeCFSolver{fail: true}
	client := newCFTestClient(t, solver)
	_, err := client.Get(context.Background(), srv.URL+"/page", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "solve exploded") {
		t.Errorf("solver failure must surface: %v", err)
	}
	var cfErr *CFChallengeError
	if !errors.As(err, &cfErr) {
		t.Fatalf("solver failure must still be typed as challenge: %T %v", err, err)
	}
}

func TestDetectCFChallenge(t *testing.T) {
	html := stdhttp.Header{"Content-Type": []string{"text/html; charset=utf-8"}}
	cases := []struct {
		name string
		resp *Response
		want bool
	}{
		{"403 + cf-mitigated header", &Response{StatusCode: 403, Header: stdhttp.Header{"Cf-Mitigated": []string{"challenge"}}}, true},
		{"403 + body marker", &Response{StatusCode: 403, Header: html, Body: []byte(`<script>challenge-platform</script>`)}, true},
		{"503 + body marker html", &Response{StatusCode: 503, Header: html, Body: []byte(`cf-chl-widget`)}, true},
		{"503 + marker but json", &Response{StatusCode: 503, Header: stdhttp.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"e":"challenge-platform"}`)}, false},
		{"403 plain html", &Response{StatusCode: 403, Header: html, Body: []byte("<html>denied</html>")}, false},
		{"200 never challenge", &Response{StatusCode: 200, Header: html, Body: []byte(`challenge-platform`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectCFChallenge(tc.resp); got != tc.want {
				t.Errorf("detectCFChallenge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChallengeOnlySingleRetry(t *testing.T) {
	handler := &challengeHandler{}
	handler.challenges.Store(1 << 30) // stays challenged
	srv := httptest.NewServer(handler)
	defer srv.Close()

	solver := &fakeCFSolver{}
	client := newCFTestClient(t, solver)
	_, err := client.Get(context.Background(), srv.URL+"/page", nil)
	if err == nil {
		t.Fatal("expected challenge error after retry stays challenged")
	}
	reqs := handler.requests()
	// 3 policy attempts + 1 challenge retry must not spiral: the
	// challenge branch bypasses the generic retry loop, so the
	// challenge response itself must not be retried three times.
	if len(reqs) > 4 {
		t.Errorf("challenge must trigger at most one extra retry, got %d requests", len(reqs))
	}
	if fmt.Sprint(solver.solves.Load()) != "1" {
		t.Errorf("one solve per Do even when retry stays challenged, got %d", solver.solves.Load())
	}
}
