package providers

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/storage"
)

func TestAllReturnsFiveWaveOneProviders(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Network
	cfg.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 5 {
		t.Fatalf("All() = %d providers, want 5", len(bare))
	}

	wantIDs := []string{"anilibria", "animevost", "anilib", "animego", "sovetromantica"}
	seen := map[string]bool{}
	for _, p := range bare {
		if seen[p.ID()] {
			t.Errorf("duplicate provider ID %q", p.ID())
		}
		seen[p.ID()] = true
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Errorf("All() missing provider %q", id)
		}
	}
}

func TestNewRegistryWrapsEveryProvider(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Network
	cfg.ProxyURL = ""

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	list := reg.List()
	if len(list) != 5 {
		t.Fatalf("List() = %d providers, want 5", len(list))
	}
	// Registration order follows All() (stable render/fan-out order).
	wantOrder := []string{"anilibria", "animevost", "anilib", "animego", "sovetromantica"}
	for i, p := range list {
		if p.ID() != wantOrder[i] {
			t.Errorf("list[%d] = %s, want %s", i, p.ID(), wantOrder[i])
		}
	}

	// Every entry must be wrapped in the search-stat delegator: a failing
	// search returns a wrapped error even when the provider forgets to.
	got, ok := reg.Get("anilibria")
	if !ok {
		t.Fatal("Get(anilibria) not found")
	}
	if _, ok := got.(SearchDelegator); !ok {
		t.Fatalf("registry entry %T is not a SearchDelegator", got)
	}
}

func TestNewRegistryRecordsSearchStatsWiring(t *testing.T) {
	// No-network proof of the wiring: SearchDelegator + ProviderStatRepo
	// recording is behavior-tested in registry_test.go against a stub;
	// this test pins that NewRegistry produces exactly that composition.
	// (A registry search here would hit production URLs, which tests must
	// never do.)
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default().Network
	cfg.ProxyURL = ""

	reg, err := NewRegistry(cfg, st.ProviderStats)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	for _, p := range reg.List() {
		if _, ok := p.(SearchDelegator); !ok {
			t.Fatalf("registry entry %s (%T) is not a SearchDelegator", p.ID(), p)
		}
	}
}

func TestNewRegistryPropagatesClientError(t *testing.T) {
	t.Parallel()

	// An invalid proxy URL makes per-provider client construction fail;
	// NewRegistry must surface it instead of dropping providers.
	cfg := config.Default().Network
	cfg.ProxyURL = "://not-a-url"

	if _, err := NewRegistry(cfg, nil); err == nil {
		t.Fatal("NewRegistry with invalid proxy must fail")
	}
}
