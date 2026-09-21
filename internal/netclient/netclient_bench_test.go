package netclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// PR81 benchmarks for the shared HTTP client's retry/backoff decision
// path — the code every provider request rides. Offline: loopback
// httptest servers, and the backoff SLEEP is stubbed instant so ns/op
// isolates the decision overhead (bounds + jitter draw + classifier +
// Retry-After parse) from the wall clock it deliberately burns.

var (
	benchSinkDuration time.Duration
	benchSinkBool     bool
	benchSinkResponse *Response
)

// benchClient builds a client whose backoff sleep is an instant stub
// recording the requested delay.
func benchClient(b *testing.B) (*Client, *[]time.Duration) {
	b.Helper()

	c, err := New(config.Default().Network)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	delays := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
	return c, delays
}

// BenchmarkBackoffDecision exercises the full decision path per op:
// backoffBounds + backoffDelay (crypto/rand jitter) + retriableStatus
// + parseRetryAfter — the per-failed-attempt overhead.
func BenchmarkBackoffDecision(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		lo, hi := backoffBounds(3)
		benchSinkDuration = backoffDelay(3)
		benchSinkBool = retriableStatus(http.StatusBadGateway)
		benchSinkDuration += parseRetryAfter("12") + lo + hi
	}
}

// BenchmarkClientDoSuccess measures one full Do round trip against a
// loopback 200 (transport + status mapping, no retry).
func BenchmarkClientDoSuccess(b *testing.B) {
	b.ReportAllocs()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	b.Cleanup(srv.Close)

	c, _ := benchClient(b)
	ctx := context.Background()
	for b.Loop() {
		resp, err := c.Get(ctx, srv.URL+"/search?q=one+piece", nil)
		if err != nil {
			b.Fatalf("do: %v", err)
		}
		benchSinkResponse = resp
	}
}

// BenchmarkClientDoNonRetriable measures the failure-decision path: a
// 404 answer is classified non-retriable and surfaced in one attempt.
func BenchmarkClientDoNonRetriable(b *testing.B) {
	b.ReportAllocs()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	b.Cleanup(srv.Close)

	c, _ := benchClient(b)
	ctx := context.Background()
	for b.Loop() {
		resp, err := c.Get(ctx, srv.URL+"/missing", nil)
		if err == nil {
			b.Fatal("expected 404 to surface as an error")
		}
		benchSinkResponse = resp
	}
}

// BenchmarkClientDoRetriableThenSuccess measures one 503 → retry →
// 200 cycle with the backoff sleep stubbed: ns/op is the retry
// decision overhead (classifier + next-attempt bounds + jitter draw).
func BenchmarkClientDoRetriableThenSuccess(b *testing.B) {
	b.ReportAllocs()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls%2 == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	b.Cleanup(srv.Close)

	c, delays := benchClient(b)
	ctx := context.Background()
	for b.Loop() {
		*delays = (*delays)[:0]
		resp, err := c.Get(ctx, srv.URL+"/flaky", nil)
		if err != nil {
			b.Fatalf("do: %v", err)
		}
		if len(*delays) != 1 {
			b.Fatalf("retried %d times, want exactly 1", len(*delays))
		}
		benchSinkResponse = resp
	}
}
