package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
)

// mustDefaultSettings returns the default settings without proxy:
// registry construction in tests must never route egress anywhere.
func mustDefaultSettings(t *testing.T) config.Settings {
	t.Helper()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	return cfg
}

func TestDoctorListsProvidersWithoutNetwork(t *testing.T) {
	t.Parallel()

	// The doctor enumeration must match the registry exactly and must
	// not touch the network: building the registry only initializes
	// clients.
	cfg := mustDefaultSettings(t)
	reg, err := providers.NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := len(reg.List()); got != 11 {
		t.Fatalf("registry has %d providers, want 11", got)
	}

	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"doctor"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute doctor: %v", err)
	}
	out := buf.String()
	for _, p := range reg.List() {
		if !strings.Contains(out, p.ID()) {
			t.Errorf("doctor output %q missing provider %q", out, p.ID())
		}
	}
}

func TestNewRootCommandShape(t *testing.T) {
	t.Parallel()

	root := NewRootCommand()
	if root.Use != "anicli" {
		t.Errorf("root.Use = %q, want anicli", root.Use)
	}
	if root.RunE == nil {
		t.Error("root must define RunE (default behavior = tui)")
	}

	want := map[string]bool{"serve": false, "doctor": false, "version": false}
	for _, sub := range root.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
		if sub.RunE == nil {
			t.Errorf("subcommand %q must define RunE", sub.Name())
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("root is missing subcommand %q", name)
		}
	}
}

func TestStubOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		contains    []string
		errContains string // non-empty: Execute must fail containing this
	}{
		{
			name: "serve requires api.enabled",
			args: []string{"serve"},
			// The default settings keep the API off; serve must refuse
			// loudly instead of silently binding a port.
			errContains: "disabled",
		},
		{
			name: "doctor lists registered providers",
			args: []string{"doctor"},
			contains: []string{
				"doctor", "providers",
				"anilibria", "animevost", "anilib", "animego", "sovetromantica",
				"gogoanime", "animepahe", "dreamcast", "sameband", "kodik",
			},
		},
		{
			name:     "version prints build info",
			args:     []string{"version"},
			contains: []string{Version, Commit, Date},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			root := NewRootCommand()
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(tt.args)

			err := root.Execute()
			if tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("Execute(%v) err = %v, want containing %q", tt.args, err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%v): %v", tt.args, err)
			}
			out := buf.String()
			for _, sub := range tt.contains {
				if !strings.Contains(out, sub) {
					t.Errorf("output %q missing %q", out, sub)
				}
			}
		})
	}
}

// TestBareInvocationLaunchesTUI: running the root command without a
// subcommand launches the real bubbletea TUI. Test sandboxes have no
// controlling terminal, so the launch surfaces the TTY error instead
// of the old stub banner — proving the wiring is live. The data dir
// is isolated so the storage open never touches the real library.
func TestBareInvocationLaunchesTUI(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Setenv("ANICLI_DATA", t.TempDir())

	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(nil)

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("bare invocation without a terminal must fail with the TTY error, got %v", err)
	}
	if strings.Contains(buf.String(), "not implemented") {
		t.Fatalf("the TUI stub banner must be gone, got %q", buf.String())
	}
}

func TestVersionFlagConfigAccepted(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--config", "/nonexistent/settings.toml", "version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute with --config flag: %v", err)
	}
	if !strings.Contains(buf.String(), Version) {
		t.Fatalf("output %q missing version %q", buf.String(), Version)
	}
}

func TestUnknownSubcommandFails(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"bogus-subcommand"})

	if err := root.Execute(); err == nil {
		t.Fatal("unknown subcommand must return an error")
	}
}

func TestConfigPathFrom(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("ANICLI_CONFIG", "/from/env/settings.toml")

		root := NewRootCommand()
		root.SetArgs([]string{"--config", "/explicit/settings.toml", "version"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got, err := ConfigPathFrom(root)
		if err != nil {
			t.Fatalf("ConfigPathFrom: %v", err)
		}
		if got != "/explicit/settings.toml" {
			t.Fatalf("ConfigPathFrom = %q, want explicit flag value", got)
		}
	})

	t.Run("no flag falls back to env", func(t *testing.T) {
		t.Setenv("ANICLI_CONFIG", "/from/env/settings.toml")

		root := NewRootCommand()
		root.SetArgs([]string{"version"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got, err := ConfigPathFrom(root)
		if err != nil {
			t.Fatalf("ConfigPathFrom: %v", err)
		}
		if got != "/from/env/settings.toml" {
			t.Fatalf("ConfigPathFrom = %q, want env value", got)
		}
	})
}
