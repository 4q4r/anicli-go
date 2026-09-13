//go:build load

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Storage load profile: bulk anime_progress upserts at the 10k-rows
// scale, ListHistory read latency over the full table, and a 30s
// concurrent read-while-write stress on the single-writer connection
// pool asserting zero SQLITE_BUSY.

const (
	loadRows         = 10_000
	loadListSamples  = 50
	loadStressWindow = 30 * time.Second
	loadStressReads  = 8

	// loadListP95SLO bounds the p95 ListHistory latency over 10k rows:
	// calibrated at ~2x the measured baseline (full-table ORDER BY scan,
	// 22 columns × 10k rows through modernc sqlite ≈ 120ms p95 on the
	// reference box) — catches structural regressions (index loss,
	// accidental per-row queries) without flaking on driver noise.
	loadListP95SLO = 250 * time.Millisecond
)

// loadRow builds one deterministic anime_progress row.
func loadRow(i int) *AnimeProgress {
	poster := fmt.Sprintf("https://load.example/poster/%d.jpg", i)
	return &AnimeProgress{
		Title:           fmt.Sprintf("Load Anime %d", i),
		Poster:          &poster,
		SourceID:        "load",
		SourceURL:       fmt.Sprintf("https://load.example/anime/%d", i),
		CurrentEpisode:  fmt.Sprint(i%24 + 1),
		ShikimoriID:     ptrInt64(10_000 + int64(i)),
		ShikimoriStatus: []string{"watching", "completed", "planned", "on_hold"}[i%4],
		Score:           i % 11,
		TotalEpisodes:   24,
		UpdatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
	}
}

// openLoadStore opens a real file-backed WAL database in a temp dir
// (exercises the production DSN + busy_timeout path, not :memory:).
func ptrInt64(i int64) *int64 { return &i }

func openLoadStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "load.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open load store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestLoadStorageBulkUpsertAndListHistory bulk-inserts 10k rows, then
// samples ListHistory and asserts the p95 SLO.
func TestLoadStorageBulkUpsertAndListHistory(t *testing.T) {
	store := openLoadStore(t)
	ctx := context.Background()

	upsertStart := time.Now()
	for i := range loadRows {
		if err := store.Progress.Upsert(ctx, loadRow(i)); err != nil {
			t.Fatalf("upsert row %d: %v", i, err)
		}
	}
	upsertWall := time.Since(upsertStart)

	latencies := make([]time.Duration, 0, loadListSamples)
	for range loadListSamples {
		start := time.Now()
		rows, err := store.Progress.ListHistory(ctx, "", 0, 0)
		if err != nil {
			t.Fatalf("list history: %v", err)
		}
		latencies = append(latencies, time.Since(start))
		if len(rows) != loadRows {
			t.Fatalf("list history returned %d rows, want %d", len(rows), loadRows)
		}
	}

	p50, p95, p99 := percentiles(latencies)
	fmt.Fprintf(os.Stdout, "\n=== storage load results (%d rows) ===\n", loadRows)
	fmt.Fprintf(os.Stdout, "bulk upsert\t%d rows\t%s\t%.0f rows/s\n",
		loadRows, upsertWall.Round(time.Millisecond), float64(loadRows)/upsertWall.Seconds())
	fmt.Fprintf(os.Stdout, "ListHistory\tp50 %s\tp95 %s\tp99 %s (SLO p95 < %s)\n\n",
		p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond), loadListP95SLO)

	if p95 >= loadListP95SLO {
		t.Fatalf("SLO violated: ListHistory p95 %s >= %s", p95, loadListP95SLO)
	}
}

// percentiles returns p50/p95/p99 of the (unsorted) input.
func percentiles(values []time.Duration) (time.Duration, time.Duration, time.Duration) {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(p float64) time.Duration {
		return sorted[int(float64(len(sorted)-1)*p)]
	}
	return at(0.50), at(0.95), at(0.99)
}

// TestLoadStorageReadWriteStress runs one writer and eight readers for
// 30 seconds against the single-connection pool and asserts that no
// operation ever surfaces SQLITE_BUSY (busy_timeout 5000 + pool of 1
// must serialize everything).
func TestLoadStorageReadWriteStress(t *testing.T) {
	store := openLoadStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed a baseline table so readers always have work.
	for i := range 1000 {
		if err := store.Progress.Upsert(ctx, loadRow(i)); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(loadStressWindow)
	var mu sync.Mutex
	var busyErrs, otherErrs []string

	var writes, reads atomic.Int64
	var next atomic.Int64

	// Single writer: alternate fresh inserts and updates of early rows
	// (both hit the upsert path).
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for time.Now().Before(deadline) {
			i := int(next.Add(1)) % loadRows
			if err := store.Progress.Upsert(ctx, loadRow(i)); err != nil {
				mu.Lock()
				if strings.Contains(strings.ToUpper(err.Error()), "BUSY") ||
					strings.Contains(strings.ToLower(err.Error()), "locked") {
					busyErrs = append(busyErrs, err.Error())
				} else {
					otherErrs = append(otherErrs, err.Error())
				}
				mu.Unlock()
				return
			}
			writes.Add(1)
		}
	}()

	// Readers: full-history scans + point lookups.
	readerDone := make(chan struct{}, loadStressReads)
	for range loadStressReads {
		go func() {
			defer func() { readerDone <- struct{}{} }()
			for time.Now().Before(deadline) {
				if _, err := store.Progress.ListHistory(ctx, "", 50, 0); err != nil {
					mu.Lock()
					if strings.Contains(strings.ToUpper(err.Error()), "BUSY") ||
						strings.Contains(strings.ToLower(err.Error()), "locked") {
						busyErrs = append(busyErrs, err.Error())
					} else {
						otherErrs = append(otherErrs, err.Error())
					}
					mu.Unlock()
					return
				}
				if _, err := store.Progress.GetByID(ctx, 1+reads.Add(1)%900); err == nil || err.Error() == "not found" {
					// both are healthy outcomes for the rotating probe id
				} else {
					mu.Lock()
					otherErrs = append(otherErrs, err.Error())
					mu.Unlock()
					return
				}
			}
		}()
	}

	<-writerDone
	for range loadStressReads {
		<-readerDone
	}

	wall := loadStressWindow.Round(time.Millisecond)
	fmt.Fprintf(os.Stdout, "=== storage stress (1 writer + %d readers, %s) ===\n", loadStressReads, wall)
	fmt.Fprintf(os.Stdout, "writes\t%d\t%.0f ops/s\nreads\t%d\t%.0f ops/s\nbusy errors\t%d\tother errors\t%d\n\n",
		writes.Load(), float64(writes.Load())/wall.Seconds(),
		reads.Load(), float64(reads.Load())/wall.Seconds(),
		len(busyErrs), len(otherErrs))

	if len(busyErrs) != 0 {
		t.Fatalf("SQLITE_BUSY surfaced %d times under single-writer pool: %v", len(busyErrs), busyErrs[:min(3, len(busyErrs))])
	}
	if len(otherErrs) != 0 {
		t.Fatalf("unexpected storage errors during stress: %v", otherErrs[:min(3, len(otherErrs))])
	}
}
