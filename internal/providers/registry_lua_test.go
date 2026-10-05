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

// TestRegistryLuaUserScriptRegisters (PR116, config-driven): a
// conforming user script in the XDG providers tree registers at the
// roster tail; the registry serves it like any compiled provider.
func TestRegistryLuaUserScriptRegisters(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "anicli", "providers", "myprov")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(luaTestScript), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token" // keep the roster intact (PR24)

	log, buf := luaTestLogger(t)
	reg, err := NewRegistry(cfg, nil, WithProviderLogger(log))
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

// TestRegistryLuaShadowReplacesGo pins the PR116 shadow rule (it
// superseded PR111's "Go always wins"): a user script with a built-in
// id REPLACES the compiled provider, and the registry's capability
// probe proves which implementation serves the id.
func TestRegistryLuaShadowReplacesGo(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// gogoanime (PR124): the shadow sample must be a COMPILED factory —
	// the Lua-only slots (anitokyo, …, animego) have no Go provider to
	// shadow-log anymore.
	dir := filepath.Join(xdg, "anicli", "providers", "gogoanime")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	shadow := `
	return {
		id = "gogoanime",
		content_lang = "lua-probe",
		search = function(query) return {} end,
		episodes = function(anime_url) return {} end,
		streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
	}
	`
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(shadow), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"

	log, buf := luaTestLogger(t)
	reg, err := NewRegistry(cfg, nil, WithProviderLogger(log))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, ok := reg.Get("gogoanime")
	if !ok {
		t.Fatal("the shadowed gogoanime must stay registered (as the Lua script)")
	}
	if got := reg.ContentLanguage("gogoanime"); got != "lua-probe" {
		t.Fatalf("gogoanime ContentLanguage = %q, want the LUA implementation's probe value (log: %s)", got, buf.String())
	}
	if !strings.Contains(buf.String(), "shadowed by its lua script") {
		t.Fatalf("the shadow must be logged, got: %s", buf.String())
	}
}

// luaTestLogger returns a buffer-backed logger for assertions.
func luaTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}
