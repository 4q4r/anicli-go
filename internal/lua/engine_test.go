package lua

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestEngine builds an Engine wired to a captured log buffer with
// test-sized budgets.
func newTestEngine(t *testing.T) (*Engine, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	e := NewEngine(Options{
		Log:      log,
		Timeout:  2 * time.Second,
		RepLimit: 1000,
	})
	return e, &buf
}

// doEval runs src in a fresh sandboxed state and returns the printed
// view of the chunk's first return value (or the error).
func doEval(t *testing.T, e *Engine, src string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	L, err := e.NewState(ctx)
	if err != nil {
		return "", err
	}
	defer L.Close()
	if err := L.DoString(src); err != nil {
		return "", err
	}
	return L.Get(-1).String(), nil
}

// TestSandboxDangerousGlobalsAbsent: the never-open list (os, io,
// debug) and the stripped base functions are invisible to scripts.
func TestSandboxDangerousGlobalsAbsent(t *testing.T) {
	e, _ := newTestEngine(t)
	for _, name := range []string{
		"os", "io", "debug",
		"dofile", "loadfile", "load", "loadstring", "getfenv", "setfenv",
	} {
		got, err := doEval(t, e, "return type("+name+")")
		if err != nil {
			t.Fatalf("type(%s): %v", name, err)
		}
		if got != "nil" {
			t.Fatalf("sandbox leaked %s: type = %s", name, got)
		}
	}
	// string.dump returns portable bytecode — the load() escape hatch's
	// twin. Stripped.
	got, err := doEval(t, e, `return type(string.dump)`)
	if err != nil {
		t.Fatalf("type(string.dump): %v", err)
	}
	if got != "nil" {
		t.Fatalf("sandbox leaked string.dump: type = %s", got)
	}
}

// TestSandboxCoreLibsPresent: the whitelist carries the everyday
// surface scripts need.
func TestSandboxCoreLibsPresent(t *testing.T) {
	e, _ := newTestEngine(t)
	for _, tc := range []struct{ src, want string }{
		{`return type(pairs) .. type(ipairs) .. type(pcall) .. type(error)`, "functionfunctionfunctionfunction"},
		{`return type(table.concat) .. type(string.format) .. type(math.floor) .. type(coroutine.create)`, "functionfunctionfunctionfunction"},
		{`return string.format("%d-%s", 7, "a")`, "7-a"},
		{`return table.concat({1, 2, 3}, ",")`, "1,2,3"},
		{`return tostring(math.floor(1.9))`, "1"},
		{`local ok, err = pcall(error, "boom"); return tostring(ok) .. ":" .. err`, "false:<string>:1: boom"},
	} {
		got, err := doEval(t, e, tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.src, got, tc.want)
		}
	}
}

// TestSandboxPrintReboundToLogger: print lands in the wired logger,
// not in stdout (a TUI must not have its screen corrupted).
func TestSandboxPrintReboundToLogger(t *testing.T) {
	e, buf := newTestEngine(t)
	if _, err := doEval(t, e, `print("hello from lua")`); err != nil {
		t.Fatalf("print: %v", err)
	}
	if !strings.Contains(buf.String(), "hello from lua") {
		t.Fatalf("print output missing from the logger: %q", buf.String())
	}
}

// TestSandboxStringRepCap: a single native op must not evade the
// context check by allocating unbounded memory (gopher-lua #521).
func TestSandboxStringRepCap(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := doEval(t, e, `return #string.rep("ab", 500)`); err != nil {
		t.Fatalf("string.rep within the cap must work: %v", err)
	}
	_, err := doEval(t, e, `return string.rep("a", 1001)`)
	if err == nil {
		t.Fatal("string.rep beyond the cap must fail")
	}
	if !strings.Contains(err.Error(), "string.rep") {
		t.Fatalf("the rep-cap error must name string.rep, got: %v", err)
	}
}

// TestSandboxContextTimeoutOnInfiniteLoop: the VM loop honors the
// installed context (issue #521's loop-level check).
func TestSandboxContextTimeoutOnInfiniteLoop(t *testing.T) {
	e, _ := newTestEngine(t)
	e.opts.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := doEval(t, e, `while true do end`)
	if err == nil {
		t.Fatal("an infinite loop must hit the context deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the deadline took %v to fire", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("the error must be context-typed, got: %v", err)
	}
}

// TestSandboxRequirePreloadOnly: require resolves preloaded modules
// only — the Lua-file loader (an arbitrary file read+load primitive)
// is stripped, even when package.path points straight at the file.
func TestSandboxRequirePreloadOnly(t *testing.T) {
	e, _ := newTestEngine(t)

	// A preloaded module resolves.
	got, err := doEval(t, e, `
		package.preload["mymod"] = function() return { answer = 42 } end
		local m = require("mymod")
		return m.answer
	`)
	if err != nil || got != "42" {
		t.Fatalf("preloaded require broken: %q %v", got, err)
	}

	// A real file on disk does NOT resolve, even with the path aimed
	// at it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stolen.lua"), []byte(`return "stolen"`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = doEval(t, e, `package.path = "`+dir+`/?.lua"; return require("stolen")`)
	if err == nil {
		t.Fatal("require must not load files from disk inside the sandbox")
	}
}

// TestSandboxPackageEscapeHatchesClosed: package.loadlib (C dlopen)
// is gone and package.path is empty.
func TestSandboxPackageEscapeHatchesClosed(t *testing.T) {
	e, _ := newTestEngine(t)
	got, err := doEval(t, e, `return type(package.loadlib) .. ":" .. tostring(package.path)`)
	if err != nil {
		t.Fatalf("package introspection: %v", err)
	}
	if got != "nil:" {
		t.Fatalf("package escape hatches open: %q", got)
	}
}

// TestEngineFreshStatesAreIsolated: every NewState is independent —
// no global leaks between invocations.
func TestEngineFreshStatesAreIsolated(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := doEval(t, e, `LEAK = "secret"`); err != nil {
		t.Fatal(err)
	}
	got, err := doEval(t, e, `return type(LEAK)`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "nil" {
		t.Fatalf("global leaked across states: LEAK = %s", got)
	}
}

// TestEngineCallStackCap: the configured call-stack size bounds
// runaway recursion.
func TestEngineCallStackCap(t *testing.T) {
	var buf bytes.Buffer
	e := NewEngine(Options{Log: slog.New(slog.NewTextHandler(&buf, nil)), CallStackSize: 32})
	_, err := doEval(t, e, `local function dive(n) if n == 0 then return 0 end return 1 + dive(n - 1) end return dive(1000)`)
	if err == nil {
		t.Fatal("recursion beyond the call-stack cap must fail")
	}
}
