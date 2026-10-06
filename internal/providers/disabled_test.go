package providers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
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

// TestAllSkipsUnconfiguredProviders pins the PR140 credential-gate
// parity for the Lua-pinned kodik slot. Two legs:
//
//   - [providers.lua] enabled (the default): the bundled script serves
//     the id and REGISTERS even tokenless — and Search fails loud with
//     the typed ErrInvalidInput BEFORE any request leaves the process
//     (the dead-endpoint base proves the short-circuit), mirroring the
//     Go constructor's fail-loud-on-use error policy. This is the
//     credential-gated no-op the parity smoke's SKIP roster expects.
//   - [providers.lua] disabled: kodik has no Go constructor anymore —
//     the slot drops entirely (the twenty migrated slots are Lua-only).
func TestAllSkipsUnconfiguredProviders(t *testing.T) {
	t.Run("lua enabled: tokenless kodik registers and fails loud on use", func(t *testing.T) {
		t.Parallel()

		cfg := config.Default()
		cfg.Network.ProxyURL = ""
		cfg.Providers.Kodik.Token = ""

		bare, err := All(cfg)
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(bare) != 30 {
			t.Fatalf("All() = %d providers, want 30 (the Lua-pinned kodik stays in the roster)", len(bare))
		}
		var kodik contracts.Provider
		for _, p := range bare {
			if p.ID() == "kodik" {
				kodik = p
				break
			}
		}
		if kodik == nil {
			t.Fatal("the Lua-pinned kodik slot must register even tokenless (it fails loud on use instead)")
		}
		_, err = kodik.Search(context.Background(), "q")
		if !errors.Is(err, contracts.ErrInvalidInput) {
			t.Fatalf("tokenless kodik Search = %v, want the typed ErrInvalidInput", err)
		}
		for _, want := range []string{"providers.kodik.token", "ANICLI_KODIK_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, must mention %q", err, want)
			}
		}
	})

	t.Run("lua disabled: the kodik slot drops with its constructor", func(t *testing.T) {
		t.Parallel()

		cfg := config.Default()
		cfg.Network.ProxyURL = ""
		cfg.Providers.Kodik.Token = ""
		cfg.Providers.Lua.Enabled = false

		bare, err := All(cfg)
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(bare) != 7 {
			t.Fatalf("All() = %d providers, want 7 (the Go factories; kodik is Lua-only since PR140)", len(bare))
		}
		for _, p := range bare {
			if p.ID() == "kodik" {
				t.Fatal("unconfigured kodik must not be built with [providers.lua] disabled")
			}
		}
	})
}

// TestRegistryDisabledListsUnconfigured: NewRegistry records the
// disabled set for the health/search surfaces. With [providers.lua]
// disabled the tokenless kodik lands there (nothing serves the id);
// with Lua enabled the bundled script un-disables it (factory.go's
// un-disabling rule) and the loud-on-use search is the gate instead.
func TestRegistryDisabledListsUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = ""
	cfg.Providers.Lua.Enabled = false

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
