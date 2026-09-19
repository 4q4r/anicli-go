package providers

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
)

// TestUnconfiguredProviders: the startup detection table (PR24) — a
// provider that cannot work without user-supplied credentials is
// reported with a RU reason; configured providers never appear.
func TestUnconfiguredProviders(t *testing.T) {
	t.Run("kodik without token is reported with the token reason", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = ""
		cfg.Providers.Anime365.Token = "secret" // isolate the kodik variable
		got := UnconfiguredProviders(cfg)
		if len(got) != 1 {
			t.Fatalf("want exactly kodik, got %+v", got)
		}
		if got[0].ID != "kodik" {
			t.Fatalf("want kodik, got %q", got[0].ID)
		}
		if got[0].Reason == "" {
			t.Fatalf("reason must not be empty")
		}
	})

	t.Run("kodik with a token is not reported", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret"
		cfg.Providers.Anime365.Token = "secret" // isolate the kodik variable
		if got := UnconfiguredProviders(cfg); len(got) != 0 {
			t.Fatalf("configured kodik must not be disabled, got %+v", got)
		}
	})

	// anime365 (PR55) joins the table with kodik's exact shape: the
	// embed data (playable links) is the ONE credential-gated resource
	// of the API, so a tokenless anime365 is useless and disabled.
	t.Run("anime365 without a token is reported with the token reason", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret" // isolate the anime365 variable
		got := UnconfiguredProviders(cfg)
		if len(got) != 1 {
			t.Fatalf("want exactly anime365, got %+v", got)
		}
		if got[0].ID != "anime365" {
			t.Fatalf("want anime365, got %q", got[0].ID)
		}
		if got[0].Reason == "" {
			t.Fatalf("reason must not be empty")
		}
		if !strings.Contains(got[0].Reason, "providers.anime365.token") {
			t.Errorf("reason must name the settings key, got %q", got[0].Reason)
		}
	})

	t.Run("anime365 with a token is not reported", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret"
		cfg.Providers.Anime365.Token = "secret"
		if got := UnconfiguredProviders(cfg); len(got) != 0 {
			t.Fatalf("configured anime365 must not be disabled, got %+v", got)
		}
	})
}

// TestAllSkipsUnconfiguredProviders: the factory excludes
// unconfigured providers from the built set entirely (PR24): kodik
// without a token never gets a client or a registry slot.
// TestNyaaDisabledWhenTorrentOff pins the disabled-table rule: nyaa
// has no credentials of its own but cannot resolve without the
// [torrent] subsystem, so torrent.enabled=false must exclude it via
// the same unconfigured convention as kodik's missing token.
func TestNyaaDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false
	disabled := UnconfiguredProviders(cfg)
	found := false
	for _, d := range disabled {
		if d.ID == "nyaa" {
			found = true
			if d.Reason == "" {
				t.Error("disabled reason must be user-facing (RU), got empty")
			}
		}
	}
	if !found {
		t.Fatal("nyaa must be in the unconfigured set when [torrent] is disabled")
	}
}

func TestAllSkipsUnconfiguredProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""
	cfg.Providers.Anime365.Token = "secret" // isolate the kodik variable (PR33/PR55)

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() == "kodik" {
			t.Fatalf("unconfigured kodik must not be built, got %v", p.ID())
		}
	}
	if len(bare) != 20 {
		t.Fatalf("want the remaining 20 providers, got %d", len(bare))
	}
}

// TestRegistryDisabledListsUnconfigured: NewRegistry records the
// disabled set for the health/search surfaces.
func TestRegistryDisabledListsUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""
	cfg.Providers.Anime365.Token = "secret" // isolate the kodik variable (PR33/PR55)

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	disabled := reg.Disabled()
	if len(disabled) != 1 || disabled[0].ID != "kodik" {
		t.Fatalf("registry must report kodik as disabled, got %+v", disabled)
	}
	if disabled[0].Reason == "" {
		t.Fatalf("disabled entry must carry the reason")
	}
	if _, ok := reg.Get("kodik"); ok {
		t.Fatalf("disabled kodik must not be registered")
	}
}
