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
		if got := UnconfiguredProviders(cfg); len(got) != 0 {
			t.Fatalf("configured kodik must not be disabled, got %+v", got)
		}
	})
}

// TestAllSkipsUnconfiguredProviders: the factory excludes
// unconfigured providers from the built set entirely (PR24): kodik
// without a token never gets a client or a registry slot.
// TestTorrentProvidersDisabledWhenTorrentOff pins the disabled-table
// rule: the torrent providers have no credentials of their own but
// cannot resolve without the [torrent] subsystem, so
// torrent.enabled=false must exclude every one of them via the same
// unconfigured convention as kodik's missing token (rutor since
// PR87, anirena since PR88, subsplease since PR89 — the rule restored
// in fix/93).
func TestTorrentProvidersDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false
	disabled := UnconfiguredProviders(cfg)
	byID := map[string]string{}
	for _, d := range disabled {
		byID[d.ID] = d.Reason
	}
	for _, id := range []string{"anilibria-torrent", "animetosho", "tokyotosho", "rutor", "anirena", "subsplease"} {
		reason, ok := byID[id]
		if !ok {
			t.Errorf("%s must be in the unconfigured set when [torrent] is disabled", id)
			continue
		}
		if reason == "" {
			t.Errorf("%s: disabled reason must be user-facing (RU), got empty", id)
		}
	}
}

func TestAllSkipsUnconfiguredProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() == "kodik" {
			t.Fatalf("unconfigured kodik must not be built, got %v", p.ID())
		}
	}
	if len(bare) != 24 {
		t.Fatalf("want the remaining 24 providers, got %d", len(bare))
	}
}

// TestRegistryDisabledListsUnconfigured: NewRegistry records the
// disabled set for the health/search surfaces.
func TestRegistryDisabledListsUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""

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
