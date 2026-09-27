package lua

import (
	"context"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// eval loads and runs src in a sandboxed engine state, returning the
// PCall error (the caller asserts on it).
func eval(t *testing.T, e *Engine, ctx context.Context, src string) error {
	t.Helper()

	sctx, cancel := e.stateCtx(ctx)
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	fn, err := L.Load(strings.NewReader(src), "test-chunk")
	if err != nil {
		return err
	}
	L.Push(fn)
	return L.PCall(0, lua.MultRet, nil)
}

// evalRun runs src and returns the first return value.
func evalRun(t *testing.T, e *Engine, ctx context.Context, src string) lua.LValue {
	t.Helper()

	sctx, cancel := e.stateCtx(ctx)
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	fn, err := L.Load(strings.NewReader(src), "test-chunk")
	if err != nil {
		t.Fatalf("load %q: %v", src, err)
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	v := L.Get(-1)
	L.Pop(1)
	return v
}

func TestSandboxForbiddenLibrariesAbsent(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	// os, io, debug are never opened — absent as globals AND from
	// package.loaded (reopening them via require must be impossible).
	for _, name := range []string{"os", "io", "debug"} {
		if g := L.GetGlobal(name); g.Type() != lua.LTNil {
			t.Fatalf("global %s = %s, want nil", name, g.Type().String())
		}
		if loaded := L.GetField(L.GetField(L.Get(lua.RegistryIndex), "_LOADED"), name); loaded.Type() != lua.LTNil {
			t.Fatalf("package.loaded[%s] = %s, want nil", name, loaded.Type().String())
		}
	}
}

func TestSandboxWhitelistedLibrariesPresent(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	// The whitelist: base essentials, table, string (minus dump), math,
	// coroutine.
	for _, src := range []string{
		`return type(pairs) == "function"`,
		`return type(pcall) == "function"`,
		`return type(select) == "function"`,
		`return type(string.gsub) == "function"`,
		`return type(table.insert) == "function"`,
		`return type(math.floor) == "function"`,
		`return type(coroutine.create) == "function"`,
	} {
		if v := evalRun(t, e, context.Background(), src); v != lua.LTrue {
			t.Fatalf("%s -> %s, want true", src, v)
		}
	}
}

func TestSandboxBaseStripped(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	// Every escape hatch in the strip list must be nil. load/loadstring
	// are the bytecode+chunk loaders, dofile/loadfile the direct file
	// readers, getfenv/setfenv the environment breakout pair.
	for _, name := range []string{"dofile", "loadfile", "load", "loadstring", "getfenv", "setfenv"} {
		if g := L.GetGlobal(name); g.Type() != lua.LTNil {
			t.Fatalf("global %s = %s, want nil (stripped)", name, g.Type().String())
		}
	}
}

func TestSandboxStringDumpStripped(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	if v := evalRun(t, e, context.Background(), `return string.dump`); v != lua.LNil {
		t.Fatalf("string.dump = %s, want nil", v)
	}
}

func TestSandboxRepCap(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))

	// Legitimate small rep works.
	if v := evalRun(t, e, context.Background(), `return string.rep("ab", 3)`); v.String() != "ababab" {
		t.Fatalf("string.rep normal = %q, want ababab", v.String())
	}

	// The issue #521 single-op escape: a huge count must raise a Lua
	// error instead of allocating gigabytes (the context deadline is
	// never consulted inside one native call).
	err := eval(t, e, context.Background(), `local x = string.rep("a", 1000000000)`)
	if err == nil {
		t.Fatal("oversized string.rep must error")
	}
	if !strings.Contains(err.Error(), "string.rep") {
		t.Fatalf("rep error = %q, want it to name string.rep", err)
	}

	// The same cap covers the result-size form: a long string repeated
	// a modest number of times.
	err = eval(t, e, context.Background(), `local big = string.rep("a", 70000); local x = string.rep(big, 100)`)
	if err == nil {
		t.Fatal("oversized repeated result must error")
	}
}

func TestSandboxRepZeroAndNegative(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	if v := evalRun(t, e, context.Background(), `return string.rep("a", 0)`); v.String() != "" {
		t.Fatalf("rep 0 = %q, want empty", v.String())
	}
	if v := evalRun(t, e, context.Background(), `return string.rep("a", -5)`); v.String() != "" {
		t.Fatalf("rep -5 = %q, want empty", v.String())
	}
}

func TestSandboxFormatWidthCap(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))

	// Normal format passes through.
	if v := evalRun(t, e, context.Background(), `return string.format("%s=%d", "a", 7)`); v.String() != "a=7" {
		t.Fatalf("format normal = %q, want a=7", v.String())
	}

	// Same single-native-op escape class as string.rep (issue #521):
	// strFormat is a raw fmt.Sprintf, so %99999999d allocates ~100MB
	// in one VM instruction.
	err := eval(t, e, context.Background(), `local x = string.format("%99999999d", 1)`)
	if err == nil {
		t.Fatal("oversized format width must error")
	}
	err = eval(t, e, context.Background(), `local x = string.format("%.99999999f", 1)`)
	if err == nil {
		t.Fatal("oversized format precision must error")
	}
}

func TestSandboxContextTimeoutInfiniteLoop(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := eval(t, e, ctx, `while true do end`)
	if err == nil {
		t.Fatal("infinite loop must hit the context deadline")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("timeout error = %q, want context deadline exceeded", err)
	}
}

func TestSandboxCallerCancelPropagates(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := eval(t, e, ctx, `while true do end`)
	if err == nil {
		t.Fatal("canceled context must abort the VM")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancel error = %q, want canceled", err)
	}
}

func TestSandboxCallStackCap(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))

	// A recursion bomb must surface a Lua error (stack overflow),
	// never a Go panic or a hang.
	err := eval(t, e, context.Background(), `local function f() return 1 + f() end; return f()`)
	if err == nil {
		t.Fatal("recursion bomb must error")
	}
}

func TestSandboxPackagePreloadOnly(t *testing.T) {
	t.Parallel()

	e := NewEngine(DefaultConfig(), mustLogger(t))
	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	L := e.NewState(sctx)
	defer L.Close()

	// Preload a module the way the SDK does; require must find it.
	L.PreloadModule("testmod", func(L *lua.LState) int {
		tbl := L.NewTable()
		tbl.RawSetH(lua.LString("answer"), lua.LNumber(42))
		L.Push(tbl)
		return 1
	})
	// Preload + require must run on the SAME state: fresh states are
	// isolated by design, so a preload never leaks across invocations.
	fn, err := L.Load(strings.NewReader(`return require("testmod").answer`), "preload-chunk")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	L.Push(fn)
	if err := L.PCall(0, 1, nil); err != nil {
		t.Fatalf("preload require: %v", err)
	}
	if v := L.Get(-1); v.String() != "42" {
		L.Pop(1)
		t.Fatalf("preload require = %s, want 42", v)
	}
	L.Pop(1)

	// package.path must be empty and the file searcher gone: require
	// can never touch the filesystem.
	if v := evalRun(t, e, context.Background(), `return package.path`); v.String() != "" {
		t.Fatalf("package.path = %q, want empty", v.String())
	}
	if v := evalRun(t, e, context.Background(), `return #package.loaders`); v.String() != "1" {
		t.Fatalf("#package.loaders = %s, want 1 (preload only)", v)
	}
	if err := eval(t, e, context.Background(), `require("evilmod")`); err == nil {
		t.Fatal("require of an unloaded module must fail")
	}
}
