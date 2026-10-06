package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// The PR116 factory-Lua integration: [providers.lua] assembles the
// bundled + user-dir script sources and the registry shadows the
// compiled Go factories with them by id. These tests pin the user-dir
// leg through XDG_CONFIG_HOME (env-seamed, so they stay off the
// parallel pool — the doctor-test precedent) and are hermetic: the
// machine's real ~/.config/anicli/providers must never leak in.

// luaXDG pins XDG_CONFIG_HOME at a fresh temp root and returns the
// providers directory inside it.
func luaXDG(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "anicli", "providers")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeLuaScript drops a provider script into dir/<id>/main.lua.
func writeLuaScript(t *testing.T, dir, id, body string) {
	t.Helper()
	sub := filepath.Join(dir, id)
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	src := "return {\n" +
		"\tid = \"" + id + "\",\n" +
		"\tsearch = function(query) return {} end,\n" +
		"\tepisodes = function(anime_url) return {} end,\n" +
		"\tstreams = function(episode_url, dub) return { dub_name = dub, links = {} } end,\n" +
		body + "}"
	if err := os.WriteFile(filepath.Join(sub, "main.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rosterIDs projects a built provider list onto ids.
func rosterIDs(built []contracts.Provider) []string {
	out := make([]string, 0, len(built))
	for _, p := range built {
		out = append(out, p.ID())
	}
	return out
}

// TestFactoryLuaShadowsGoInPlace pins the shadow rule: a user script
// with a Go roster id REPLACES the compiled provider at the SAME
// roster position (the order pin stays byte-identical) and the
// capability surfaces prove which implementation serves the id.
func TestFactoryLuaShadowsGoInPlace(t *testing.T) {
	dir := luaXDG(t)
	writeLuaScript(t, dir, "anilibria", "\tcontent_lang = \"lua-probe\",\n")

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 30 {
		t.Fatalf("All() = %d providers, want 30 (shadow, not add)", len(bare))
	}
	if bare[0].ID() != "anilibria" {
		t.Fatalf("roster[0] = %q, want anilibria (shadow keeps the slot)", bare[0].ID())
	}
	lc, ok := bare[0].(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the shadowed anilibria lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "lua-probe" {
		t.Errorf("anilibria ContentLanguage = %q, want the LUA implementation's probe value", got)
	}
}

// TestFactoryLuaUserOnlyAppends pins the tail rule: a script with a
// NEW id registers at the roster tail (the Go order untouched).
func TestFactoryLuaUserOnlyAppends(t *testing.T) {
	dir := luaXDG(t)
	writeLuaScript(t, dir, "userscript", "")

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 31 {
		t.Fatalf("All() = %d providers, want 31 (30 Go + 1 user script)", len(bare))
	}
	if got := rosterIDs(bare); got[len(got)-1] != "userscript" {
		t.Errorf("roster tail = %q, want userscript", got[len(got)-1])
	}
	if got := rosterIDs(bare); got[0] != "anilibria" {
		t.Errorf("roster head = %q, want the untouched Go order", got[0])
	}
}

// TestFactoryLuaExcludedIsGone pins that [providers].exclude applies
// to Lua providers by id: an excluded shadow leaves NO provider under
// that id (the Go factory must not resurrect behind the exclusion).
func TestFactoryLuaExcludedIsGone(t *testing.T) {
	dir := luaXDG(t)
	writeLuaScript(t, dir, "anilibria", "")

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"
	cfg.Providers.Exclude = []string{"anilibria"}

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, id := range rosterIDs(bare) {
		if id == "anilibria" {
			t.Fatalf("excluded anilibria still registered: %v", rosterIDs(bare))
		}
	}
	if len(bare) != 29 {
		t.Fatalf("All() = %d providers, want 29 (30 minus the excluded shadow)", len(bare))
	}
}

// TestFactoryLuaDisabledConfig pins the kill switch: [providers.lua]
// enabled = false leaves the compiled Go factories alone. The
// twenty migrated slots (anitokyo, animedia, animevib since
// PR116; anilibria PR120; animevost PR119; anilib PR122; yummy
// PR123; animego PR124; shiza PR125; animeheaven PR126; anikoto
// PR127; gogoanime PR128; kickassanime PR129; anizone PR130;
// sameband PR131; anidub PR132; anikado PR133; animiku PR134;
// anifilm PR135; animemobi PR137; anistar PR138) are EMPTY in this
// mode: they live only in the bundled Lua scripts.
func TestFactoryLuaDisabledConfig(t *testing.T) {
	dir := luaXDG(t)
	writeLuaScript(t, dir, "userscript", "")

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"
	cfg.Providers.Lua.Enabled = false

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	ids := rosterIDs(bare)
	if len(ids) != 9 {
		t.Fatalf("All() = %d providers, want 9 (the Go factories; the twenty-one migrated providers are Lua-only)", len(ids))
	}
	for _, id := range ids {
		if id == "userscript" {
			t.Fatal("userscript registered with [providers.lua] disabled")
		}
		for _, migrated := range []string{"anitokyo", "animedia", "animevib", "anilibria", "animevost", "anilib", "yummy", "animego", "shiza", "animeheaven", "anikoto", "gogoanime", "kickassanime", "anizone", "sameband", "anidub", "anikado", "animiku", "anifilm", "animemobi", "anistar"} {
			if id == migrated {
				t.Errorf("%s registered with [providers.lua] disabled (it is Lua-only since PR116/PR119/PR120/PR122/PR123/PR124/PR125/PR126/PR127/PR128/PR129/PR130/PR131/PR132/PR133/PR134/PR135/PR137/PR138)", migrated)
			}
		}
	}
}

// TestFactoryLuaBrokenScriptIsolated pins startup survival: a broken
// script is a skip; the roster (including the sibling good script)
// builds. The anilibria and animevost legs pin the Lua-pinned slot's
// contract: a broken user OVERRIDE of a bundled script swallows the
// bundled fallback (the dedup keeps first occurrence) and the slot
// drops — the user's replacement intent, failed loud, never faked.
func TestFactoryLuaBrokenScriptIsolated(t *testing.T) {
	dir := luaXDG(t)
	writeLuaScript(t, dir, "animevost", "\tthis is not valid lua =\n")
	writeLuaScript(t, dir, "anilibria", "\tthis is not valid lua =\n")
	writeLuaScript(t, dir, "goodscript", "")

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v (a broken user script must never fail startup)", err)
	}
	ids := rosterIDs(bare)
	if len(ids) != 29 {
		t.Fatalf("All() = %d providers (%v), want 29 (28 Go-served + goodscript: both broken overrides — anilibria and animevost — dropped their Lua-pinned slots)", len(ids), ids)
	}
	for _, id := range ids {
		if id == "anilibria" || id == "animevost" {
			t.Errorf("%s registered despite its broken user override (the Lua-pinned slot must drop, not fall back silently): %v", id, ids)
		}
	}
	if ids[len(ids)-1] != "goodscript" {
		t.Errorf("roster tail = %q, want goodscript", ids[len(ids)-1])
	}
}
