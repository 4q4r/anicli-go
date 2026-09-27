// Package lua embeds a sandboxed Lua engine for user-written anime
// provider scripts (the mangal/luaprovider architecture): scripts
// drop into ~/.config/anicli/providers/<id>/main.lua, are discovered
// at startup, and register alongside the built-in Go providers.
//
// Sandbox boundary (the kikito/sandbox.lua + homer GHSA-m726-p857-j3cc
// whitelist recipe): the VM is created with SkipOpenLibs and opens an
// explicit whitelist — base (minus the code/env escape hatches),
// table, string (minus dump, rep capped), math, coroutine and a
// preload-only package. os, io and debug are never opened; require
// resolves preloaded modules only, so a script cannot read files or
// spawn processes.
//
// Resource bounds (gopher-lua issue #521): the VM loop honors the
// installed context, but a SINGLE long native operation does not — so
// string.rep is shimmed to a byte budget and HTTP bodies are capped
// before they reach a script. Every invocation runs in a fresh LState
// (issue #197: thousands of states per process are cheap), so scripts
// carry no state between calls.
package lua

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/yuin/gopher-lua"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// SDKVersion is the anicli Lua SDK API version. Scripts compare it via
// anicli.version to guard against incompatible SDK changes.
const SDKVersion = "1"

// Options configure the sandbox budgets and the SDK's outside world.
// The zero value is usable: every field carries a sane default.
type Options struct {
	// Timeout bounds ONE provider invocation (script load + method
	// call). The VM loop checks it; native-op bypasses are mitigated
	// separately (RepLimit, BodyLimit).
	Timeout time.Duration
	// CallStackSize bounds Lua call-stack depth (runaway recursion).
	CallStackSize int
	// RegistrySize bounds the Lua data stack.
	RegistrySize int
	// RepLimit caps the RESULT byte size of one string.rep call.
	RepLimit int
	// BodyLimit caps the HTTP response body size handed to a script.
	BodyLimit int64
	// Log receives print() rebinds and SDK diagnostics.
	Log *slog.Logger
	// HTTP routes anicli.http through the app transport (proxy, UA,
	// retry policy shared with the built-in providers). Nil falls back
	// to a provider-less client at first use.
	HTTP *netclient.Client
}

// Engine is the sandboxed VM factory. It holds the budget options and
// hands out fresh, isolated LStates; it carries no script state.
type Engine struct {
	opts Options
}

// NewEngine fills the option defaults. The zero Options is valid.
func NewEngine(opts Options) *Engine {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.CallStackSize <= 0 {
		opts.CallStackSize = 1024
	}
	if opts.RegistrySize <= 0 {
		opts.RegistrySize = 20480
	}
	if opts.RepLimit <= 0 {
		opts.RepLimit = 1 << 20
	}
	if opts.BodyLimit <= 0 {
		opts.BodyLimit = 1 << 20
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Engine{opts: opts}
}

// Options returns the engine's effective (default-filled) options.
func (e *Engine) Options() Options { return e.opts }

// discardWriter sinks logger output for engines built without one.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// baseStripList names the base-library escape hatches: arbitrary code
// loading (dofile/loadfile/load/loadstring), environment swapping
// (getfenv/setfenv). print is not stripped — it is rebound to the
// wired logger.
var baseStripList = []string{
	"dofile", "loadfile", "load", "loadstring", "getfenv", "setfenv",
}

// NewState builds a fresh sandboxed LState with ctx installed as the
// VM's cancellation context. The caller owns L.Close(); the context
// deadline (opts.Timeout on top of the passed ctx) fires on its own.
func (e *Engine) NewState(ctx context.Context) (*lua.LState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	L := lua.NewState(lua.Options{
		SkipOpenLibs:  true,
		CallStackSize: e.opts.CallStackSize,
		RegistrySize:  e.opts.RegistrySize,
	})

	// The whitelist. Nothing outside these Open* calls exists.
	lua.OpenBase(L)
	lua.OpenTable(L)
	lua.OpenString(L)
	lua.OpenMath(L)
	lua.OpenCoroutine(L)
	lua.OpenPackage(L)

	e.stripBase(L)
	e.stripString(L)
	e.restrictPackage(L)
	e.rebindPrint(L)

	// The engine budget caps whatever deadline the caller passed:
	// one invocation never outlives opts.Timeout.
	wctx, _ := e.timeoutCtx(ctx)
	L.SetContext(wctx)
	return L, nil
}

// stripBase removes the code-loading and environment-swap escape
// hatches from the opened base library.
func (e *Engine) stripBase(L *lua.LState) {
	for _, name := range baseStripList {
		L.SetGlobal(name, lua.LNil)
	}
}

// stripString removes string.dump (portable bytecode for the stripped
// load) and shims string.rep to the engine's byte budget (issue #521:
// one native op evades the context check).
func (e *Engine) stripString(L *lua.LState) {
	str := L.GetGlobal("string").(*lua.LTable)
	str.RawSetString("dump", lua.LNil)
	str.RawSetString("rep", L.NewFunction(e.repShim))
}

// repShim is the capped string.rep: the result byte budget is checked
// before the allocation happens.
func (e *Engine) repShim(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	if n < 0 {
		L.Push(lua.LString(""))
		return 1
	}
	if int64(len(s))*int64(n) > int64(e.opts.RepLimit) {
		L.RaiseError("string.rep: %d bytes × %d exceeds the sandbox budget of %d bytes",
			len(s), n, e.opts.RepLimit)
		return 0
	}
	L.Push(lua.LString(strings.Repeat(s, n)))
	return 1
}

// restrictPackage turns the package library into preload-only require:
// loadlib (the C dlopen hatch) is removed, path/cpath are emptied and
// the loader chain (which reads files from disk) is replaced with a
// single preload resolver.
func (e *Engine) restrictPackage(L *lua.LState) {
	pkg := L.GetGlobal("package").(*lua.LTable)
	pkg.RawSetString("loadlib", lua.LNil)
	pkg.RawSetString("path", lua.LString(""))
	pkg.RawSetString("cpath", lua.LString(""))

	preloadLoader := L.NewFunction(preloadOnlyLoader)
	loaders := L.NewTable()
	loaders.RawSetInt(1, preloadLoader)
	pkg.RawSetString("loaders", loaders)
	L.SetField(L.Get(lua.RegistryIndex), "_LOADERS", loaders)
}

// preloadOnlyLoader resolves require() against package.preload and
// nothing else.
func preloadOnlyLoader(L *lua.LState) int {
	name := L.CheckString(1)
	preload := L.GetField(L.GetGlobal("package"), "preload")
	mod := L.GetField(preload, name)
	if mod == lua.LNil {
		L.RaiseError("module %q not found: the sandbox resolves package.preload only", name)
		return 0
	}
	L.Push(mod)
	return 1
}

// rebindPrint routes print() into the wired logger: a TUI process
// must not have its screen corrupted by script stdout.
func (e *Engine) rebindPrint(L *lua.LState) {
	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		parts := make([]string, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, L.Get(i).String())
		}
		e.opts.Log.Info("lua: print", "msg", strings.Join(parts, "\t"))
		return 0
	}))
}

// timeoutCtx derives the per-invocation deadline from the engine
// budget on top of the caller's context.
func (e *Engine) timeoutCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, e.opts.Timeout)
}
