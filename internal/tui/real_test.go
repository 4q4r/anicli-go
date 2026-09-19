package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/download"
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
	settings.Download.Dir = t.TempDir()           // isolate the offline scan
	settings.Providers.Kodik.Token = "test-token" // keep kodik registered (PR24)
	real, err := NewRealDeps(settings, store)
	if err != nil {
		t.Fatalf("NewRealDeps: %v", err)
	}
	defer real.Close()

	if real.Deps == nil || real.Deps.Search == nil || real.Deps.Episode == nil ||
		real.Deps.Playback == nil || real.Deps.History == nil || real.Deps.Offline == nil ||
		real.Deps.Database == nil || real.Deps.Health == nil || real.Deps.Shiki == nil ||
		real.Deps.Download == nil || real.Deps.Metadata == nil {
		t.Fatalf("all services must be wired (incl. metadata, PR24)")
	}
	if real.Deps.SearchTimeout != settings.Network.SearchTimeout {
		t.Fatalf("SearchTimeout must propagate from settings: got %v want %v",
			real.Deps.SearchTimeout, settings.Network.SearchTimeout)
	}

	providers := real.Deps.Search.Providers()
	if len(providers) < 12 {
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

// TestRealDepsDisabledProviders (PR24): a tokenless kodik lands in the
// disabled set and is absent from the searchable roster.
func TestRealDepsDisabledProviders(t *testing.T) {
	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	settings := config.Default()
	settings.Download.Dir = t.TempDir()
	// anime365 stays configured: this test pins the kodik specimen of
	// the disabled-set mechanism (PR55 added another credentialled
	// provider).
	settings.Providers.Anime365.Token = "test-token"
	real, err := NewRealDeps(settings, store)
	if err != nil {
		t.Fatalf("NewRealDeps: %v", err)
	}
	defer real.Close()

	for _, p := range real.Deps.Search.Providers() {
		if p.ID == "kodik" {
			t.Fatalf("tokenless kodik must not be searchable")
		}
	}
	disabled := real.Deps.Search.DisabledProviders()
	if len(disabled) != 1 || disabled[0].ID != "kodik" || disabled[0].Reason == "" {
		t.Fatalf("kodik must be reported disabled with a reason, got %+v", disabled)
	}
}

// TestRealDownloadPrunesSettledParts (M13): once the manager settles
// a background task (done or failed), the next submit prunes its
// resolve parts — the bridge map cannot grow unboundedly.
func TestRealDownloadPrunesSettledParts(t *testing.T) {
	bridge := &realDownload{}
	// A stub runner keeps the manager self-contained: the prune logic
	// only consults the manager's task states.
	manager := download.NewManager(1, func(_ context.Context, _ download.Task, _ func(float64)) error {
		return nil
	})
	bridge.manager = manager
	defer func() { _ = manager.Close() }()

	first := DownloadTask{AnimeTitle: "A", EpisodeNum: "1", DubID: "d", Quality: "720"}
	second := DownloadTask{AnimeTitle: "B", EpisodeNum: "1", DubID: "d", Quality: "720"}
	bridge.Submit(first)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if task, ok := manager.Task(downloadTaskID(first)); ok &&
			task.State != download.StateQueued && task.State != download.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first task never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	bridge.Submit(second)
	bridge.mu.Lock()
	_, hasFirst := bridge.parts[downloadTaskID(first)]
	_, hasSecond := bridge.parts[downloadTaskID(second)]
	bridge.mu.Unlock()
	if hasFirst {
		t.Fatalf("settled task parts must be pruned")
	}
	if !hasSecond {
		t.Fatalf("live task parts must survive the prune")
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
	if err := h.BindSource(context.Background(), rec.ID, "anilib", "u2", title); err != nil {
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
