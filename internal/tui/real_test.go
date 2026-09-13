package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/storage"
)

// TestRealDepsConstruction: the production wiring builds every service
// from settings + an open store without touching the network, and the
// provider list matches the registry order.
func TestRealDepsConstruction(t *testing.T) {
	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	settings := config.Default()
	settings.Download.Dir = t.TempDir() // isolate the offline scan
	real, err := NewRealDeps(settings, store)
	if err != nil {
		t.Fatalf("NewRealDeps: %v", err)
	}
	defer real.Close()

	if real.Deps == nil || real.Deps.Search == nil || real.Deps.Episode == nil ||
		real.Deps.Playback == nil || real.Deps.History == nil || real.Deps.Offline == nil ||
		real.Deps.Database == nil || real.Deps.Health == nil || real.Deps.Shiki == nil ||
		real.Deps.Download == nil {
		t.Fatalf("all services must be wired")
	}

	providers := real.Deps.Search.Providers()
	if len(providers) < 11 {
		t.Fatalf("expected the full provider wave, got %d", len(providers))
	}
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.ID)
	}
	for _, want := range []string{"anilibria", "animego", "kodik", "allanime"} {
		if !strings.Contains(strings.Join(ids, ","), want) {
			t.Fatalf("provider %s missing from %v", want, ids)
		}
	}

	// Offline service on a fresh data dir reports an empty library
	// (legal state, I3 spirit).
	titles, err := real.Deps.Offline.Titles()
	if err != nil || len(titles) != 0 {
		t.Fatalf("fresh offline library must be empty, got %v (%v)", titles, err)
	}
}

// TestRealHistoryBindSource: binding moves the record onto the new
// (source_id, source_url) key — the old row disappears, the new row
// keeps the title and drops the correction flag.
func TestRealHistoryBindSource(t *testing.T) {
	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	title := "Ванпанчмен"
	rec := &storage.AnimeProgress{
		Title: title, SourceID: "animego", SourceURL: "u1",
		CurrentEpisode: "3", NeedsCorrection: true, UpdatedAt: time.Now().UTC(),
	}
	if err := store.Progress.Upsert(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := &realHistory{store: store}
	if err := h.BindSource(context.Background(), rec.ID, "anilib", "u2"); err != nil {
		t.Fatalf("BindSource: %v", err)
	}

	items, err := h.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want exactly one row after bind, got %d", len(items))
	}
	got := items[0]
	if got.SourceID != "anilib" || got.SourceURL != "u2" {
		t.Fatalf("row must move to the new source, got %s/%s", got.SourceID, got.SourceURL)
	}
	if got.Title != title || got.NeedsCorrection {
		t.Fatalf("title must survive and correction flag drop: %+v", got)
	}
}
