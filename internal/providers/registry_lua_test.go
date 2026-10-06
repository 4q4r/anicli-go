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

// TestRegistryLuaUserOverrideReplacesBundled pins the override rule
// (the PR116 shadow rule's final form — it superseded PR111's "Go
// always wins", and PR147 retired its premise). A user script with a
// built-in id REPLACES the bundled copy at the SAME roster position,
// and the registry's capability probe proves which implementation
// serves the id.
//
// Why this is no longer a "shadow" test: the sample was re-pointed to
// subsplease in PR144 precisely because the roster's stream providers
// had all gone Lua-only — a user script shadowing a COMPILED factory
// needed a compiled factory to exist. Since PR147 none does (the
// tokyotosho migration was the thirtieth and last Go→Lua slot), so
// the machinery's shadow branch (the "shadowed by its lua script"
// log, fired only for non-luaOnly factories) is unreachable for the
// real roster: the loader's first-occurrence rule (user XDG dir
// ahead of the bundled embeds) makes the override a REPLACEMENT. The
// decision to rewrite rather than synthesize a test-only compiled
// factory: the allFactories table is package-level state with no
// mutation seam (every test reads it through All()/NewRegistry in
// parallel), so a synthetic factory would invent global-state
// machinery no test exercises — while the load-source line below
// already proves the replacement honestly.
func TestRegistryLuaUserOverrideReplacesBundled(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// subsplease stays the sample: a torrent-declared BUNDLED script
	// (Lua-only since PR144), fully anonymous, one client build away
	// once [torrent] is on. A user copy without a torrent declaration
	// serves the slot as a plain Lua provider — the first-occurrence
	// rule does not care what it replaces.
	dir := filepath.Join(xdg, "anicli", "providers", "subsplease")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	override := `
	return {
		id = "subsplease",
		content_lang = "lua-probe",
		search = function(query) return {} end,
		episodes = function(anime_url) return {} end,
		streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
	}
	`
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"
	// The roster builds complete only with the [torrent] subsystem on
	// (the torrent-declared bundled scripts drop their slots
	// otherwise — the unconfigured rule).
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
	// The user copy REPLACES the bundled one by the first-occurrence
	// rule — proven by the load-source line naming the user XDG dir,
	// not "bundled".
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
