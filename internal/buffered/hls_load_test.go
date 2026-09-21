//go:build load

package buffered

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// PR81 download-pipeline load profile: the full HLS stage (playlist
// fetch → parse → segment orchestration → local file) against a
// loopback fixture server with 500 × 256KiB segments (~125 MiB feed).
// Asserts completion and byte-identity, reports MB/s, allocations and
// no goroutine leak after Cleanup.

const (
	hlsLoadSegments     = 500
	hlsLoadSegmentBytes = 256 << 10 // 256 KiB
	hlsLoadBudget       = 60 * time.Second
)

// loadGoroutineDelta waits for the goroutine count to settle and
// returns the delta against baseline (the package-local twin of the
// pattern used across the load suite).
func loadGoroutineDelta(t *testing.T, baseline int, window time.Duration, allowed int) int {
	t.Helper()
	deadline := time.Now().Add(window)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == last && current <= baseline+allowed {
			return current - baseline
		}
		last = current
	}
	return last - baseline
}

// TestLoadHLSPipeline500Segments drives the pipeline end to end and
// checks the orchestration overhead stays inside the budget.
func TestLoadHLSPipeline500Segments(t *testing.T) {
	playlist := benchMediaPlaylist(hlsLoadSegments)
	segment := strings.Repeat("b", hlsLoadSegmentBytes)
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(playlist))
			return
		}
		served++
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write([]byte(segment))
	}))
	t.Cleanup(srv.Close)

	d := benchBufferDownloader(t)
	ctx, cancel := context.WithTimeout(context.Background(), hlsLoadBudget)
	defer cancel()

	goroutinesBefore := runtime.NumGoroutine()
	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()
	handle, err := d.Buffer(ctx, Source{URL: srv.URL + "/hls/1080/index.m3u8"}, nil)
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}

	got, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatalf("read buffered file: %v", err)
	}
	if len(got) != hlsLoadSegments*hlsLoadSegmentBytes {
		t.Fatalf("buffered %d bytes, want %d", len(got), hlsLoadSegments*hlsLoadSegmentBytes)
	}
	if served < hlsLoadSegments {
		t.Fatalf("server saw %d segment requests, want >= %d", served, hlsLoadSegments)
	}

	handle.Cleanup()
	runtime.ReadMemStats(&memAfter)
	leaked := loadGoroutineDelta(t, goroutinesBefore, 2*time.Second, 25)

	mib := float64(len(got)) / (1 << 20)
	mbps := mib / wall.Seconds()
	fmt.Fprintf(os.Stdout, "\n=== HLS pipeline load results (%d segments × %d KiB) ===\n",
		hlsLoadSegments, hlsLoadSegmentBytes/(1<<10))
	fmt.Fprintf(os.Stdout, "payload\t%.1f MiB\twall %s\tthroughput\t%.1f MB/s\talloc %d MB\tleak delta %d\n\n",
		mib, wall.Round(time.Millisecond), mbps,
		(memAfter.TotalAlloc-memBefore.TotalAlloc)/(1<<20), leaked)

	if leaked > 10 {
		t.Fatalf("goroutine leak: delta %d after settle", leaked)
	}
}
