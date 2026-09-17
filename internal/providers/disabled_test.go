package providers

import (
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
		cfg.Providers.Yanima.DDoSP1 = "p1" // isolate the kodik variable
		cfg.Providers.Yanima.DDoSP2 = "p2"
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
		cfg.Providers.Yanima.DDoSP1 = "p1" // isolate the kodik variable
		cfg.Providers.Yanima.DDoSP2 = "p2"
		if got := UnconfiguredProviders(cfg); len(got) != 0 {
			t.Fatalf("configured kodik must not be disabled, got %+v", got)
		}
	})

	t.Run("yanima without both DDoS cookies is reported", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret" // isolate the yanima variable
		got := UnconfiguredProviders(cfg)
		if len(got) != 1 || got[0].ID != "yanima" {
			t.Fatalf("want exactly yanima, got %+v", got)
		}
		if got[0].Reason == "" {
			t.Fatalf("reason must not be empty")
		}
	})

	t.Run("yanima with one cookie missing is still reported", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret"
		cfg.Providers.Yanima.DDoSP1 = "p1" // ddoS_p2 left empty
		got := UnconfiguredProviders(cfg)
		if len(got) != 1 || got[0].ID != "yanima" {
			t.Fatalf("the wall answers 403 without BOTH cookies, got %+v", got)
		}
	})

	t.Run("yanima with both cookies is not reported", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "secret"
		cfg.Providers.Yanima.DDoSP1 = "p1"
		cfg.Providers.Yanima.DDoSP2 = "p2"
		if got := UnconfiguredProviders(cfg); len(got) != 0 {
			t.Fatalf("configured yanima must not be disabled, got %+v", got)
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
	cfg.Providers.Yanima.DDoSP1 = "p1" // isolate the kodik variable (PR33)
	cfg.Providers.Yanima.DDoSP2 = "p2"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() == "kodik" {
			t.Fatalf("unconfigured kodik must not be built, got %v", p.ID())
		}
	}
	if len(bare) != 12 {
		t.Fatalf("want the remaining 12 providers, got %d", len(bare))
	}
}

// TestRegistryDisabledListsUnconfigured: NewRegistry records the
// disabled set for the health/search surfaces.
func TestRegistryDisabledListsUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""
	cfg.Providers.Yanima.DDoSP1 = "p1" // isolate the kodik variable (PR33)
	cfg.Providers.Yanima.DDoSP2 = "p2"

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

// TestRegistryDisabledListsYanima pins the yanima shape of the same
// mechanism (PR33): cookieless yanima is reported with its reason and
// never registered.
func TestRegistryDisabledListsYanima(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "set" // isolate the yanima variable

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	disabled := reg.Disabled()
	if len(disabled) != 1 || disabled[0].ID != "yanima" {
		t.Fatalf("registry must report yanima as disabled, got %+v", disabled)
	}
	if disabled[0].Reason == "" {
		t.Fatalf("disabled entry must carry the reason")
	}
	if _, ok := reg.Get("yanima"); ok {
		t.Fatalf("disabled yanima must not be registered")
	}
}
