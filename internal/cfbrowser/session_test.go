package cfbrowser

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Session lifecycle tests: the browser exists ONLY while solves are
// in flight (plus the idle grace). Everything runs against fake
// drivers on a fake idle-timer clock — zero browsers, zero real
// waiting on the 15s default.

// ---- fake idle clock -------------------------------------------------

// fakeClock replaces the package afterFunc seam: armings are recorded
// and fired manually, Stop is honored exactly like a real timer.
type fakeClock struct {
	mu    sync.Mutex
	armed []*fakeArming
	wall  []time.Duration // durations handed to afterFunc, in order
}

type fakeArming struct {
	fire    func()
	stopped bool
}

type fakePoolTimer struct {
	a *fakeArming
}

func (t *fakePoolTimer) Stop() bool {
	t.a.stopped = true
	return true
}

func (fc *fakeClock) afterFunc(d time.Duration, f func()) poolTimer {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	a := &fakeArming{fire: f}
	fc.armed = append(fc.armed, a)
	fc.wall = append(fc.wall, d)
	return &fakePoolTimer{a: a}
}

// fire triggers the n-th arming unless it was stopped.
func (fc *fakeClock) fire(t *testing.T, n int) {
	t.Helper()
	fc.mu.Lock()
	a := fc.armed[n]
	fc.mu.Unlock()
	if a.stopped {
		return
	}
	a.fire()
}

func (fc *fakeClock) stopped(t *testing.T, n int) bool {
	t.Helper()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.armed[n].stopped
}

func (fc *fakeClock) armingCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.armed)
}

// swapClock installs fc as the idle-timer seam for one test.
func swapClock(t *testing.T, fc *fakeClock) {
	t.Helper()
	prev := afterFunc
	afterFunc = fc.afterFunc
	t.Cleanup(func() { afterFunc = prev })
}

// ---- crash fake -------------------------------------------------------

// crashNav is a driver whose browser dies mid-solve: Navigate fails
// and the session reports itself dead (liveness).
type crashNav struct {
	mu     sync.Mutex
	dead   bool
	closes atomic.Int64
}

func (c *crashNav) Navigate(_ context.Context, _ string) (NavState, error) {
	c.mu.Lock()
	c.dead = true
	c.mu.Unlock()
	return NavState{}, errors.New("target closed: browser crashed")
}

func (c *crashNav) Click(context.Context, float64, float64) error {
	return errors.New("dead session")
}

func (c *crashNav) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.dead
}

func (c *crashNav) Close() error { c.closes.Add(1); return nil }

// ---- helpers ----------------------------------------------------------

// newSessionSolver builds a Solver with a counting factory returning
// fresh fakeNavs, plus its own per-host stores are shared.
func newSessionSolver(t *testing.T, idle time.Duration, factory DriverFactory) (*Solver, *ClearanceStore) {
	t.Helper()
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	cfg := SolverConfig{
		SolveTimeout:       2 * time.Second,
		PollInterval:       5 * time.Millisecond,
		BrowserIdleTimeout: idle,
		Store:              store,
		Logger:             testLogger(t),
		DriverFactory:      factory,
	}
	return NewSolver(cfg), store
}

// countingFactory hands out fakeNavs and counts launches.
type countingFactory struct {
	mu       sync.Mutex
	launches int
	navs     []*fakeNav
}

func (cf *countingFactory) factory() DriverFactory {
	return func(LaunchOptions) (Naviger, error) {
		cf.mu.Lock()
		defer cf.mu.Unlock()
		nav := &fakeNav{reloadsToSolve: 0, userAgent: "UA"}
		cf.launches++
		cf.navs = append(cf.navs, nav)
		return nav, nil
	}
}

func (cf *countingFactory) count() int {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return cf.launches
}

// waitNavigates polls until the fake reports >= n navigations.
func waitNavigates(t *testing.T, nav *fakeNav, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for nav.navigates.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("fake nav never reached %d navigations (has %d)", n, nav.navigates.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- tests ------------------------------------------------------------

// TestSessionSharedAcrossConcurrentSolves: concurrent solves on
// different hosts share ONE browser session (refcount), and the
// session outlives both solves only until Close.
func TestSessionSharedAcrossConcurrentSolves(t *testing.T) {
	cf := &countingFactory{}
	solver, _ := newSessionSolver(t, time.Hour, cf.factory())

	const workers = 4
	var wg sync.WaitGroup
	started := make(chan struct{}, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			_, err := solver.SolveChallenge(context.Background(),
				"https://host.example/"+strings.Repeat("x", i), 2*time.Second)
			if err != nil {
				t.Errorf("solve: %v", err)
			}
		}()
	}
	// Ensure genuine overlap before any can finish.
	for range workers {
		<-started
	}
	wg.Wait()

	if got := cf.count(); got != 1 {
		t.Errorf("concurrent solves must share ONE session, factory launched %d", got)
	}
	for _, nav := range cf.navs {
		if nav.closeCount.Load() != 0 {
			t.Errorf("session must stay open under refcount, closeCount=%d", nav.closeCount.Load())
		}
	}
	if err := solver.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := cf.navs[0].closeCount.Load(); got != 1 {
		t.Errorf("Close must stop the shared session exactly once, got %d", got)
	}
}

// TestIdleCloseArmsDefaultTimeout: with the default idle timeout
// (negative selector) the 15s timer is armed after the last solve;
// firing it closes the session.
func TestIdleCloseArmsDefaultTimeout(t *testing.T) {
	fc := &fakeClock{}
	swapClock(t, fc)
	cf := &countingFactory{}
	solver, _ := newSessionSolver(t, -1, cf.factory())

	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := fc.armingCount(); got != 1 {
		t.Fatalf("exactly one idle timer must arm after the last solve, got %d", got)
	}
	if fc.wall[0] != DefaultBrowserIdleTimeout {
		t.Errorf("idle timer duration = %v, want default %v", fc.wall[0], DefaultBrowserIdleTimeout)
	}
	if got := cf.navs[0].closeCount.Load(); got != 0 {
		t.Fatalf("session must survive until the timer fires, closeCount=%d", got)
	}
	fc.fire(t, 0)
	if got := cf.navs[0].closeCount.Load(); got != 1 {
		t.Errorf("idle timer fire must close the session, closeCount=%d", got)
	}
}

// TestIdleCloseLogsReason: session_close carries reason=idle.
func TestIdleCloseLogsReason(t *testing.T) {
	var buf bytes.Buffer
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	fc := &fakeClock{}
	swapClock(t, fc)
	solver := NewSolver(SolverConfig{
		SolveTimeout:       2 * time.Second,
		PollInterval:       5 * time.Millisecond,
		BrowserIdleTimeout: time.Minute,
		Store:              store,
		Logger:             logger,
		DriverFactory: func(LaunchOptions) (Naviger, error) {
			return &fakeNav{reloadsToSolve: 0, userAgent: "UA"}, nil
		},
	})
	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	fc.fire(t, 0)
	out := buf.String()
	if !strings.Contains(out, "session_open") {
		t.Errorf("session_open must be logged, got: %s", out)
	}
	if !strings.Contains(out, "session_close") || !strings.Contains(out, "reason=idle") {
		t.Errorf("session_close reason=idle must be logged, got: %s", out)
	}
}

// TestIdleCloseImmediateOnZero: browser_idle_timeout = 0 closes the
// session synchronously when the last solve finishes — no timer.
func TestIdleCloseImmediateOnZero(t *testing.T) {
	fc := &fakeClock{}
	swapClock(t, fc)
	cf := &countingFactory{}
	solver, _ := newSessionSolver(t, 0, cf.factory())

	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := cf.navs[0].closeCount.Load(); got != 1 {
		t.Fatalf("0s idle must close immediately after the last solve, closeCount=%d", got)
	}
	if got := fc.armingCount(); got != 0 {
		t.Errorf("0s idle must not arm a timer, got %d armings", got)
	}
}

// TestIdleTimerCancelledByNewSolve: a solve arriving before the idle
// timer fires cancels it and reuses the session; the re-armed timer
// after THAT solve is what eventually closes.
func TestIdleTimerCancelledByNewSolve(t *testing.T) {
	fc := &fakeClock{}
	swapClock(t, fc)
	cf := &countingFactory{}
	solver, _ := newSessionSolver(t, time.Minute, cf.factory())

	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if fc.armingCount() != 1 {
		t.Fatalf("expected first idle timer armed, got %d", fc.armingCount())
	}
	// New solve before the timer fires: cancels it, reuses session.
	if _, err := solver.SolveChallenge(context.Background(), "https://b.example/", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := cf.count(); got != 1 {
		t.Errorf("session must be reused by the second solve, relaunched %d times", got)
	}
	if !fc.stopped(t, 0) {
		t.Error("the pending idle timer must be cancelled by a new solve")
	}
	// Firing a properly cancelled timer does nothing (fake honors Stop).
	fc.fire(t, 0)
	if got := cf.navs[0].closeCount.Load(); got != 0 {
		t.Fatalf("cancelled timer must not close the live session, closeCount=%d", got)
	}
	// The second solve's own release armed the closing timer.
	if fc.armingCount() != 2 {
		t.Fatalf("expected second idle timer armed, got %d", fc.armingCount())
	}
	fc.fire(t, 1)
	if got := cf.navs[0].closeCount.Load(); got != 1 {
		t.Errorf("second idle timer fire must close the session, closeCount=%d", got)
	}
}

// TestCrashedSessionTornDownAndRelaunched: a crashed session is torn
// down immediately (reason=crash) and the next solve relaunches (lazy
// retry preserved).
func TestCrashedSessionTornDownAndRelaunched(t *testing.T) {
	crashed := &crashNav{}
	var healthy *fakeNav
	launches := 0
	solver, _ := newSessionSolver(t, time.Hour, func(LaunchOptions) (Naviger, error) {
		launches++
		if launches == 1 {
			return crashed, nil
		}
		healthy = &fakeNav{reloadsToSolve: 0, userAgent: "UA"}
		return healthy, nil
	})

	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err == nil {
		t.Fatal("solve on a crashed browser must fail")
	}
	if got := crashed.closes.Load(); got != 1 {
		t.Fatalf("crashed session must be torn down at once, closeCount=%d", got)
	}
	// Next solve: fresh launch, fresh session.
	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", 2*time.Second); err != nil {
		t.Fatalf("solve after relaunch: %v", err)
	}
	if launches != 2 {
		t.Errorf("next solve must relaunch the browser, launches=%d", launches)
	}
	if healthy == nil || healthy.navigates.Load() == 0 {
		t.Error("the relaunched session must actually navigate")
	}
}

// TestSolverCloseCancelsInflight: Close does NOT wait for in-flight
// solves — it cancels them and kills the session synchronously.
// Idempotent on repeat calls.
func TestSolverCloseCancelsInflight(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 1 << 30, blockNavigate: 10 * time.Second, userAgent: "UA"}
	solver, _ := newSessionSolver(t, time.Hour, func(LaunchOptions) (Naviger, error) {
		return nav, nil
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := solver.SolveChallenge(context.Background(), "https://a.example/", 30*time.Second)
		errCh <- err
	}()
	waitNavigates(t, nav, 1)

	closed := make(chan error, 1)
	go func() { closed <- solver.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close must be synchronous")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled in-flight solve must surface an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close must cancel in-flight solves promptly")
	}
	if got := nav.closeCount.Load(); got != 1 {
		t.Errorf("session killed exactly once, closeCount=%d", got)
	}
	// Idempotent.
	if err := solver.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := nav.closeCount.Load(); got != 1 {
		t.Errorf("second close must be a no-op, closeCount=%d", got)
	}
}

// TestSolveAfterCloseFailsLoud: solves arriving after Close fail
// instead of resurrecting a browser.
func TestSolveAfterCloseFailsLoud(t *testing.T) {
	cf := &countingFactory{}
	solver, _ := newSessionSolver(t, time.Hour, cf.factory())
	if err := solver.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := solver.SolveChallenge(context.Background(), "https://a.example/", time.Second); err == nil {
		t.Fatal("solve after Close must fail loud")
	}
	if got := cf.count(); got != 0 {
		t.Errorf("no browser may launch after Close, launches=%d", got)
	}
}
