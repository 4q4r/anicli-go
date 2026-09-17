package netclient

// Regression tests for the no-first-byte watchdog (ConnectTimeout).
//
// The incident (PR36 follow-up, controller-reproduced 2026-09-17):
// `parity search nyaa --proxy …` hard-failed with "context deadline
// exceeded" on nyaa.si RSS. Live probing showed the proxied route
// intermittently drops ~30-50% of FRESH connections into total silence
// AFTER TCP+CONNECT+TLS+request-write (DDoS-Guard soft-tarpit of the
// exit IP; curl, stdlib net/http and tls-client all hang identically;
// a healthy answer takes ~0.5s). Each fresh connection is an
// independent coin roll, so a retry on a NEW connection recovers —
// but Do's retry policy was structurally dead: the per-attempt budget
// equalled the caller's whole-op budget in every production caller,
// so one silent attempt burned the entire budget and no retry ever
// happened.
//
// The contract pinned here: ConnectTimeout bounds how long an attempt
// may produce NO response headers (silent connection). Such attempts
// fail fast, Do retries dial a fresh connection, and the full
// RequestTimeout still governs the body of a proven-alive response.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// silentOnceServer returns a server whose first hit never responds
// (the tarpit) and whose later hits answer immediately. The cleanup
// releases the blocked handlers.
func silentOnceServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			<-block // tarpit: accept, never answer
			return
		}
		writeBody(w, "recovered")
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})
	return srv
}

// TestDoWatchdogFastFailsSilentAttemptAndRetries is the incident
// regression: attempt #1 goes silent, the watchdog must abandon it
// within ConnectTimeout — NOT within RequestTimeout — and the retry on
// a fresh connection must recover.
func TestDoWatchdogFastFailsSilentAttemptAndRetries(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RequestTimeout = 2 * time.Second
	cfg.ConnectTimeout = 200 * time.Millisecond
	var hits atomic.Int32
	srv := silentOnceServer(t, &hits)

	c, _ := newTestClient(t, cfg)
	start := time.Now()
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(resp.Body) != "recovered" {
		t.Errorf("body = %q, want %q", resp.Body, "recovered")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2 (one silent attempt + one retry)", got)
	}
	// The watchdog, not the request timeout, must bound the silent
	// attempt: generous upper bound well below RequestTimeout keeps
	// the assertion stable on a loaded CI runner.
	if elapsed >= cfg.RequestTimeout {
		t.Errorf("Do took %v (>= RequestTimeout %v): the silent attempt was not fast-failed by ConnectTimeout",
			elapsed.Round(time.Millisecond), cfg.RequestTimeout)
	}
}

// TestDoWatchdogAllAttemptsSilentFailsTyped pins the exhausted case:
// every attempt silent → exactly maxAttempts attempts, each bounded by
// the watchdog, ending in the typed provider-timeout error (not a
// caller-context deadline — the caller had no deadline).
func TestDoWatchdogAllAttemptsSilentFailsTyped(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RequestTimeout = 3 * time.Second
	cfg.ConnectTimeout = 150 * time.Millisecond
	block := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	c, _ := newTestClient(t, cfg)
	start := time.Now()
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Do: want error, got nil")
	}
	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Errorf("err = %v, want wrapped contracts.ErrProviderTimeout", err)
	}
	if got := hits.Load(); got != maxAttempts {
		t.Errorf("server hits = %d, want %d", got, maxAttempts)
	}
	// Three watchdog-bounded attempts (~450ms) plus backoffs — far
	// below the 3×3s the pre-fix code needed.
	if elapsed >= 2*time.Second {
		t.Errorf("Do took %v: attempts were not watchdog-bounded", elapsed.Round(time.Millisecond))
	}
}

// TestDoWatchdogDoesNotCutSlowBody pins the TTFB-only scope: once
// response headers arrived inside the budget the connection is proven
// alive, and a body that trickles in slower than ConnectTimeout must
// still be read to completion under the full RequestTimeout.
func TestDoWatchdogDoesNotCutSlowBody(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RequestTimeout = 2 * time.Second
	cfg.ConnectTimeout = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			panic("httptest ResponseWriter cannot flush")
		}
		// Headers flush immediately; the body then trickles in
		// chunks spaced WELL beyond ConnectTimeout.
		for i := range 4 {
			_, _ = fmt.Fprintf(w, "chunk-%d,", i)
			flusher.Flush()
			time.Sleep(2 * cfg.ConnectTimeout)
		}
	}))
	defer srv.Close()

	c, _ := newTestClient(t, cfg)
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	want := "chunk-0,chunk-1,chunk-2,chunk-3,"
	if string(resp.Body) != want {
		t.Errorf("body = %q, want %q", resp.Body, want)
	}
}

// TestDoWatchdogDisabledWhenConnectTimeoutZero pins the off switch:
// ConnectTimeout <= 0 keeps the pre-watchdog semantics (no TTFB bound,
// only RequestTimeout applies).
func TestDoWatchdogDisabledWhenConnectTimeoutZero(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RequestTimeout = 2 * time.Second
	cfg.ConnectTimeout = 0
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond) // beyond any watchdog, inside RequestTimeout
		writeBody(w, "slow but alive")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, cfg)
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL, Op: contracts.OpSearch})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(resp.Body) != "slow but alive" {
		t.Errorf("body = %q", resp.Body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (watchdog disabled, no retry)", got)
	}
}

// configDefaultConnectTimeout documents that the shipped default keeps
// the watchdog armed: the config default must stay > 0.
func TestConfigDefaultConnectTimeoutArmsWatchdog(t *testing.T) {
	t.Parallel()

	if d := config.Default().Network.ConnectTimeout; d <= 0 {
		t.Errorf("config default ConnectTimeout = %v, want > 0 (watchdog armed by default)", d)
	}
}

// TestNewNormalizesConnectTimeout pins the clamp: a ConnectTimeout at
// or above RequestTimeout could never fire before the per-attempt
// deadline, silently disabling the fast-fail — New normalizes it onto
// RequestTimeout instead.
func TestNewNormalizesConnectTimeout(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RequestTimeout = 5 * time.Second
	cfg.ConnectTimeout = 30 * time.Second

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.cfg.ConnectTimeout; got != cfg.RequestTimeout {
		t.Errorf("ConnectTimeout = %v, want normalized to RequestTimeout %v", got, cfg.RequestTimeout)
	}
	if c.cfg.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout = %v, want untouched", c.cfg.RequestTimeout)
	}
}

// stubHTTP replaces the tls-client transport so the worker's result can
// be scheduled deterministically around the watchdog boundary — the
// deadlock under test lives in a scheduler-timing window that real
// httptest traffic only crosses by luck.
type stubHTTP struct {
	tls_client.HttpClient // embedded nil: only Do is ever called
	do                    func(*fhttp.Request) (*fhttp.Response, error)
}

func (s *stubHTTP) Do(r *fhttp.Request) (*fhttp.Response, error) { return s.do(r) }

// TestDoWatchdogReturnsWhenAttemptErrorsJustPastBudget pins the
// deadlock race at the watchdog boundary: when the worker's own error
// lands a hair AFTER the budget elapses, the timer branch must return
// that error instead of falling through to a blocking drain that waits
// for a second channel send that never comes (the worker sends exactly
// once). Review-blocker regression, reproduced with the error scheduled
// ε past the timer across a sweep of ε values and repeats — every call
// must RETURN, never hang.
func TestDoWatchdogReturnsWhenAttemptErrorsJustPastBudget(t *testing.T) {
	t.Parallel()

	const (
		budget = 30 * time.Millisecond
		guard  = 2 * time.Second
		reps   = 8
	)

	cfg := testConfig()
	cfg.RequestTimeout = 80 * time.Millisecond // attempt ctx dies LATER than the watchdog
	cfg.ConnectTimeout = budget
	c, _ := newTestClient(t, cfg)

	delays := []time.Duration{0, 100 * time.Microsecond, 500 * time.Microsecond, 1 * time.Millisecond, 5 * time.Millisecond}
	for _, delay := range delays {
		for rep := range reps {
			errBoom := fmt.Errorf("boom %v %d", delay, rep)
			c.http = &stubHTTP{do: func(*fhttp.Request) (*fhttp.Response, error) {
				time.Sleep(budget + delay)
				return nil, errBoom
			}}

			actx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
			freq, err := fhttp.NewRequestWithContext(actx, fhttp.MethodGet, "http://stub.invalid/", nil)
			if err != nil {
				cancel()
				t.Fatalf("build request: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := c.doWithWatchdog(freq, cancel)
				done <- err
			}()

			select {
			case <-done:
				// Returned: the only contract under test. Either the
				// worker's own error (race won) or the watchdog error
				// (drain path) is a valid outcome.
			case <-time.After(guard):
				cancel()
				t.Fatalf("delay %v rep %d: doWithWatchdog did not return within %v: watchdog deadlock (worker send consumed, then blocked on drain)", delay, rep, guard)
			}
			cancel()
		}
	}
}
