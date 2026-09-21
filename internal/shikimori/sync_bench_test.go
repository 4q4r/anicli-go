package shikimori

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"log/slog"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
)

// PR81 benchmark for the startup two-way sync merge semantics: the
// pull phase over a 200-row remote rate list (a large but realistic
// user roster), every local row bound — measures the merge + storage
// round-trip cost the startup sync screen rides. Offline: loopback
// httptest + instant rate-limiter sleep.

// benchWriteJSON writes one JSON response.
func benchWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// benchRatesPayload renders n user_rates rows (targets 500.., ids 900..).
func benchRatesPayload(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	statuses := []string{"watching", "completed", "planned", "on_hold", "dropped"}
	for i := range n {
		out = append(out, map[string]any{
			"id": 900 + i, "user_id": 50, "target_id": 500 + i, "target_type": "Anime",
			"status": statuses[i%len(statuses)], "episodes": i % 12, "score": i % 10, "rewatches": i % 3,
		})
	}
	return out
}

// benchSyncServer serves the cookie-mode plumbing plus a 200-row rate
// list; POST/PUT user_rates answer with a created/updated row.
func benchSyncServer(b *testing.B, rates []map[string]any) *httptest.Server {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			benchWriteJSON(w, map[string]any{"id": 50})
		case "/api/v2/user_rates":
			if r.Method == http.MethodGet {
				benchWriteJSON(w, rates)
				return
			}
			benchWriteJSON(w, rates[0])
		case "/api/animes":
			// Batched metadata: answer the requested ids with stubs.
			q := r.URL.Query().Get("ids")
			_ = q
			benchWriteJSON(w, []map[string]any{{"id": 1, "episodes": 12}})
		default:
			benchWriteJSON(w, rates[0])
		}
	}))
	b.Cleanup(srv.Close)
	return srv
}

// benchSyncFixture builds the b-variant of the sync test fixture:
// cookie-mode client, instant limiter, in-memory store seeded with n
// locally-bound rows matching the remote rate targets.
func benchSyncFixture(b *testing.B, n int) (*Syncer, *storage.Store) {
	b.Helper()

	srv := benchSyncServer(b, benchRatesPayload(n))

	net, err := netclient.New(config.Default().Network)
	if err != nil {
		b.Fatalf("netclient: %v", err)
	}
	cfg := config.Shikimori{Enabled: true, Session: "bench"}
	c := New(cfg, net, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	// Instant limiter: the fake clock advances on every read so the
	// sliding window admits instantly without real stalls.
	clock := &benchClock{}
	c.lim.now = clock.Now
	c.lim.sleep = func(_ context.Context, _ time.Duration) error {
		clock.advance(time.Second)
		return nil
	}

	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	statuses := []string{"watching", "completed", "planned", "on_hold", "dropped"}
	for i := range n {
		shikiID := 500 + int64(i)
		rec := storage.AnimeProgress{
			Title: "Bench " + itoaB(i), SourceID: "animego",
			SourceURL:       "https://animego.example/" + itoaB(i),
			CurrentEpisode:  itoaB(i % 12),
			ShikimoriID:     &shikiID,
			ShikimoriStatus: statuses[i%len(statuses)],
		}
		if err := st.Progress.Upsert(context.Background(), &rec); err != nil {
			b.Fatalf("seed row %d: %v", i, err)
		}
	}
	return NewSyncer(c, st.Progress, nil), st
}

func itoaB(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// benchClock is a fake monotonic clock: advances on demand so the
// limiter's sliding window never stalls the benchmark.
type benchClock struct{ cur time.Time }

func (c *benchClock) Now() time.Time {
	if c.cur.IsZero() {
		c.cur = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return c.cur
}

func (c *benchClock) advance(d time.Duration) { c.cur = c.Now().Add(d) }

// BenchmarkSyncFullPullMerge200 runs the full two-way sync against a
// 200-rate remote roster with every local row bound: the pull-merge
// semantics (status/rate-id/score merge, forward-only episodes) over
// the real sqlite store.
func BenchmarkSyncFullPullMerge200(b *testing.B) {
	b.ReportAllocs()
	syncer, _ := benchSyncFixture(b, 200)
	ctx := context.Background()
	for b.Loop() {
		result, err := syncer.SyncFull(ctx, nil)
		if err != nil {
			b.Fatalf("sync full: %v", err)
		}
		benchSinkUpdated = result.Updated
	}
}

var benchSinkUpdated int

// BenchmarkSyncPayloadBuild measures the per-row push payload build
// (RateInput) — the shape pushed on every dirty replay.
func BenchmarkSyncPayloadBuild(b *testing.B) {
	b.ReportAllocs()
	episode := 7
	var sink *RateInput
	for b.Loop() {
		sink = &RateInput{Episodes: &episode, Status: "watching"}
	}
	benchSinkRate = sink
}

var benchSinkRate *RateInput
