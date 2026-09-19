package cfbrowser

// Ephemeral session lifecycle (PR14 core requirement): the browser
// exists ONLY while solves are actually in flight, plus a bounded
// idle grace. Solve traffic is ~1 burst per 30-60min per challenged
// host, so a warm browser pool would pin ~300-600 MB RSS resident for
// the whole app lifetime to save a 300-600ms cold start; launching
// per burst and tearing down when idle is the correct trade.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultBrowserIdleTimeout is the [cf].browser_idle_timeout default:
// how long an idle session survives after the last solve finishes.
const DefaultBrowserIdleTimeout = 15 * time.Second

// errSolverClosed rejects solves arriving after Close.
var errSolverClosed = errors.New("cfbrowser: solver closed")

// poolTimer is the one-shot timer face the pool needs; time.AfterFunc
// is the production implementation and session tests swap a fake
// clock through the afterFunc seam.
type poolTimer interface {
	Stop() bool
}

// afterFunc arms f to run after d. Package var seam — session tests
// drive idle timers deterministically on a fake clock.
var afterFunc = func(d time.Duration, f func()) poolTimer {
	return time.AfterFunc(d, f)
}

// liveness is implemented by drivers that can report their own death
// (chromedpNav: the chromedp session context is done). It powers
// crash detection: a dead session is torn down immediately so the
// next solve relaunches (lazy retry semantics preserved).
type liveness interface {
	Alive() bool
}

// browserIdleTimeout resolves the idle grace: negative selects the
// default, zero closes immediately after the last solve.
func (c SolverConfig) browserIdleTimeout() time.Duration {
	if c.BrowserIdleTimeout < 0 {
		return DefaultBrowserIdleTimeout
	}
	return c.BrowserIdleTimeout
}

// solveRef tracks one in-flight solve's cancel so Close can cancel
// (not wait for) every solve riding the session.
type solveRef struct {
	cancel context.CancelFunc
}

// sessionPool owns the ephemeral browser session: launch on the
// first concurrent unserved solve, share it under a refcount, tear it
// down when the refcount drops to zero for browser_idle_timeout
// ("0s" = immediately), relaunch lazily on the next solve. All state
// is goroutine-safe under mu; Close is idempotent and synchronous.
type sessionPool struct {
	cfg SolverConfig

	mu        sync.Mutex
	nav       Naviger
	refs      int                    // in-flight solves using nav
	cancels   map[*solveRef]struct{} // in-flight solve cancels
	idleTimer poolTimer              // armed when refs == 0 && nav != nil
	closed    bool                   // explicit Close: pool is dead
}

func newSessionPool(cfg SolverConfig) *sessionPool {
	return &sessionPool{cfg: cfg, cancels: make(map[*solveRef]struct{})}
}

// acquire registers one solve: it disarms a pending idle close,
// launches the browser if none serves, derives a solve context the
// pool can cancel on Close, and returns a release func the caller
// MUST defer. Launch happens under mu — concurrent first solves
// serialize behind one launch instead of racing two browsers up.
func (p *sessionPool) acquire(ctx context.Context) (Naviger, context.Context, func(), error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, nil, errSolverClosed
	}
	// A solve arriving before the idle timer fires reuses the
	// session instead of letting it die.
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	p.refs++
	if p.nav == nil {
		if err := p.launchLocked(); err != nil {
			p.refs--
			p.mu.Unlock()
			return nil, nil, nil, err
		}
	}
	sctx, cancel := context.WithCancel(ctx)
	ref := &solveRef{cancel: cancel}
	p.cancels[ref] = struct{}{}
	nav := p.nav
	p.mu.Unlock()
	return nav, sctx, func() { p.release(ref) }, nil
}

// release retires one solve (idempotent via ref.cancel). The LAST
// release arms the idle timer — or closes immediately for a zero
// browser_idle_timeout.
func (p *sessionPool) release(ref *solveRef) {
	ref.cancel()
	p.mu.Lock()
	delete(p.cancels, ref)
	p.refs--
	if p.refs <= 0 && p.nav != nil && p.idleTimer == nil && !p.closed {
		if d := p.cfg.browserIdleTimeout(); d <= 0 {
			// Close errors are logged inside; idle teardown has no
			// caller to surface them to.
			_ = p.closeSessionLocked("idle")
		} else {
			p.idleTimer = afterFunc(d, p.idleClose)
		}
	}
	p.mu.Unlock()
}

// idleClose runs on the idle timer: if no solve arrived in the
// meantime, the session dies here — the whole point of the ephemeral
// lifecycle. The refs/nav re-checks make a racing acquire (which
// stopped the timer but could not un-fire it) a no-op.
func (p *sessionPool) idleClose() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idleTimer = nil
	if p.closed || p.refs > 0 || p.nav == nil {
		return
	}
	// Close errors are logged inside; idle teardown has no caller to
	// surface them to.
	_ = p.closeSessionLocked("idle")
}

// Close kills the session pool: in-flight solves are CANCELLED (Close
// deliberately does not wait for them — document this at the call
// sites) and the browser process group dies synchronously.
// Idempotent; solves after Close fail with errSolverClosed.
func (p *sessionPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	for ref := range p.cancels {
		ref.cancel()
	}
	p.cancels = make(map[*solveRef]struct{})
	err := p.closeSessionLocked("explicit")
	p.mu.Unlock()
	return err
}

// discardIfDead tears a crashed session down at once (reason=crash)
// so the next solve relaunches instead of hammering a corpse. No-op
// for live sessions, non-liveness drivers, or already-replaced
// sessions.
func (p *sessionPool) discardIfDead(nav Naviger) {
	l, ok := nav.(liveness)
	if !ok || l.Alive() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.nav != nav {
		return
	}
	// Close errors are logged inside; crash teardown has no caller
	// beyond the failing solve that triggered it.
	_ = p.closeSessionLocked("crash")
}

// closeSessionLocked tears the current session down; mu held. The
// driver Close runs under the lock so both idle and explicit teardown
// are synchronous with respect to new acquires (a solve arriving
// mid-teardown waits, then launches a fresh browser).
func (p *sessionPool) closeSessionLocked(reason string) error {
	nav := p.nav
	p.nav = nil
	p.cfg.logger().Info("cfbrowser: session_close", "reason", reason)
	if nav == nil {
		return nil
	}
	err := nav.Close()
	if err != nil {
		p.cfg.logger().Warn("cfbrowser: session close error", "reason", reason, "error", err)
	}
	return err
}

// launchLocked builds the driver lazily, preferring the newest
// complete cache binary at launch time (auto-updated binaries take
// effect on the next solve); mu held.
func (p *sessionPool) launchLocked() error {
	info := p.cfg.Binary
	if info == nil {
		resolved, err := ResolveCurrentBinary(ResolveOptions{Channel: p.cfg.Channel})
		if err != nil {
			return err
		}
		info = resolved
	}
	userDataDir := p.cfg.UserDataDir
	if userDataDir == "" {
		base, err := defaultProfileDir()
		if err != nil {
			return err
		}
		userDataDir = base
	}

	// Geoip alignment (proxy solves): the browser fingerprint must
	// match the egress IP. Direct solves keep system defaults.
	timezone, locale := p.cfg.Timezone, p.cfg.Locale
	if p.cfg.ProxyURL != "" && (timezone == "" || locale == "") {
		gi, err := resolveGeo(context.Background(), p.cfg.ProxyURL, p.cfg.GeoEndpoint, nil)
		if err != nil {
			// Strictly best-effort: slog-only, solve continues.
			p.cfg.logger().Warn("cfbrowser: geoip alignment failed", "error", err)
		} else {
			if timezone == "" {
				timezone = gi.Timezone
			}
			if locale == "" {
				locale = LocaleForCountry(gi.CountryCode)
			}
		}
	}

	factory := p.cfg.DriverFactory
	if factory == nil {
		factory = chromedpDriver
	}
	nav, err := factory(LaunchOptions{
		BinaryPath:  info.Path,
		ProxyURL:    p.cfg.ProxyURL,
		Timezone:    timezone,
		Locale:      locale,
		UserDataDir: userDataDir,
	})
	if err != nil {
		return fmt.Errorf("cfbrowser: launch stealth chromium %s: %w", info.Path, err)
	}
	p.cfg.logger().Info("cfbrowser: session_open", "binary", info.Path)
	p.nav = nav
	return nil
}
