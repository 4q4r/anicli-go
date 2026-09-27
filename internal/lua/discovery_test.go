package lua

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// discoveryDir points XDG_CONFIG_HOME at a fresh temp root and returns
// the providers directory inside it.
func discoveryDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "anicli", "providers")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeScript(t *testing.T, dir, id, src string) {
	t.Helper()
	sub := filepath.Join(dir, id)
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "main.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryFindsValidScript(t *testing.T) {
	dir := discoveryDir(t)
	writeScript(t, dir, "myprov", "return {\n"+
		"\tid = \"myprov\",\n"+
		"\tsearch = function(query) return { { title = \"hit\", url = \"/u\" } } end,\n"+
		"\tepisodes = function(anime_url) return {} end,\n"+
		"\tstreams = function(episode_url, dub) return { dub_name = dub, links = {} } end,\n"+
		"}")

	log, buf := testLogger(t)
	provs, errs := Discover(DefaultConfig(), log)
	if len(errs) != 0 {
		t.Fatalf("unexpected discovery errors: %v", errs)
	}
	if len(provs) != 1 {
		t.Fatalf("discovered %d providers, want 1", len(provs))
	}
	p := provs[0]
	if p.ID() != "myprov" || p.Name() != "myprov" {
		t.Fatalf("discovered provider = %q/%q", p.ID(), p.Name())
	}

	// The discovered provider works end-to-end.
	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "hit" || results[0].SourceID != "myprov" {
		t.Fatalf("search through the discovered provider = %+v", results)
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Fatalf("source type = %q", p.SourceType())
	}
	_ = buf
}

func TestDiscoveryInvalidScriptSkippedAndLogged(t *testing.T) {
	dir := discoveryDir(t)
	writeScript(t, dir, "halves", `return { id = "halves", search = function() end }`)

	log, buf := testLogger(t)
	provs, errs := Discover(DefaultConfig(), log)
	if len(provs) != 0 {
		t.Fatalf("an invalid script must be skipped, got %d providers", len(provs))
	}
	if len(errs) != 1 {
		t.Fatalf("the skip must be reported, got %d errors", len(errs))
	}
	if !logContains(buf, "halves") {
		t.Fatalf("the skip must be logged with the provider id: %s", buf.String())
	}
}

func TestDiscoveryIDMismatchSkipped(t *testing.T) {
	dir := discoveryDir(t)
	writeScript(t, dir, "wrapped", `
	return {
		id = "other",
		search = function() return {} end,
		episodes = function() return {} end,
		streams = function() return {} end,
	}
	`)
	log, buf := testLogger(t)
	provs, errs := Discover(DefaultConfig(), log)
	if len(provs) != 0 || len(errs) != 1 {
		t.Fatalf("an id mismatch must skip: %d providers, %d errors", len(provs), len(errs))
	}
	if !logContains(buf, "wrapped") {
		t.Fatalf("the skip must be logged: %s", buf.String())
	}
}

func TestDiscoveryNoDirectoryIsNotAnError(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg) // no anicli/providers subtree at all

	provs, errs := Discover(DefaultConfig(), mustLogger(t))
	if len(provs) != 0 || len(errs) != 0 {
		t.Fatalf("a missing tree must discover nothing quietly: %d, %v", len(provs), errs)
	}
}

func TestDiscoveryIgnoresNonMainLuaFiles(t *testing.T) {
	dir := discoveryDir(t)
	writeScript(t, dir, "real", "return {\n"+
		"\tid = \"real\",\n"+
		"\tsearch = function() return {} end,\n"+
		"\tepisodes = function() return {} end,\n"+
		"\tstreams = function() return {} end,\n"+
		"}")
	// A stray lua file in a provider dir without main.lua: not a provider.
	writeScript(t, dir, "helper", "")
	if err := os.Remove(filepath.Join(dir, "helper", "main.lua")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper", "util.lua"), []byte("return 1"), 0o600); err != nil {
		t.Fatal(err)
	}

	provs, errs := Discover(DefaultConfig(), mustLogger(t))
	if len(provs) != 1 || provs[0].ID() != "real" {
		t.Fatalf("only main.lua providers must load: %d providers", len(provs))
	}
	if len(errs) != 0 {
		t.Fatalf("a dir without main.lua must be ignored quietly: %v", errs)
	}
}

// TestDiscoveryUsesSandboxedBudget: a discovered script still runs
// under the invocation timeout — discovery itself cannot hang startup
// on a hostile script.
func TestDiscoveryUsesSandboxedBudget(t *testing.T) {
	dir := discoveryDir(t)
	writeScript(t, dir, "hanger", `
	return {
		id = "hanger",
		search = function(query) while true do end end,
		episodes = function() return {} end,
		streams = function() return {} end,
	}
	`)
	cfg := DefaultConfig()
	cfg.Timeout = 100 * time.Millisecond

	start := time.Now()
	provs, errs := Discover(cfg, mustLogger(t))
	if len(provs) != 1 {
		t.Fatalf("the provider loads fine (validation is bounded): %v", errs)
	}
	_, err := provs[0].Search(context.Background(), "q")
	if err == nil {
		t.Fatal("the hostile script must hit the budget")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the budget took %v to fire", time.Since(start))
	}
}
