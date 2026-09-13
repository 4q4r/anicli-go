package providers

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
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

// TestAllProvidersSourceTypeBoth pins the corrected SourceType
// semantics (PR23): SourceType describes content SUITABILITY for the
// user's wanted audio languages (EN/JA/RU), and every current roster
// member serves wanted-language audio with acceptable video — so the
// whole roster is BOTH. VIDEO is for unwanted/absent audio (future
// providers), AUDIO for catalog-wide hardsubs/unwatchable video.
func TestAllProvidersSourceTypeBoth(t *testing.T) {
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
	for _, p := range bare {
		if got := p.SourceType(); got != contracts.SourceTypeBoth {
			t.Errorf("provider %s SourceType() = %q, want %q", p.ID(), got, contracts.SourceTypeBoth)
		}
	}
}

// TestContentLanguageRoster pins each provider's declared primary
// content language (PR23): Russian-dub sites tag their dubs "ru",
// Japanese-audio sites with English subtitles "ja". The language is a
// service-level property — every dub a provider emits carries it, no
// per-dub introspection.
func TestContentLanguageRoster(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"anilibria": "ru",
		"animevost": "ru",
		"anilib":    "ru",
		"animego":   "ru",
		"gogoanime": "ja",
		"animepahe": "ja",
		"dreamcast": "ru",
		"sameband":  "ru",
		"kodik":     "ru",
		"allanime":  "ja", // primary sub track is Japanese; dub→"en"
		"anidub":    "ru",
	}

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		lc, ok := p.(interface{ ContentLanguage() string })
		if !ok {
			t.Errorf("provider %s (%T) does not expose ContentLanguage", p.ID(), p)
			continue
		}
		if got := lc.ContentLanguage(); got != want[p.ID()] {
			t.Errorf("provider %s ContentLanguage() = %q, want %q", p.ID(), got, want[p.ID()])
		}
	}
}
