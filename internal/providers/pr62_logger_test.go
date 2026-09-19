package providers

// PR62 owner defect #4: provider-level drop logs (PR53-era Info lines)
// went through the package slog default handler → stderr, corrupting
// the TUI alt-screen. They must ride the injected FILE logger, and a
// provider without an injected logger must never fall back to
// slog.Default.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
)

// TestAnilibDropsLogThroughInjectedLogger (PR62 #4): the contentless
// drop line rides the provider's injected logger — the TUI's file
// logger — and never the slog default (stderr).
func TestAnilibDropsLogThroughInjectedLogger(t *testing.T) {
	var injected bytes.Buffer
	base := anilibMuxFixtureServer(t, map[string][]byte{
		"/anime":                   fixture(t, "anilib_search_black_lagoon.json"),
		"/episodes?anime_id=25322": fixture(t, "anilib_episodes_cm.json"),
		"/episodes/141970":         fixture(t, "anilib_episode_players_cm.json"),
		"/episodes?anime_id=805":   fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=1343":  fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=3864":  fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=5317":  fixture(t, "anilib_episodes.json"),
		"/episodes/13":             fixture(t, "anilib_episode_players.json"),
	}, nil)
	p := newAnilib(base, testClient(t, "anilib"))
	p.SetLogger(slog.New(slog.NewTextHandler(&injected, nil)))

	if _, err := p.Search(context.Background(), "black lagoon"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(injected.String(), "anilib: dropped contentless release") {
		t.Fatalf("the drop line must reach the injected file logger, got:\n%s", injected.String())
	}
}

// TestBaseLoggerSeamNeverDefaults (PR62 #4): an unwired provider
// logger degrades to discard — never slog.Default (stderr), the same
// rule as the TUI's logf.
func TestBaseLoggerSeamNeverDefaults(t *testing.T) {
	var b Base
	got := b.loggerOrDiscard()
	if got == nil {
		t.Fatalf("logger must never return nil")
	}
	if got == slog.Default() {
		t.Fatalf("an unwired provider logger must degrade to discard, not slog.Default (stderr)")
	}
}

// TestRegistryWiresProviderLogger (PR62 #4): NewRegistry injects the
// WithProviderLogger sink into every provider carrying the Base seam.
func TestRegistryWiresProviderLogger(t *testing.T) {
	var buf bytes.Buffer
	reg, err := NewRegistry(config.Default(), nil, WithProviderLogger(
		slog.New(slog.NewTextHandler(&buf, nil))))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	p, ok := reg.Get("anilib")
	if !ok {
		t.Fatalf("anilib not registered")
	}
	al, ok := bareProvider(p).(*Anilib)
	if !ok {
		t.Fatalf("bare anilib expected, got %T", bareProvider(p))
	}
	if al.logger == nil {
		t.Fatalf("the registry must inject the provider logger into the Base seam")
	}

	tp, ok := reg.Get("tokyotosho")
	if !ok {
		t.Fatalf("tokyotosho not registered")
	}
	tt, ok := bareProvider(tp).(*TokyoTosho)
	if !ok {
		t.Fatalf("bare tokyotosho expected, got %T", bareProvider(tp))
	}
	if tt.logger == nil {
		t.Fatalf("the registry must inject the provider logger into tokyotosho's Base seam")
	}
}
