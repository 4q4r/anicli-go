package storage

import (
	"context"
	"testing"
)

// PR81-review before/after evidence for the /history N+1 P0: the old
// handleHistoryList shape (ListHistory + one ListByAnime per row) vs
// the batched shape (ListHistory + one ListAll grouped in Go) over the
// same 1000-row table.

// historyBenchRow builds one deterministic history row (local twin of
// the load-suite loadRow, which is behind the load build tag).
func historyBenchRow(i int) *AnimeProgress {
	poster := "https://bench.example/poster/" + itoaStorageBench(i) + ".jpg"
	return &AnimeProgress{
		Title:          "Bench History Anime " + itoaStorageBench(i),
		Poster:         &poster,
		SourceID:       "load",
		SourceURL:      "https://bench.example/anime/" + itoaStorageBench(i),
		CurrentEpisode: itoaStorageBench(i%24 + 1),
		TotalEpisodes:  24,
		Score:          i % 11,
	}
}

func itoaStorageBench(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// historyBenchStore seeds n history rows with two sources each.
func historyBenchStore(b *testing.B, n int) *Store {
	b.Helper()

	store, err := Open(context.Background(), ":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	for i := range n {
		rec := historyBenchRow(i)
		if err := store.Progress.Upsert(ctx, rec); err != nil {
			b.Fatalf("upsert row %d: %v", i, err)
		}
		row, err := store.Progress.GetBySource(ctx, rec.SourceID, rec.SourceURL)
		if err != nil {
			b.Fatalf("get row %d: %v", i, err)
		}
		if err := store.Sources.ReplaceForAnime(ctx, row.ID, []AnimeSource{
			{SourceID: "load", SourceURL: rec.SourceURL},
			{SourceID: "load2", SourceURL: rec.SourceURL + "#alt"},
		}); err != nil {
			b.Fatalf("replace sources %d: %v", i, err)
		}
	}
	return store
}

// BenchmarkHistoryListNPlusOne1000 is the BEFORE shape: per-row source
// queries (1000 rows → 1001 statements per op).
func BenchmarkHistoryListNPlusOne1000(b *testing.B) {
	b.ReportAllocs()
	store := historyBenchStore(b, 1000)
	ctx := context.Background()
	for b.Loop() {
		records, err := store.Progress.ListHistory(ctx, "", 0, 0)
		if err != nil {
			b.Fatalf("list history: %v", err)
		}
		for i := range records {
			if _, err := store.Sources.ListByAnime(ctx, records[i].ID); err != nil {
				b.Fatalf("list sources: %v", err)
			}
		}
	}
}

// BenchmarkHistoryListBatched1000 is the AFTER shape: two statements
// total, sources grouped in memory.
func BenchmarkHistoryListBatched1000(b *testing.B) {
	b.ReportAllocs()
	store := historyBenchStore(b, 1000)
	ctx := context.Background()
	for b.Loop() {
		records, err := store.Progress.ListHistory(ctx, "", 0, 0)
		if err != nil {
			b.Fatalf("list history: %v", err)
		}
		grouped, err := store.Sources.ListAll(ctx)
		if err != nil {
			b.Fatalf("list all sources: %v", err)
		}
		benchSinkSources = len(grouped)
		if len(records) != 1000 {
			b.Fatalf("records = %d", len(records))
		}
	}
}

var benchSinkSources int
