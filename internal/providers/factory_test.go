package providers

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/storage"
)

func TestAllReturnsElevenProviders(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 11 {
		t.Fatalf("All() = %d providers, want 11", len(bare))
	}

	wantIDs := []string{
		"anilibria", "animevost", "anilib", "animego",
		"gogoanime", "animepahe", "dreamcast", "sameband", "kodik",
		"allanime", "anidub",
	}
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

func TestAllWiresKodikTokenFromConfig(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "from-config"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() != "kodik" {
			continue
		}
		k, ok := p.(*Kodik)
		if !ok {
			t.Fatalf("kodik entry is %T, want *Kodik", p)
		}
		if k.token != "from-config" {
			t.Errorf("kodik token = %q, want the settings value", k.token)
		}
		return
	}
	t.Fatal("All() missing the kodik provider")
}

func TestNewRegistryWrapsEveryProvider(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	list := reg.List()
	if len(list) != 11 {
		t.Fatalf("List() = %d providers, want 11", len(list))
	}
	// Registration order follows All() (stable render/fan-out order);
	// anidub (no frozen Python original) is appended after the ported
	// roster.
	wantOrder := []string{
		"anilibria", "animevost", "anilib", "animego",
		"gogoanime", "animepahe", "dreamcast", "sameband", "kodik",
		"allanime", "anidub",
	}
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

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

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
	cfg := config.Default()
	cfg.Network.ProxyURL = "://not-a-url"

	if _, err := NewRegistry(cfg, nil); err == nil {
		t.Fatal("NewRegistry with invalid proxy must fail")
	}
}
