package cfbrowser

// chromedp CDP driver — the deliberate THIN SHELL around the solver
// core (solver.go): everything challenge-related (poll loop, solved
// detection, singleflight, store) lives behind the Naviger interface
// and is unit-tested with fakes. This file only translates NavState
// into CDP round-trips and is verified by compilation + cross-compile
// matrix: spinning a real stealth Chromium in `go test` would flake
// on CI (no display, ~200 MB binary, network-gated), so runtime
// verification happens through `anicli cf solve` against live hosts.
//
// PR14 posture (headless-only + ephemeral low-memory session):
//   - HEADLESS ALWAYS: there is no headed mode and no flag that could
//     reintroduce one; the explicit argv below is the complete launch
//     posture (chromedp.DefaultExecAllocatorOptions are NOT used —
//     see buildAllocatorArgs).
//   - The browser runs in its own process group and Close kills the
//     whole group synchronously (platform split in prockill_*.go).
//   - Solve pages run on a resource diet (fetch domain): Image and
//     Media requests are denied; scripts, stylesheets, fonts, frames,
//     XHR/fetch and websockets flow untouched — the challenge needs
//     them (and Turnstile renders visually, so CSS and its webfonts
//     are sacred).
//
// Fingerprint posture: CloakBrowser compiles its stealth patches into
// the binary and auto-generates a random fingerprint seed at startup,
// so the launch needs no anti-detection flags.

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// bodySnippetBytes bounds the markup fetched for challenge
// fingerprinting (IsChallengePage only needs the head scripts).
const bodySnippetBytes = 64 * 1024

// turnstileProbe is the JS locating a visible Turnstile checkbox and
// returning its viewport center: [x, y] or null.
const turnstileProbe = `(function () {
  const sel = 'iframe[src*="challenges.cloudflare.com"], [class*="cf-turnstile"], [id*="turnstile"]';
  for (const el of document.querySelectorAll(sel)) {
    const r = el.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) {
      return [r.left + r.width / 2, r.top + r.height / 2];
    }
  }
  return null;
})()`

// blockedResourceTypes is the solve-page resource diet: the CDP
// request types denied at the network layer while challenges solve.
// Images and media dominate challenge-page weight and are irrelevant
// to clearing. Stylesheet and Font are deliberately NOT here —
// Turnstile renders visually (a broken widget cannot be clicked), the
// widget ships its own webfonts, and fetch-domain interception is
// armed on the browser session, so blocked types would starve the
// challenge iframe of its fonts too; fonts are cheap next to that
// risk. Script/XHR/Fetch/frames/WebSocket are the challenge machinery
// itself.
var blockedResourceTypes = map[network.ResourceType]struct{}{
	network.ResourceTypeImage: {},
	network.ResourceTypeMedia: {},
}

// blockedResourceType reports whether one CDP resource type is
// denied on solve pages.
func blockedResourceType(rt network.ResourceType) bool {
	_, ok := blockedResourceTypes[rt]
	return ok
}

// fetchBlockPatterns scopes fetch-domain interception to exactly the
// blocked resource types: only matching requests ever pause, so
// everything else flows with zero interception latency.
func fetchBlockPatterns() []*fetch.RequestPattern {
	patterns := make([]*fetch.RequestPattern, 0, len(blockedResourceTypes))
	for rt := range blockedResourceTypes {
		patterns = append(patterns, &fetch.RequestPattern{
			RequestStage: fetch.RequestStageRequest,
			ResourceType: rt,
		})
	}
	return patterns
}

// handlePausedRequest routes one paused request: blocked types fail
// with the standard client-blocked reason; anything else continues
// untouched (defensive — the typed patterns already scope the pauses).
func handlePausedRequest(ctx context.Context, ev *fetch.EventRequestPaused) {
	if blockedResourceType(ev.ResourceType) {
		_ = fetch.FailRequest(ev.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
		return
	}
	_ = fetch.ContinueRequest(ev.RequestID).Do(ctx)
}

// buildAllocatorArgs returns the FULL explicit chromium argv for one
// launch — the single source of truth; allocatorOptions derives the
// chromedp options from this very list and driver_launch_test.go pins
// the exact tables. chromedp.DefaultExecAllocatorOptions are
// deliberately replaced wholesale (they smuggle a second Headless
// toggle, --enable-automation and friends — the PR14 bug), so this
// list is the complete posture. chromedp itself still appends the CDP
// mechanics on top (--remote-debugging-port=0 when absent, about:blank
// as the first page, --no-sandbox when running as root) — allocator
// plumbing, not posture. Timezone travels as the TZ environment
// variable (chromedp.Env), not as an argv flag, matching Chromium's
// own resolution order. --disable-dev-shm-usage stays even though it
// trades shm for file-backed growth in long-lived browsers
// (chromedp#1627): our sessions are ephemeral by design.
//
// --enable-unsafe-swiftshader: Turnstile's challenge-platform JS
// requires WebGL to render its widget; with --disable-gpu the only
// WebGL provider is software (SwiftShader), and Chromium >=139
// deprecated the automatic software-WebGL fallback — without this
// flag headless launches have NO WebGL at all, the widget never
// renders (iframe count stays 0) and the challenge is unsolvable.
// Verified live on the challenged page via the official MCP browser
// console (2026-09-13): "Automatic fallback to software WebGL has
// been deprecated. Please use the --enable-unsafe-swiftshader flag".
// The name says "unsafe"; the exposure is bounded — this browser is
// headless, ephemeral and only ever visits challenge pages.
func buildAllocatorArgs(opts LaunchOptions) []string {
	args := []string{
		"--headless",
		"--disable-gpu",
		// Software WebGL so the Turnstile widget can render (see
		// doc comment above) despite --disable-gpu.
		"--enable-unsafe-swiftshader",
		"--disable-dev-shm-usage",
		"--disable-extensions",
		"--disable-background-networking",
		"--mute-audio",
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-translate",
		"--renderer-process-limit=1",
		"--disable-component-update",
		"--password-store=basic",
		"--use-mock-keychain",
	}
	args = append(args, "--user-data-dir="+opts.UserDataDir)
	if opts.ProxyURL != "" {
		args = append(args, "--proxy-server="+opts.ProxyURL)
	}
	if opts.Locale != "" {
		args = append(args, "--lang="+opts.Locale)
	}
	return args
}

// flagFromArg converts one argv entry into a chromedp.Flag(name,
// value) input, inverting chromedp's own rendering (string ->
// --name=value, boolean true -> bare --name; see allocate.go
// Allocate). Entries without '=' are boolean true flags.
func flagFromArg(arg string) (string, any) {
	s := strings.TrimPrefix(arg, "--")
	if i := strings.IndexByte(s, '='); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, true
}

// allocatorOptions derives the chromedp exec allocator options from
// buildAllocatorArgs — one list, one truth; the round-trip mapping is
// proven by TestFlagFromArgRoundTripMapping.
func allocatorOptions(opts LaunchOptions) []chromedp.ExecAllocatorOption {
	args := buildAllocatorArgs(opts)
	out := make([]chromedp.ExecAllocatorOption, 0, len(args))
	for _, arg := range args {
		name, value := flagFromArg(arg)
		out = append(out, chromedp.Flag(name, value))
	}
	return out
}

// withSessionExecutor derives a context carrying the chromedp
// target-session CDP executor. CRITICAL chromedp semantics (the PR15
// "invalid context" bug): chromedp.Run executes its actions on
// cdp.WithExecutor(ctx, c.Target) — a DERIVED context — while the
// context NewContext returned never carries the executor, so every
// direct cdproto Do(ctx) call on it (fetch.Enable,
// network.GetCookies, input.DispatchMouseEvent…) failed with
// cdp.ErrInvalidContext. A context that already carries an executor
// (tests inject fakes; derived navigation contexts inherit one)
// passes through unchanged; a targetless chromedp context passes
// through too (the caller's warn-only posture applies).
func withSessionExecutor(ctx context.Context) context.Context {
	if cdpExecutorFrom(ctx) != nil {
		return ctx
	}
	if c := chromedp.FromContext(ctx); c != nil && c.Target != nil {
		return cdp.WithExecutor(ctx, c.Target)
	}
	return ctx
}

// cdpExecutorFrom reads the cdp executor without ExecutorFromContext's
// absent-key panic.
func cdpExecutorFrom(ctx context.Context) cdp.Executor {
	defer func() { _ = recover() }() // absent key: typed-nil assertion panics
	return cdp.ExecutorFromContext(ctx)
}

// enableResourceDiet arms fetch-domain interception on the solve
// session: the listener attaches FIRST (no paused request may slip
// through unanswered), then Fetch.enable scopes the pauses to the
// blocked resource types. ctx must be a session-executor context —
// on a bare chromedp context Fetch.enable fails with
// "invalid context" and the factory degrades warn-only.
func enableResourceDiet(ctx context.Context) error {
	chromedp.ListenTarget(ctx, func(ev any) {
		if paused, ok := ev.(*fetch.EventRequestPaused); ok {
			handlePausedRequest(ctx, paused)
		}
	})
	return fetch.Enable().WithPatterns(fetchBlockPatterns()).Do(ctx)
}

// chromedpDriver is the production DriverFactory (headless-only).
func chromedpDriver(opts LaunchOptions) (Naviger, error) {
	// The browser's exec.Cmd is captured at spawn time via
	// ModifyCmdFunc (called synchronously inside chromedp.Run ->
	// Allocate, before cmd.Start) and read after Run returns on this
	// same goroutine — the ordering is linear, no lock needed.
	var cmd *exec.Cmd
	allocOpts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(opts.BinaryPath),
		// Own process group: group-scoped kills on close, and (linux)
		// Pdeathsig so a crashed anicli cannot orphan the browser.
		// This override displaces chromedp's default command setup —
		// prockill_linux.go replicates the Pdeathsig part of it.
		chromedp.ModifyCmdFunc(func(c *exec.Cmd) {
			setNewProcessGroup(c)
			cmd = c
		}),
	}
	allocOpts = append(allocOpts, allocatorOptions(opts)...)
	if opts.Timezone != "" {
		// Chromium reads TZ from the environment.
		allocOpts = append(allocOpts, chromedp.Env("TZ="+opts.Timezone))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)

	// Force the process launch NOW so factory failures (bad binary)
	// surface at launch(), where the solver retries them.
	if err := chromedp.Run(ctx); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, fmt.Errorf("start chromium: %w", err)
	}
	if cmd == nil || cmd.Process == nil {
		cancelCtx()
		cancelAlloc()
		return nil, fmt.Errorf("start chromium: no process handle captured")
	}

	// Resource diet (session-scoped, applies to every solve page):
	// the listener is attached BEFORE Fetch.enable so no paused
	// request can slip through unanswered. Both run on the
	// session-executor context — the bare chromedp context fails
	// Fetch.enable with "invalid context".
	if err := enableResourceDiet(withSessionExecutor(ctx)); err != nil {
		// Best-effort diet: a full-resource solve still works, just
		// fatter. Warn, do not fail the session over it.
		slog.Warn("cfbrowser: resource blocking unavailable", "error", err)
	}

	return &chromedpNav{
		ctx:         ctx,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
		cmd:         cmd,
		run:         chromedp.Run,
	}, nil
}

// chromedpNav adapts one chromedp browser session to Naviger.
type chromedpNav struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc
	// cmd is the spawned browser process (group kills on close).
	cmd *exec.Cmd
	// run executes chromedp actions; the seam exists so tests can
	// verify the per-navigation bounding without a real browser.
	run func(ctx context.Context, actions ...chromedp.Action) error
}

// Alive reports whether the browser session still breathes (crash
// detection for the session pool).
func (n *chromedpNav) Alive() bool {
	return n.ctx.Err() == nil
}

// Navigate loads rawURL, waits for the document to settle and
// snapshots the NavState (title, body snippet, cookies, UA, language,
// turnstile target).
//
// The browser session lives on n.ctx (created by NewContext, parented
// on Background — it must outlive individual solves), so the caller's
// context cannot parent it directly. Instead every navigation derives
// a bounded context from the session via navContext: the caller's
// solve budget as a hard deadline plus caller-cancellation bridging,
// so a hung page load can never pin the singleflighted solve past its
// 90s timeout.
func (n *chromedpNav) Navigate(ctx context.Context, rawURL string) (NavState, error) {
	// Entry gate: an already-expired caller fails fast.
	if err := ctx.Err(); err != nil {
		return NavState{}, err
	}
	nctx, cancel := n.navContext(ctx)
	defer cancel()
	// Session-executor context: chromedp actions run through Run's
	// own executor wrapping anyway, but the direct cdproto calls
	// below (GetCookies) need the executor explicitly — the bare
	// session context fails them with "invalid context".
	tctx := withSessionExecutor(nctx)

	var st NavState
	var bodySnippet string
	var ua, lang string
	var click []float64

	err := n.run(tctx,
		chromedp.Navigate(rawURL),
		// Challenge interstitials replace the document on solve;
		// WaitReady('body') + a short settle covers both states.
		chromedp.WaitReady(`body`, chromedp.ByQuery),
		chromedp.Sleep(solveSettle),
		chromedp.Title(&st.Title),
		chromedp.Evaluate(`document.documentElement.outerHTML.slice(0, `+
			fmt.Sprint(bodySnippetBytes)+`)`, &bodySnippet),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
		chromedp.Evaluate(`navigator.languages ? navigator.languages.join(",") : navigator.language`, &lang),
		chromedp.Evaluate(turnstileProbe, &click),
	)
	if err != nil {
		return NavState{}, err
	}

	st.Body = bodySnippet
	st.UserAgent = strings.TrimSpace(ua)
	st.AcceptLanguage = strings.TrimSpace(lang)
	if len(click) == 2 {
		st.HasClickTarget = true
		st.ClickX, st.ClickY = click[0], click[1]
	}

	// Cookie harvest via CDP (network.GetCookies) under the same
	// bounded navigation context.
	ncookies, err := network.GetCookies().Do(tctx)
	if err == nil {
		st.Cookies = make([]Cookie, 0, len(ncookies))
		for _, ck := range ncookies {
			st.Cookies = append(st.Cookies, Cookie{
				Name:   ck.Name,
				Value:  ck.Value,
				Domain: ck.Domain,
				Path:   ck.Path,
			})
		}
	} // Cookie harvest failure is non-fatal: title/body still gate.
	return st, nil
}

// solveSettle is the fixed post-load settle delay giving challenge
// scripts time to run/redirect before fingerprinting.
const solveSettle = 1500 * time.Millisecond

// navContext derives the per-navigation context: chromedp actions run
// against the session (a context derived from n.ctx inherits the
// session values) under the caller's solve budget — the caller's
// remaining deadline when it carries one, DefaultSolveTimeout
// otherwise — and abort as soon as the caller is cancelled. Without
// this bound a hung page load (server that never finishes responding,
// interstitial that never settles) would outlive the solver's outer
// timeout and pin the singleflighted solve indefinitely.
func (n *chromedpNav) navContext(caller context.Context) (context.Context, context.CancelFunc) {
	budget := DefaultSolveTimeout
	if dl, ok := caller.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			budget = rem
		}
	}
	nctx, cancel := context.WithTimeout(n.ctx, budget)
	if caller.Done() == nil {
		// The caller can never be cancelled: no bridge needed.
		return nctx, cancel
	}
	// Bridge the caller's cancellation into the session-derived
	// context (the session parents on Background, so this is the only
	// place the two lifetimes can meet).
	stop := make(chan struct{})
	go func() {
		select {
		case <-caller.Done():
			cancel()
		case <-stop:
		}
	}()
	var once sync.Once
	return nctx, func() {
		once.Do(func() {
			cancel()
			close(stop)
		})
	}
}

// Click dispatches one left-button click at viewport CSS coordinates
// (the best-effort Turnstile interaction). F55: the click runs under
// the same navContext budget as Navigate — it previously rode the
// session context unbounded and a wedged input dispatch could
// outlive the solve budget.
func (n *chromedpNav) Click(ctx context.Context, x, y float64) error {
	nctx, cancel := n.navContext(ctx)
	defer cancel()
	// Session-executor context: direct input dispatch on the bare
	// session context fails with "invalid context" (same class as
	// the F-fetch bug — the click could never land before PR15).
	tctx := withSessionExecutor(nctx)
	press := input.DispatchMouseEvent(input.MousePressed, x, y).
		WithButton(input.Left).WithClickCount(1)
	release := input.DispatchMouseEvent(input.MouseReleased, x, y).
		WithButton(input.Left).WithClickCount(1)
	if err := press.Do(tctx); err != nil {
		return err
	}
	return release.Do(tctx)
}

// Close tears the browser session down: the whole process group dies
// FIRST (leader + renderer/gpu/utility children, synchronously), then
// the chromedp contexts are cancelled to reap the process and drop
// the DevTools connection.
func (n *chromedpNav) Close() error {
	err := killProcessGroup(n.cmd)
	n.cancelCtx()
	n.cancelAlloc()
	if err != nil {
		return fmt.Errorf("cfbrowser: close chromium: %w", err)
	}
	return nil
}
