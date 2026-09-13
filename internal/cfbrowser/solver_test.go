package cfbrowser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNav scripts a Naviger: the first navigations report a challenge
// page; after Click (or after clicksRequired reloads) the page turns
// clean and carries cf_clearance.
type fakeNav struct {
	mu                sync.Mutex
	navigates         atomic.Int64
	clicks            atomic.Int64
	reloadsToSolve    int           // navigations before the page solves
	needsClick        bool          // solve requires one click
	launchErr         error         // fail driver creation
	navErr            error         // fail Navigate
	blockNavigate     time.Duration // optional Navigate latency
	closeCount        atomic.Int64
	userAgent         string
	acceptLanguage    string
	extraCookies      []Cookie
	noClearanceCookie bool // solved page without cf_clearance
}

func (f *fakeNav) Navigate(_ context.Context, _ string) (NavState, error) {
	f.navigates.Add(1)
	if f.blockNavigate > 0 {
		time.Sleep(f.blockNavigate)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.navErr != nil {
		return NavState{}, f.navErr
	}
	solved := f.needsClick && f.clicks.Load() > 0
	if !f.needsClick {
		solved = f.navigates.Load() > int64(f.reloadsToSolve)
	}
	if !solved {
		return NavState{
			Title:          "Just a moment...",
			Body:           `<script src="/cdn-cgi/challenge-platform/h/b/orchestrate"></script>`,
			UserAgent:      f.userAgent,
			HasClickTarget: f.needsClick,
			ClickX:         150,
			ClickY:         30,
		}, nil
	}
	cookies := []Cookie{{Name: "session", Value: "s1", Domain: "animego.one", Path: "/"}}
	if !f.noClearanceCookie {
		cookies = append(cookies, Cookie{Name: "cf_clearance", Value: "cl-42", Domain: "animego.one", Path: "/"})
	}
	cookies = append(cookies, f.extraCookies...)
	return NavState{
		Title:          "AnimeGo — аниме",
		Body:           "<html><body>content</body></html>",
		Cookies:        cookies,
		UserAgent:      f.userAgent,
		AcceptLanguage: f.acceptLanguage,
	}, nil
}

func (f *fakeNav) Click(_ context.Context, _, _ float64) error {
	f.clicks.Add(1)
	return nil
}

func (f *fakeNav) Close() error {
	f.closeCount.Add(1)
	return nil
}

// solverHarness wires a Solver against a scripted fake driver.
type solverHarness struct {
	solver *Solver
	nav    *fakeNav
	store  *ClearanceStore
}

func newSolverHarness(t *testing.T, nav *fakeNav) *solverHarness {
	t.Helper()
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	cfg := SolverConfig{
		Headed:       false,
		SolveTimeout: 2 * time.Second,
		PollInterval: 10 * time.Millisecond,
		Store:        store,
		Logger:       testLogger(t),
		DriverFactory: func(LaunchOptions) (Naviger, error) {
			if nav.launchErr != nil {
				return nil, nav.launchErr
			}
			return nav, nil
		},
	}
	return &solverHarness{solver: NewSolver(cfg), nav: nav, store: store}
}

func TestSolveChallengePollsUntilSolved(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 2, userAgent: "UA/146", acceptLanguage: "ru-RU,ru;q=0.9"}
	h := newSolverHarness(t, nav)

	started := time.Now()
	c, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/anime", 2*time.Second)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if !c.HasCFClearance() || c.UserAgent != "UA/146" || c.AcceptLanguage != "ru-RU,ru;q=0.9" {
		t.Errorf("clearance = %+v", c)
	}
	if time.Since(started) > time.Second {
		t.Errorf("solve with fast poll took %v — poll loop broken", time.Since(started))
	}
	// Result persisted for the host.
	if got, ok := h.store.Get("animego.one"); !ok || !got.HasCFClearance() {
		t.Errorf("store after solve: %v %v", got, ok)
	}
}

func TestSolveChallengeTurnstileClickOnce(t *testing.T) {
	nav := &fakeNav{needsClick: true, userAgent: "UA/146"}
	h := newSolverHarness(t, nav)

	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", 2*time.Second); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if nav.clicks.Load() != 1 {
		t.Errorf("exactly ONE best-effort click expected, got %d", nav.clicks.Load())
	}
}

func TestSolveChallengeStoreShortCircuit(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 1}
	h := newSolverHarness(t, nav)

	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatal(err)
	}
	before := nav.navigates.Load()
	for range 5 {
		c, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/other", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !c.HasCFClearance() {
			t.Fatal("stored clearance must be replayed")
		}
	}
	if nav.navigates.Load() != before {
		t.Errorf("store hits must not re-navigate: %d -> %d", before, nav.navigates.Load())
	}
}

func TestSolveChallengeSingleFlightPerHost(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 3, blockNavigate: 30 * time.Millisecond, userAgent: "UA"}
	h := newSolverHarness(t, nav)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.solver.SolveChallenge(context.Background(), "https://animego.one/", 5*time.Second)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// One solve's poll iterations only — not n parallel navigations.
	if got := nav.navigates.Load(); got > 6 {
		t.Errorf("single in-flight solve per host expected, %d navigations", got)
	}
}

func TestSolveChallengeTimeoutTypedError(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 1 << 30} // never solves
	h := newSolverHarness(t, nav)

	_, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
	var timeout *SolveTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("want *SolveTimeoutError, got %T: %v", err, err)
	}
}

func TestSolveChallengeNavigateFailureWrapped(t *testing.T) {
	nav := &fakeNav{navErr: errors.New("cdp exploded")}
	h := newSolverHarness(t, nav)
	_, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", time.Second)
	if err == nil || errors.Is(err, nav.navErr) == false {
		t.Fatalf("navigate failure must wrap through: %v", err)
	}
}

func TestSolveChallengeCleanPageWithoutClearanceCookie(t *testing.T) {
	// A false-positive challenge that renders clean without ever
	// setting cf_clearance must return the (cookie-less) state, not
	// spin to timeout.
	nav := &fakeNav{reloadsToSolve: 1, noClearanceCookie: true, userAgent: "UA"}
	h := newSolverHarness(t, nav)
	c, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", 2*time.Second)
	if err != nil {
		t.Fatalf("clean page must solve without cf_clearance: %v", err)
	}
	if c.HasCFClearance() {
		t.Errorf("no clearance cookie was scripted, got %+v", c.Cookies)
	}
}

func TestSolveChallengeHostOf(t *testing.T) {
	cases := map[string]string{
		"https://animego.one/anime/naruto": "animego.one",
		"http://gogoanime3.co":             "gogoanime3.co",
		"https://api.aniliberty.top:443/v": "api.aniliberty.top",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hostOf(u); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSolverCloseStopsDriver(t *testing.T) {
	nav := &fakeNav{reloadsToSolve: 1}
	h := newSolverHarness(t, nav)
	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := h.solver.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if nav.closeCount.Load() != 1 {
		t.Errorf("driver must be closed exactly once, got %d", nav.closeCount.Load())
	}
}

func TestSolveChallengeLaunchFailureRetryable(t *testing.T) {
	nav := &fakeNav{launchErr: fmt.Errorf("no such file"), userAgent: "UA"}
	h := newSolverHarness(t, nav)
	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err == nil {
		t.Fatal("launch failure must surface")
	}
	// Binary appears (launch heals): the next solve must retry the
	// launch instead of caching the failure.
	nav.mu.Lock()
	nav.launchErr = nil
	nav.mu.Unlock()
	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatalf("retry after launch heal: %v", err)
	}
}

func TestSolveNotBlockedBySlowUpdater(t *testing.T) {
	// Deferred update with a slow API: solving must proceed on the
	// current binary while the update crawls in the background.
	nav := &fakeNav{reloadsToSolve: 1, userAgent: "UA"}
	h := newSolverHarness(t, nav)

	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)
	fx.delay.Store(int64(3 * time.Second))

	u := NewUpdater(UpdaterConfig{
		Enabled: true, Interval: time.Hour,
		APIBase: fx.srv.URL, ProbeURL: fx.srv.URL,
		CacheDir: cache, Logger: testLogger(t),
	})
	defer u.Close()
	// Force deferred state so PreSolveKick actually fires.
	u.record(cache, UpdateStatus{Deferred: true, InstalledVersion: "146.0.7680.177.4"})

	h.solver.SetUpdater(u)
	start := time.Now()
	if _, err := h.solver.SolveChallenge(context.Background(), "https://animego.one/", 3*time.Second); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slow update must not block solve, took %v", elapsed)
	}
}
