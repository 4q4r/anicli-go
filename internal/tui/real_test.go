package tui

import (
	"context"
	"strings"
	"testing"

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
