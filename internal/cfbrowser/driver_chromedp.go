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
// Fingerprint posture: CloakBrowser compiles its stealth patches into
// the binary and auto-generates a random fingerprint seed at startup,
// so the launch needs no anti-detection flags — only the minimum:
// no-first-run, user-data-dir, lang, TZ env, proxy-server and the
// headless toggle.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// DisplayError reports a headed launch on linux without $DISPLAY.
type DisplayError struct{}

// Error implements error with the xvfb-run hint.
func (*DisplayError) Error() string {
	return "cfbrowser: headed browser needs a display on linux — " +
		"run under a desktop session, Xvfb (xvfb-run -a anicli …), or set cf.headed = false"
}

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

// chromedpDriver is the production DriverFactory.
func chromedpDriver(opts LaunchOptions) (Naviger, error) {
	if opts.Headed && runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {
		return nil, &DisplayError{}
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(opts.BinaryPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(opts.UserDataDir),
	)
	if !opts.Headed {
		allocOpts = append(allocOpts, chromedp.Headless)
	}
	if opts.ProxyURL != "" {
		allocOpts = append(allocOpts, chromedp.ProxyServer(opts.ProxyURL))
	}
	if opts.Locale != "" {
		allocOpts = append(allocOpts, chromedp.Flag("lang", opts.Locale))
	}
	if opts.Timezone != "" {
		// Chromium reads TZ from the environment.
		allocOpts = append(allocOpts, chromedp.Env("TZ="+opts.Timezone))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)

	// Force the process launch NOW so factory failures (bad binary,
	// no display) surface at launch(), where the solver retries them.
	if err := chromedp.Run(ctx); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, fmt.Errorf("start chromium: %w", err)
	}
	return &chromedpNav{
		ctx:         ctx,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
		run:         chromedp.Run,
	}, nil
}

// chromedpNav adapts one chromedp browser session to Naviger.
type chromedpNav struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc
	// run executes chromedp actions; the seam exists so tests can
	// verify the per-navigation bounding without a real browser.
	run func(ctx context.Context, actions ...chromedp.Action) error
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

	var st NavState
	var bodySnippet string
	var ua, lang string
	var click []float64

	err := n.run(nctx,
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
	ncookies, err := network.GetCookies().Do(nctx)
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
// (the best-effort Turnstile interaction).
func (n *chromedpNav) Click(ctx context.Context, x, y float64) error {
	press := input.DispatchMouseEvent(input.MousePressed, x, y).
		WithButton(input.Left).WithClickCount(1)
	release := input.DispatchMouseEvent(input.MouseReleased, x, y).
		WithButton(input.Left).WithClickCount(1)
	if err := press.Do(n.ctx); err != nil {
		return err
	}
	return release.Do(n.ctx)
}

// Close tears the browser session down.
func (n *chromedpNav) Close() error {
	n.cancelCtx()
	n.cancelAlloc()
	return nil
}
