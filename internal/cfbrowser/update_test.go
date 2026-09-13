package cfbrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// updateFixture serves a mutable release listing: assets can be
// re-pointed mid-test (first check old, then new) and the /releases
// hit count is tracked.
type updateFixture struct {
	srv    *httptest.Server
	mu     sync.Mutex
	tags   []string
	assets map[string][]ghAsset
	bodies map[string]string
	hits   atomic.Int64
	delay  atomic.Int64 // nanoseconds to sleep per /releases request
}

func newUpdateFixture(t *testing.T, tags []string, assets map[string][]ghAsset, bodies map[string]string) *updateFixture {
	t.Helper()
	fx := &updateFixture{tags: tags, assets: assets, bodies: bodies}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/CloakHQ/cloakbrowser/releases" {
			fx.hits.Add(1)
			if d := fx.delay.Load(); d > 0 {
				time.Sleep(time.Duration(d))
			}
			fx.mu.Lock()
			defer fx.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixture("http://"+r.Host, fx.tags, fx.assets)))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := fx.bodies[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		// Probe endpoint.
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fx.srv.Close)
	// Asset downloads from this fixture must pass the host allowlist
	// through the documented override.
	t.Setenv(EnvDownloadURL, fx.srv.URL)
	return fx
}

func releaseWith(asset string, archive []byte) ([]string, map[string][]ghAsset, map[string]string) {
	return []string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{asset, int64(len(archive)), sha256Hex(archive), ""}},
		},
		map[string]string{asset: string(archive)}
}

func updaterCfg(t *testing.T, fx *updateFixture, cache string) UpdaterConfig {
	t.Helper()
	return UpdaterConfig{
		Enabled:  true,
		Interval: time.Hour, // ticker off in these tests
		APIBase:  fx.srv.URL,
		ProbeURL: fx.srv.URL,
		CacheDir: cache,
		Logger:   testLogger(t),
	}
}

func TestUpdaterInstallsNewerKeepsPreviousPrunesThird(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "144.0.0.0.1") // third: must be pruned
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)

	u := NewUpdater(updaterCfg(t, fx, cache))
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	for _, dir := range []string{"chromium-146.0.7680.177.4", "chromium-146.0.7680.177.5"} {
		if fi, err := os.Stat(filepath.Join(cache, dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s must survive (rollback), got %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "chromium-144.0.0.0.1")); !os.IsNotExist(err) {
		t.Errorf("third-oldest dir must be pruned, stat err = %v", err)
	}

	st := u.Status()
	if st.InstalledVersion != "146.0.7680.177.5" || st.LatestVersion != "146.0.7680.177.5" {
		t.Errorf("status = %+v", st)
	}
	if st.Deferred {
		t.Error("update must not be deferred when online")
	}
	// Status file persisted.
	raw, err := os.ReadFile(filepath.Join(cache, updateStatusFile)) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("status file: %v", err)
	}
	if !strings.Contains(string(raw), "146.0.7680.177.5") {
		t.Errorf("status file content: %s", raw)
	}
}

func TestUpdaterNoOpWhenCurrentIsNewest(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	// API serves an OLDER release.
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.4/chrome": {0o755, "ELF-old"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, archive)
	fx := newUpdateFixture(t, tags, assets, bodies)

	u := NewUpdater(updaterCfg(t, fx, cache))
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		}
	}
	if dirs != 1 {
		t.Errorf("no download must happen for older release: %v", entries)
	}
}

func TestUpdaterOfflineMarksDeferred(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	cfg := UpdaterConfig{
		Enabled:  true,
		Interval: time.Hour,
		APIBase:  deadURL,
		ProbeURL: deadURL,
		CacheDir: cache,
		Logger:   testLogger(t),
	}
	u := NewUpdater(cfg)
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("deferred update is not a caller error: %v", err)
	}
	if !u.Status().Deferred {
		t.Fatal("offline check must mark the update deferred")
	}
	// No download residue (the status bookkeeping file is expected).
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.Name() == updateStatusFile {
			continue
		}
		if e.Name() != "chromium-146.0.7680.177.4" {
			t.Errorf("unexpected cache entry %q while offline", e.Name())
		}
	}
}

func TestUpdaterDeferredThenOnlineRetry(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)

	// Phase 1: API alive but probe dead (separate dead listener).
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	u := NewUpdater(UpdaterConfig{
		Enabled: true, Interval: time.Hour,
		APIBase: fx.srv.URL, ProbeURL: deadURL,
		CacheDir: cache, Logger: testLogger(t),
	})
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !u.Status().Deferred || u.Status().InstalledVersion != "146.0.7680.177.4" {
		t.Fatalf("phase 1 status: %+v", u.Status())
	}

	// Phase 2: probe comes back — retry installs.
	u.SetProbeURL(fx.srv.URL)
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.Status().Deferred {
		t.Fatalf("phase 2 must clear deferred: %+v", u.Status())
	}
	if u.Status().InstalledVersion != "146.0.7680.177.5" {
		t.Fatalf("phase 2 must install the new version: %+v", u.Status())
	}
}

func TestUpdaterDisabledHonored(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	fx := newUpdateFixture(t, nil, nil, nil)

	cfg := updaterCfg(t, fx, cache)
	cfg.Enabled = false
	u := NewUpdater(cfg)
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.hits.Load() != 0 {
		t.Errorf("disabled updater must not touch the API, hits = %d", fx.hits.Load())
	}
}

func TestUpdaterEnvDisables(t *testing.T) {
	t.Setenv("CLOAKBROWSER_AUTO_UPDATE", "false")
	if envAutoUpdateEnabled(true) {
		t.Fatal("CLOAKBROWSER_AUTO_UPDATE=false must disable")
	}
	t.Setenv("CLOAKBROWSER_AUTO_UPDATE", "1")
	if !envAutoUpdateEnabled(true) {
		t.Fatal("CLOAKBROWSER_AUTO_UPDATE=1 must keep it enabled")
	}
	t.Setenv("CLOAKBROWSER_AUTO_UPDATE", "")
	if !envAutoUpdateEnabled(true) {
		t.Fatal("empty env must fall back to the config value")
	}
	if envAutoUpdateEnabled(false) {
		t.Fatal("config false stays false without env")
	}
}

func TestUpdaterBinaryOverrideSkipsUpdates(t *testing.T) {
	cache := t.TempDir()
	override := filepath.Join(cache, "my-chrome")
	if err := os.WriteFile(override, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)

	cfg := updaterCfg(t, fx, cache)
	cfg.BinaryPath = override
	u := NewUpdater(cfg)
	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.hits.Load() != 0 {
		t.Errorf("override must skip the API entirely, hits = %d", fx.hits.Load())
	}
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "chromium-") {
			t.Errorf("override must not install cache dirs, found %q", e.Name())
		}
	}
}

func TestUpdaterTickerRetriesAndCloseStops(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	cfg := UpdaterConfig{
		Enabled: true, Interval: 40 * time.Millisecond,
		APIBase: fx.srv.URL, ProbeURL: deadURL,
		CacheDir: cache, Logger: testLogger(t),
	}
	u := NewUpdater(cfg)
	u.Start()
	defer u.Close()

	// Let one deferred tick fire, then heal the probe: the next tick
	// must install.
	time.Sleep(100 * time.Millisecond)
	u.SetProbeURL(fx.srv.URL)
	deadline := time.Now().Add(3 * time.Second)
	for u.Status().InstalledVersion != "146.0.7680.177.5" {
		if time.Now().After(deadline) {
			t.Fatalf("ticker retry never installed, status %+v", u.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Close stops the ticker: hit count must freeze.
	u.Close()
	settled := fx.hits.Load()
	time.Sleep(150 * time.Millisecond)
	if now := fx.hits.Load(); now > settled {
		t.Errorf("ticker must stop after Close: %d -> %d hits", settled, now)
	}
}

func TestUpdaterCloseCancelsInFlightCheck(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	// Probe answers instantly; the releases listing blocks until its
	// request context dies (client disconnect), proving cancellation.
	listingStarted := make(chan struct{})
	listingAborted := make(chan struct{})
	release := make(chan struct{}) // teardown escape hatch (see below)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/CloakHQ/cloakbrowser/releases" {
			close(listingStarted)
			select {
			case <-r.Context().Done():
				close(listingAborted) // genuine cancellation signal
			case <-release:
				// Test teardown only: lets the pre-fix (blocking) run
				// fail cleanly instead of wedging Server.Close.
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	// Registered after srv.Close: LIFO runs it first at teardown.
	t.Cleanup(func() { close(release) })

	u := NewUpdater(UpdaterConfig{
		Enabled: true, Interval: 10 * time.Millisecond,
		APIBase: srv.URL, ProbeURL: srv.URL,
		CacheDir: cache, Logger: testLogger(t),
	})
	u.Start()
	// No deferred Close: with the pre-fix synchronous loop a second
	// Close would block the test body on stopOnce; the asserted Close
	// below is the only one needed (and returns promptly post-fix).

	select {
	case <-listingStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("tick check never started")
	}

	closed := make(chan struct{})
	go func() { u.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(7 * time.Second):
		t.Fatal("Close must cancel an in-flight check instead of waiting out its 15m budget")
	}
	// The stalled listing must have been aborted by the cancellation.
	select {
	case <-listingAborted:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight check context was never cancelled")
	}
}

func TestUpdaterPreSolveKickNonBlocking(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)
	// Slow API: 2s per listing.
	fx.delay.Store(int64(2 * time.Second))

	u := NewUpdater(updaterCfg(t, fx, cache))
	start := time.Now()
	u.PreSolveKick()
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("PreSolveKick must be fire-and-forget, blocked for %v", elapsed)
	}
	u.Close()
}

func TestUpdaterSingleFlight(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	newArchive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF-new"},
	})
	tags, assets, bodies := releaseWith(linuxX64Asset, newArchive)
	fx := newUpdateFixture(t, tags, assets, bodies)
	fx.delay.Store(int64(300 * time.Millisecond))

	u := NewUpdater(updaterCfg(t, fx, cache))
	const parallel = 5
	done := make(chan error, parallel)
	for range parallel {
		go func() { done <- u.CheckAndMaybeInstall(context.Background()) }()
	}
	for range parallel {
		if err := <-done; err != nil {
			t.Fatalf("concurrent check: %v", err)
		}
	}
	if hits := fx.hits.Load(); hits > 2 {
		t.Errorf("singleflight must collapse concurrent checks, API hits = %d", hits)
	}
	u.Close()
}
