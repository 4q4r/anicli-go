package cli

import (
	"bytes"
	"strings"
	"testing"
)

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
		name     string
		args     []string
		contains []string
	}{
		{
			name:     "bare invocation runs tui stub",
			args:     nil,
			contains: []string{"tui", "not implemented"},
		},
		{
			name:     "serve stub",
			args:     []string{"serve"},
			contains: []string{"serve", "not implemented"},
		},
		{
			name:     "doctor stub",
			args:     []string{"doctor"},
			contains: []string{"doctor", "not implemented"},
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

			if err := root.Execute(); err != nil {
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
