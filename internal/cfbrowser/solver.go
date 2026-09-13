package cfbrowser

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Solve-path defaults (overridable through SolverConfig).
const (
	DefaultSolveTimeout = 90 * time.Second
	solvePollInterval   = 2 * time.Second
)

// SolveTimeoutError reports a challenge that did not clear within the
// solve budget.
type SolveTimeoutError struct {
	// Host is the challenged host.
	Host string
}

// Error implements error with the fallback hint.
func (e *SolveTimeoutError) Error() string {
	return fmt.Sprintf("cfbrowser: challenge on %s did not clear in time; "+
		"retry the request or run `anicli cf solve`", e.Host)
}

// NavState is the settled page state one navigation yields — the
// entire vocabulary the solver core understands. The chromedp driver
// is a thin adapter producing NavState; tests script it with fakes.
type NavState struct {
	// Title is document.title.
	Title string
	// Body is page markup used for challenge fingerprinting.
	Body string
	// Cookies are the browser cookies for the visited origin.
	Cookies []Cookie
	// UserAgent is navigator.userAgent.
	UserAgent string
	// AcceptLanguage is navigator language / the Accept-Language the
	// browser would send.
	AcceptLanguage string
	// HasClickTarget reports a visible Turnstile checkbox; ClickX/Y
	// are its CSS-pixel center inside the viewport.
	HasClickTarget bool
	ClickX, ClickY float64
}

// Naviger abstracts the browser driver: navigate-and-inspect plus a
// best-effort coordinate click. Implemented by the chromedp adapter
// (driver_chromedp.go) and by test fakes.
type Naviger interface {
	// Navigate loads url, waits for the page to settle and returns
	// its state.
	Navigate(ctx context.Context, url string) (NavState, error)
	// Click dispatches a viewport mouse click at CSS coordinates.
	Click(ctx context.Context, x, y float64) error
	// Close releases the browser session.
	Close() error
}

// LaunchOptions are the browser launch parameters handed to a driver
// factory. Headless-only by design ruling: no field can reintroduce
// a windowed mode.
type LaunchOptions struct {
	// BinaryPath is the stealth-Chromium executable.
	BinaryPath string
	// ProxyURL routes the browser through the same proxy as netclient.
	ProxyURL string
	// Timezone/Locale align the fingerprint (geoip).
	Timezone, Locale string
	// UserDataDir is the persistent profile directory.
	UserDataDir string
}

// DriverFactory builds a Naviger for one browser session.
type DriverFactory func(LaunchOptions) (Naviger, error)

// SolverConfig parameterizes the Solver.
type SolverConfig struct {
	// ProxyURL is netclient's proxy, replayed into the browser.
	ProxyURL string
	// Timezone/Locale fingerprint alignment (geoip-resolved upstream
	// when empty and a proxy is set; system defaults otherwise).
	Timezone, Locale string
	// UserDataDir overrides DataDir()/cfprofile.
	UserDataDir string
	// SolveTimeout bounds one challenge solve (default 90s).
	SolveTimeout time.Duration
	// PollInterval paces the solve poll loop (default 2s).
	PollInterval time.Duration
	// BrowserIdleTimeout is how long an idle session survives after
	// the last solve before teardown (default DefaultBrowserIdleTimeout
	// = 15s; 0 closes immediately; negative selects the default).
	BrowserIdleTimeout time.Duration
	// Store persists clearances; required.
	Store *ClearanceStore
	// Logger receives solve diagnostics (nil = slog.Default()).
	Logger *slog.Logger
	// GeoEndpoint overrides the geoip lookup URL (tests).
	GeoEndpoint string
	// Binary pins the resolved binary; nil resolves lazily at first
	// solve — CLOAKBROWSER_BINARY_PATH > newest complete cache dir —
	// so freshly auto-updated binaries are picked up.
	Binary *BinaryInfo
	// DriverFactory builds the Naviger (nil = chromedp driver).
	DriverFactory DriverFactory
}

func (c SolverConfig) solveTimeout() time.Duration {
	if c.SolveTimeout <= 0 {
		return DefaultSolveTimeout
	}
	return c.SolveTimeout
}

func (c SolverConfig) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return solvePollInterval
	}
	return c.PollInterval
}

func (c SolverConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Solver harvests Cloudflare clearances: store-first, then a
// single-flighted per-host browser solve driven through a Naviger.
// The browser session is EPHEMERAL (session.go): launched on the
// first unserved solve, shared under a refcount, torn down when idle
// for BrowserIdleTimeout — the ~300-600 MB RSS is resident only
// while actually solving. Launch stays lazy and non-sticky (a failed
// launch — binary missing — is retried on the next solve; a crashed
// session is discarded and relaunched the same way).
type Solver struct {
	cfg SolverConfig

	updaterMu sync.RWMutex
	updater   *Updater

	pool *sessionPool

	flight singleflight.Group
}

// NewSolver builds an idle Solver; the browser launches on first
// unserved solve.
func NewSolver(cfg SolverConfig) *Solver {
	return &Solver{cfg: cfg, pool: newSessionPool(cfg)}
}

// SetUpdater attaches the auto-updater whose PreSolveKick fires
// immediately before solves (non-blocking) and whose fresh binaries
// the lazy launch picks up.
func (s *Solver) SetUpdater(u *Updater) {
	s.updaterMu.Lock()
	s.updater = u
	s.updaterMu.Unlock()
}

// SolveChallenge returns a live clearance for the host of targetURL:
// the store first, then one single-flighted browser solve. timeout
// <= 0 selects SolverConfig.SolveTimeout.
func (s *Solver) SolveChallenge(ctx context.Context, targetURL string, timeout time.Duration) (Clearance, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return Clearance{}, fmt.Errorf("cfbrowser: parse target %q: %w", targetURL, err)
	}
	host := hostOf(u)
	if host == "" {
		return Clearance{}, fmt.Errorf("cfbrowser: target %q has no host", targetURL)
	}
	if c, ok := s.cfg.Store.Get(host); ok {
		return c, nil
	}

	v, err, _ := s.flight.Do("host:"+host, func() (any, error) {
		// Store may have been filled while waiting on the flight.
		if c, ok := s.cfg.Store.Get(host); ok {
			return c, nil
		}
		return s.solveHost(ctx, targetURL, host, timeout)
	})
	if err != nil {
		return Clearance{}, err
	}
	return v.(Clearance), nil
}

// solveHost runs the poll loop against the driver inside one
// acquired ephemeral session slot.
func (s *Solver) solveHost(ctx context.Context, targetURL, host string, timeout time.Duration) (Clearance, error) {
	logger := s.cfg.logger()

	// Retry trigger: a deferred update gets its chance right before
	// the solve — strictly non-blocking.
	s.updaterMu.RLock()
	updater := s.updater
	s.updaterMu.RUnlock()
	if updater != nil {
		updater.PreSolveKick()
	}

	nav, sctx, release, err := s.pool.acquire(ctx)
	if err != nil {
		return Clearance{}, err
	}
	defer release()

	if timeout <= 0 {
		timeout = s.cfg.solveTimeout()
	}
	ctx, cancel := context.WithTimeout(sctx, timeout)
	defer cancel()

	logger.Info("cfbrowser: solving challenge", "host", host, "timeout", timeout)

	state, navErr := nav.Navigate(ctx, targetURL)
	clicked := false
	for {
		if navErr != nil {
			// A crashed browser must not poison the next solve:
			// dead sessions are torn down now, the next solve
			// relaunches (lazy retry).
			s.pool.discardIfDead(nav)
			return Clearance{}, fmt.Errorf("cfbrowser: navigate %s: %w", host, navErr)
		}
		if SolvedState(state.Title, state.Body, cookiesHaveCFClearance(state.Cookies)) {
			c := Clearance{
				Cookies:        state.Cookies,
				UserAgent:      state.UserAgent,
				AcceptLanguage: state.AcceptLanguage,
				Obtained:       time.Now(),
			}
			if err := s.cfg.Store.Put(host, c); err != nil {
				logger.Warn("cfbrowser: persist clearance", "host", host, "error", err)
			}
			logger.Info("cfbrowser: challenge solved", "host", host,
				"cf_clearance", c.HasCFClearance(), "user_agent", c.UserAgent)
			return c, nil
		}

		// One best-effort interactive click on the Turnstile
		// checkbox. Non-fatal by design: solving is headless-only, so an
		// interactive challenge that resists the scripted click simply
		// runs to the solve budget and reports a typed timeout.
		if !clicked && state.HasClickTarget {
			clicked = true
			logger.Info("cfbrowser: attempting turnstile click", "host", host,
				"x", state.ClickX, "y", state.ClickY)
			if err := nav.Click(ctx, state.ClickX, state.ClickY); err != nil {
				logger.Warn("cfbrowser: turnstile click failed (non-fatal)", "host", host, "error", err)
			}
		}

		if ctx.Err() != nil {
			return Clearance{}, &SolveTimeoutError{Host: host}
		}
		if err := sleepCtx(ctx, s.cfg.pollInterval()); err != nil {
			return Clearance{}, &SolveTimeoutError{Host: host}
		}
		state, navErr = nav.Navigate(ctx, targetURL)
	}
}

// Close releases the browser session (idempotent). It CANCELS
// in-flight solves — it does not wait for them — and kills the
// browser process group synchronously.
func (s *Solver) Close() error {
	return s.pool.Close()
}

// cookiesHaveCFClearance scans a cookie slice for cf_clearance.
func cookiesHaveCFClearance(cookies []Cookie) bool {
	for _, c := range cookies {
		if c.Name == "cf_clearance" {
			return true
		}
	}
	return false
}

// hostOf renders a URL's hostname (port stripped).
func hostOf(u *url.URL) string {
	return u.Hostname()
}

// sleepCtx waits for d unless ctx finishes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
