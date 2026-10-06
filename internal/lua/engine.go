// Package lua embeds a sandboxed Lua engine for user-written anime
// provider scripts (the mangal/luaprovider architecture): scripts
// drop into ~/.config/anicli/providers/<id>/main.lua, are discovered
// at startup, and register alongside the built-in Go providers.
//
// Sandbox boundary (the kikito/sandbox.lua + homer GHSA-m726-p857-j3cc
// whitelist recipe): the VM is created with SkipOpenLibs and opens an
// explicit whitelist — base (minus the code/env escape hatches),
// table, string (minus dump; rep and format width capped), math,
// coroutine and a preload-only package. os, io and debug are never
// opened; require resolves preloaded modules only, so a script cannot
// read files, load C libraries or spawn processes.
//
// Resource bounds (gopher-lua issue #521): the VM loop honors the
// installed context, but a SINGLE long native operation does not — so
// string.rep and string.format carry byte/width budgets and HTTP
// bodies are capped before they reach a script. Every invocation runs
// in a fresh LState (issue #197: thousands of states per process are
// cheap), so scripts carry no state between calls.
package lua

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuin/gopher-lua"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// SDKVersion is the anicli Lua SDK API version. Scripts compare it via
// anicli.version to guard against incompatible SDK changes.
const SDKVersion = "1"

// Config carries the sandbox budgets. The zero value is not used
// directly — DefaultConfig fills the sane defaults; tests and
// embedding code override single fields.
type Config struct {
	// Timeout bounds ONE provider invocation (script load + method
	// call) on top of the caller's context. The VM loop checks it;
	// native-op bypasses are mitigated separately (RepLimit,
	// FormatWidthCap, BodyLimit).
	Timeout time.Duration
	// CallStackSize bounds Lua call-stack depth (runaway recursion).
	CallStackSize int
	// RegistrySize bounds the Lua data stack.
	RegistrySize int
	// RepLimit caps the RESULT byte size of one string.rep call.
	RepLimit int
	// FormatWidthCap caps the width/precision of one string.format
	// verb ("%99999999d" allocates ~100MB in a single VM instruction).
	FormatWidthCap int
	// BodyLimit caps the HTTP response body size handed to a script.
	BodyLimit int64
	// HTTP routes anicli.http through the app transport (proxy, UA,
	// retry policy shared with the built-in providers). Nil falls back
	// to a plain client at first use.
	HTTP *netclient.Client
	// ProviderSettings carries the flattened settings of the ONE
	// provider being loaded — the providers.<id>.<key> string values
	// anicli.provider_setting(key) reads (PR140; the loader and the
	// factory wire it per id through the same seam as HTTP). Nil means
	// the provider has no settings section: every read yields Lua nil.
	ProviderSettings map[string]string
	// Transport overrides the fallback client's round tripper (tests
	// redirect the provider's real base_url at an httptest server).
	Transport http.RoundTripper
}

// DefaultConfig returns the sandbox budgets used in production.
func DefaultConfig() Config {
	return Config{
		Timeout:        30 * time.Second,
		CallStackSize:  1024,
		RegistrySize:   20480,
		RepLimit:       1 << 20,
		FormatWidthCap: 4096,
		BodyLimit:      1 << 20,
	}
}

// Engine is the sandboxed VM factory. It holds the budgets and hands
// out fresh, isolated LStates; it carries no script state.
type Engine struct {
	cfg Config
	log *slog.Logger

	stdOnce sync.Once
	stdHTTP *http.Client

	// fbOnce/fbClient build the fallback netclient for anicli.extract
	// when no transport is wired (sdk_ext.go).
	fbOnce   sync.Once
	fbClient *netclient.Client
}

// stdClient is the fallback transport when no netclient is wired:
// deadlines come from the per-invocation context alone.
func (e *Engine) stdClient() *http.Client {
	e.stdOnce.Do(func() { e.stdHTTP = &http.Client{Transport: e.cfg.Transport} })
	return e.stdHTTP
}

// NewEngine builds an engine with the given budgets and logger (print
// rebinds and SDK diagnostics land there).
func NewEngine(cfg Config, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Engine{cfg: cfg, log: log}
}

// Config returns the engine's effective budget configuration.
func (e *Engine) Config() Config { return e.cfg }

// discardWriter sinks logger output for engines built without one.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// baseStripList names the base-library escape hatches: arbitrary code
// loading (dofile/loadfile/load/loadstring) and environment swapping
// (getfenv/setfenv). print is not stripped — it is rebound to the
// wired logger.
var baseStripList = []string{
	"dofile", "loadfile", "load", "loadstring", "getfenv", "setfenv",
}

// stateCtx derives the per-invocation deadline (cfg.Timeout) on top of
// the caller's context. The caller owns the cancel.
func (e *Engine) stateCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e.cfg.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.cfg.Timeout)
}

// NewState builds a fresh sandboxed LState with ctx installed as the
// VM's cancellation context. The caller owns ls.Close() and the
// context's cancel.
func (e *Engine) NewState(ctx context.Context) *lua.LState {
	if ctx == nil {
		ctx = context.Background()
	}
	ls := lua.NewState(lua.Options{
		SkipOpenLibs:  true,
		CallStackSize: e.cfg.CallStackSize,
		RegistrySize:  e.cfg.RegistrySize,
	})

	// The whitelist. Nothing outside these Open* calls exists.
	lua.OpenBase(ls)
	lua.OpenTable(ls)
	lua.OpenString(ls)
	lua.OpenMath(ls)
	lua.OpenCoroutine(ls)
	lua.OpenPackage(ls)

	e.stripBase(ls)
	e.stripString(ls)
	e.restrictPackage(ls)
	e.rebindPrint(ls)
	e.openSDK(ls)

	ls.SetContext(ctx)
	return ls
}

// stripBase removes the code-loading and environment-swap escape
// hatches from the opened base library.
func (e *Engine) stripBase(ls *lua.LState) {
	for _, name := range baseStripList {
		ls.SetGlobal(name, lua.LNil)
	}
}

// stripString removes string.dump (portable bytecode for the stripped
// load) and shims string.rep / string.format with the engine budgets
// (issue #521: one native op evades the context check).
func (e *Engine) stripString(ls *lua.LState) {
	str := ls.GetGlobal("string").(*lua.LTable)
	str.RawSetString("dump", lua.LNil)
	str.RawSetString("rep", ls.NewFunction(e.repShim))

	origFormat := str.RawGetH(lua.LString("format"))
	str.RawSetString("format", ls.NewFunction(func(ls *lua.LState) int {
		if err := e.checkFormat(ls); err != nil {
			ls.RaiseError("%s", err.Error())
			return 0
		}
		nargs := ls.GetTop()
		args := make([]lua.LValue, nargs)
		for i := range nargs {
			args[i] = ls.Get(i + 1)
		}
		ls.Push(origFormat)
		for _, a := range args {
			ls.Push(a)
		}
		ls.Call(nargs, 1)
		return 1
	}))
}

// checkFormat validates every verb's width/precision against
// cfg.FormatWidthCap before the original strFormat allocates.
func (e *Engine) checkFormat(ls *lua.LState) error {
	if ls.GetTop() < 1 {
		return nil
	}
	f, ok := ls.Get(1).(lua.LString)
	if !ok {
		return nil // the original reports the type error
	}
	cap := e.cfg.FormatWidthCap
	inSpec, afterDot := false, false
	width, precision := 0, 0
	for _, r := range string(f) {
		switch {
		case !inSpec:
			inSpec = r == '%'
		case r == '%':
			// "%%" literal percent.
			inSpec, afterDot, width, precision = false, false, 0, 0
		case r == '.':
			afterDot = true
		case r >= '0' && r <= '9':
			if afterDot {
				precision = precision*10 + int(r-'0')
				if precision > cap {
					return fmt.Errorf("string.format: precision %d exceeds the sandbox cap of %d",
						precision, cap)
				}
			} else {
				width = width*10 + int(r-'0')
				if width > cap {
					return fmt.Errorf("string.format: width %d exceeds the sandbox cap of %d",
						width, cap)
				}
			}
		case r == '-' || r == '+' || r == ' ' || r == '#':
			// flags
		default:
			// The verb character closes the spec.
			inSpec, afterDot, width, precision = false, false, 0, 0
		}
	}
	return nil
}

// repShim is the capped string.rep: the result byte budget is checked
// before the allocation happens.
func (e *Engine) repShim(ls *lua.LState) int {
	s := ls.CheckString(1)
	n := ls.CheckInt(2)
	if n <= 0 {
		ls.Push(lua.LString(""))
		return 1
	}
	if int64(len(s))*int64(n) > int64(e.cfg.RepLimit) {
		ls.RaiseError("string.rep: %d bytes × %d exceeds the sandbox budget of %d bytes",
			len(s), n, e.cfg.RepLimit)
		return 0
	}
	ls.Push(lua.LString(strings.Repeat(s, n)))
	return 1
}

// restrictPackage turns the package library into preload-only require:
// loadlib (the C dlopen hatch) is removed, path/cpath are emptied and
// the loader chain (which reads files from disk) is replaced with a
// single preload resolver.
func (e *Engine) restrictPackage(ls *lua.LState) {
	pkg := ls.GetGlobal("package").(*lua.LTable)
	pkg.RawSetString("loadlib", lua.LNil)
	pkg.RawSetString("path", lua.LString(""))
	pkg.RawSetString("cpath", lua.LString(""))

	preloadLoader := ls.NewFunction(preloadOnlyLoader)
	loaders := ls.NewTable()
	loaders.RawSetInt(1, preloadLoader)
	pkg.RawSetString("loaders", loaders)
	ls.SetField(ls.Get(lua.RegistryIndex), "_LOADERS", loaders)
}

// preloadOnlyLoader resolves require() against package.preload and
// nothing else.
func preloadOnlyLoader(ls *lua.LState) int {
	name := ls.CheckString(1)
	preload := ls.GetField(ls.GetGlobal("package"), "preload")
	mod := ls.GetField(preload, name)
	if mod == lua.LNil {
		ls.RaiseError("module %q not found: the sandbox resolves package.preload only", name)
		return 0
	}
	ls.Push(mod)
	return 1
}

// rebindPrint routes print() into the wired logger: a TUI process
// must not have its screen corrupted by script stdout.
func (e *Engine) rebindPrint(ls *lua.LState) {
	ls.SetGlobal("print", ls.NewFunction(func(ls *lua.LState) int {
		parts := make([]string, 0, ls.GetTop())
		for i := 1; i <= ls.GetTop(); i++ {
			parts = append(parts, ls.Get(i).String())
		}
		e.log.Info("lua: print", "msg", strings.Join(parts, "\t"))
		return 0
	}))
}
