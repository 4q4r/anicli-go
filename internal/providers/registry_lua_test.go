package providers

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
)

// luaTestScript is a conforming minimal provider.
const luaTestScript = `
return {
	id = "myprov",
	search = function(query) return { { title = "hit", url = "/u" } } end,
	episodes = function(anime_url) return {} end,
	streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
}
`

// TestRegistryWithLuaDiscovery: a conforming user script in the XDG
// providers tree registers alongside the built-ins; the registry
// serves it like any compiled provider.
func TestRegistryWithLuaDiscovery(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "anicli", "providers", "myprov")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(luaTestScript), 0o600); err != nil {
		t.Fatal(err)
	}

	log, buf := luaTestLogger(t)
	reg, err := NewRegistry(config.Settings{}, nil, WithLuaDiscovery(), WithProviderLogger(log))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, ok := reg.Get("myprov")
	if !ok {
		t.Fatalf("the discovered provider is not registered (log: %s)", buf.String())
	}
	if p.ID() != "myprov" {
		t.Fatalf("registered id = %q", p.ID())
	}
}

// TestRegistryLuaDuplicateIDSkipped: a Lua shadow of a built-in never
// replaces it — Register rejects the duplicate and the startup logs
// the skip.
func TestRegistryLuaDuplicateIDSkipped(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "anicli", "providers", "animego")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	shadow := `
	return {
		id = "animego",
		search = function(query) return {} end,
		episodes = function(anime_url) return {} end,
		streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
	}
	`
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(shadow), 0o600); err != nil {
		t.Fatal(err)
	}

	log, buf := luaTestLogger(t)
	reg, err := NewRegistry(config.Settings{}, nil, WithLuaDiscovery(), WithProviderLogger(log))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, ok := reg.Get("animego")
	if !ok {
		t.Fatal("the built-in animego must stay registered")
	}
	if p.ID() != "animego" {
		t.Fatalf("unexpected provider %q", p.ID())
	}
	if !strings.Contains(buf.String(), "not registered") && !strings.Contains(buf.String(), "skipped") {
		t.Fatalf("the duplicate skip must be logged, got: %s", buf.String())
	}
}

// luaTestLogger returns a buffer-backed logger for assertions.
func luaTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}
