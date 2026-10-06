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
	// subsplease (PR143): the shadow sample must be a COMPILED
	// factory — after the PR141 hdrezka, PR142 rutor and PR143
	// anirena migrations every stream provider is Lua-only, and the
	// compiled factories left are the five torrent ones. subsplease
	// is the sample (fully anonymous, one client build away once
	// [torrent] is on; the other four behave identically through
	// the same factory plumbing; the anirena slot additionally wraps
	// its script in the luaTorrent adapter, which this shadow rule
	// does not exercise).
	dir := filepath.Join(xdg, "anicli", "providers", "subsplease")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	shadow := `
	return {
		id = "subsplease",
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
	// The torrent factories build only when the [torrent] subsystem is
	// enabled (the unconfigured rule drops them otherwise — there
	// would be no compiled provider to shadow).
	cfg.Torrent.Enabled = true

	log, buf := luaTestLogger(t)
	reg, err := NewRegistry(cfg, nil, WithProviderLogger(log))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, ok := reg.Get("subsplease")
	if !ok {
		t.Fatal("the overridden subsplease must stay registered (as the user's Lua script)")
	}
	if got := reg.ContentLanguage("subsplease"); got != "lua-probe" {
		t.Fatalf("subsplease ContentLanguage = %q, want the LUA implementation's probe value (log: %s)", got, buf.String())
	}
	// With zero compiled factories left the override is not a
	// "shadow" (that log fired only for a script taking a COMPILED
	// factory's slot): the user copy REPLACES the bundled one by the
	// first-occurrence rule — proven by the load-source line naming
	// the user XDG dir, not "bundled".
	if !strings.Contains(buf.String(), "provider=subsplease source="+filepath.Join(xdg, "anicli", "providers")) {
		t.Fatalf("the user override must be the served copy (the load source names the user dir), got: %s", buf.String())
	}
}

// luaTestLogger returns a buffer-backed logger for assertions.
func luaTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}
