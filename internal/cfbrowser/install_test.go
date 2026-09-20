package cfbrowser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// installFixture builds an httptest GitHub API serving one release
// whose linux-x64 asset is the given archive bytes with a correct
// digest, plus an optional tampered digest variant.
type installFixture struct {
	srv    *httptest.Server
	apiURL string
}

func newInstallFixture(t *testing.T, archive []byte, digestOverride string) *installFixture {
	t.Helper()
	digest := sha256Hex(archive)
	if digestOverride != "" {
		digest = digestOverride
	}
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, int64(len(archive)), digest, ""}},
		},
		map[string]string{linuxX64Asset: string(archive)},
	)
	return &installFixture{srv: srv, apiURL: srv.URL}
}

// fakeInstalledBinary creates cacheDir/chromium-<version>/<exec> with
// the platform executable in place, returning its path.
func fakeInstalledBinary(t *testing.T, cacheDir, version string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, VersionDirName(version))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "x")), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chrome"), []byte("fake-elf"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "chrome")
}

func linuxSpec(t *testing.T) PlatformSpec {
	t.Helper()
	spec, err := platformAssetFor("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestInstallReusesExistingBinary(t *testing.T) {
	probeAlways(t)
	cache := t.TempDir()
	existing := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	// Dead API: reuse must never touch the network.
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	info, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  deadURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != existing {
		t.Errorf("path = %q, want reused %q", info.Path, existing)
	}
	if info.Version != "146.0.7680.177.5" {
		t.Errorf("version = %q", info.Version)
	}
	if info.Channel != channelFree {
		t.Errorf("channel = %q, want %q", info.Channel, channelFree)
	}
}

func TestInstallBinaryPathOverrideWins(t *testing.T) {
	cache := t.TempDir()
	override := filepath.Join(cache, "custom-chrome")
	if err := os.WriteFile(override, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Cache also holds a discoverable binary that must be ignored.
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	info, err := Install(context.Background(), InstallOptions{
		CacheDir:   cache,
		BinaryPath: override,
		Platform:   linuxSpec(t),
		Logger:     testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != override {
		t.Errorf("path = %q, want override %q", info.Path, override)
	}
	if info.Channel != channelUser {
		t.Errorf("channel = %q, want %q", info.Channel, channelUser)
	}
}

func TestInstallPrefersNewestCached(t *testing.T) {
	probeAlways(t)
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	newer := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	info, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  "http://127.0.0.1:1", // unreachable: no download allowed
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != newer {
		t.Errorf("path = %q, want newest %q", info.Path, newer)
	}
}

func TestInstallDownloadsAndVerifies(t *testing.T) {
	probeAlways(t)
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome":        {0o755, "ELF"},
		"chromium-146.0.7680.177.5/resources.pak": {0o644, "pak"},
	})
	fx := newInstallFixture(t, archive, "")

	cache := t.TempDir()
	info, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  fx.apiURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	wantDir := filepath.Join(cache, "chromium-146.0.7680.177.5")
	if filepath.Dir(info.Path) != wantDir {
		t.Errorf("binary dir = %q, want %q", filepath.Dir(info.Path), wantDir)
	}
	if info.Version != "146.0.7680.177.5" || info.Dir != wantDir || info.Channel != channelFree {
		t.Errorf("info = %+v", info)
	}
	if data, err := os.ReadFile(filepath.Join(wantDir, "resources.pak")); err != nil || string(data) != "pak" { //nolint:gosec // test-owned temp path
		t.Errorf("resources.pak round-trip: %v", err)
	}
	// Work directory must be cleaned up.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("work dir %q leaked", e.Name())
		}
	}
}

func TestInstallTamperedDigestFailsLoud(t *testing.T) {
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF"},
	})
	fx := newInstallFixture(t, archive, "sha256:"+strings.Repeat("0", 64))

	cache := t.TempDir()
	_, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  fx.apiURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err == nil {
		t.Fatal("expected digest mismatch failure")
	}
	if !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("error must name the digest mismatch: %v", err)
	}
	// No chromium- dir and no work dir may survive.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "chromium-") || strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("residue %q must not survive a failed install", e.Name())
		}
	}
}

func TestInstallOfflineTypedError(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	_, err := Install(context.Background(), InstallOptions{
		CacheDir: t.TempDir(),
		APIBase:  deadURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err == nil {
		t.Fatal("expected offline error")
	}
	var offline *OfflineError
	if !errors.As(err, &offline) {
		t.Fatalf("want *OfflineError, got %T: %v", err, err)
	}
	if !strings.Contains(offline.Error(), ManualReleasesURL) {
		t.Errorf("offline error must carry manual URL hint: %v", offline)
	}
}

func TestInstallMissingDigestFailsLoud(t *testing.T) {
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF"},
	})
	// No API digest and no SHA256SUMS asset: nothing may install.
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, int64(len(archive)), "", ""}},
		},
		map[string]string{linuxX64Asset: string(archive)},
	)

	cache := t.TempDir()
	_, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  srv.URL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err == nil {
		t.Fatal("expected missing-digest failure — an unverifiable archive must not install")
	}
	var missing *MissingDigestError
	if !errors.As(err, &missing) {
		t.Fatalf("want *MissingDigestError, got %T: %v", err, err)
	}
	if missing.TagName != "chromium-v146.0.7680.177.5" || missing.AssetName != linuxX64Asset {
		t.Errorf("error must name release+asset: %+v", missing)
	}
	if !strings.Contains(missing.Error(), ManualReleasesURL) {
		t.Errorf("error must carry the manual URL hint: %v", missing)
	}
	// Nothing unpacked: no chromium- dir and no work dir residue.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "chromium-") || strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("residue %q must not survive a missing-digest failure", e.Name())
		}
	}
}

func TestResolveCacheDirEnv(t *testing.T) {
	t.Setenv("CLOAKBROWSER_CACHE_DIR", "/tmp/cf-env-cache")
	got, err := ResolveCacheDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/cf-env-cache" {
		t.Errorf("cache dir = %q, want env value", got)
	}
	// Explicit override beats env.
	got, err = ResolveCacheDir("/tmp/explicit")
	if err != nil || got != "/tmp/explicit" {
		t.Errorf("explicit = %q, %v", got, err)
	}
}

func TestScanCacheSkipsIncompleteDirs(t *testing.T) {
	cache := t.TempDir()
	// Complete older binary.
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")
	// Incomplete newer dir without the executable (interrupted
	// install): must be skipped, not chosen.
	if err := os.MkdirAll(filepath.Join(cache, "chromium-150.0.0.0.1"), 0o750); err != nil {
		t.Fatal(err)
	}

	bin, ok := scanCache(cache, linuxSpec(t))
	if !ok {
		t.Fatal("expected the complete 146.0.7680.177.4 dir to resolve")
	}
	if !strings.HasSuffix(bin.Path, filepath.Join("chromium-146.0.7680.177.4", "chrome")) {
		t.Errorf("path = %q", bin.Path)
	}
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit check is unix-only")
	}
}

// TestInstallReportsProgressCallback (PR80): the OnProgress seam feeds
// the CLI's colored pre-TUI progress line — integer percents, monotone,
// the installed version attached.
func TestInstallReportsProgressCallback(t *testing.T) {
	probeAlways(t)
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome":        {0o755, "ELF"},
		"chromium-146.0.7680.177.5/resources.pak": {0o644, "pak"},
	})
	fx := newInstallFixture(t, archive, "")

	var mu sync.Mutex
	type tick struct {
		pct     int
		version string
	}
	var ticks []tick
	info, err := Install(context.Background(), InstallOptions{
		CacheDir: t.TempDir(),
		APIBase:  fx.apiURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
		OnProgress: func(pct int, version string) {
			mu.Lock()
			defer mu.Unlock()
			ticks = append(ticks, tick{pct, version})
		},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ticks) == 0 {
		t.Fatal("OnProgress never fired")
	}
	last := 0
	for _, tk := range ticks {
		if tk.pct < last || tk.pct < 0 || tk.pct > 100 {
			t.Fatalf("non-monotone or out-of-range pct: %v", ticks)
		}
		if tk.version != "146.0.7680.177.5" {
			t.Errorf("version = %q", tk.version)
		}
		last = tk.pct
	}
	// The 5%-granularity throttle can swallow the exact 100 tick
	// (96 → 100 crosses 4); the renderer paints the final state on
	// Install success regardless.
	if ticks[len(ticks)-1].pct < 95 {
		t.Errorf("last pct = %d, want >= 95", ticks[len(ticks)-1].pct)
	}
	_ = info
}

// TestInstallRidesConfiguredProxy (PR80): with [cf] proxy set, the
// free-channel download/update traffic flows THROUGH the configured
// proxy (a transparent stub records and forwards to the fixture);
// the install completes only because the proxy carried every request.
func TestInstallRidesConfiguredProxy(t *testing.T) {
	probeAlways(t)
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome":        {0o755, "ELF"},
		"chromium-146.0.7680.177.5/resources.pak": {0o644, "pak"},
	})
	fx := newInstallFixture(t, archive, "")

	var seen atomic.Int64
	prox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		// Absolute-form request: forward to the recorded target host.
		target := "http://" + r.Host + r.URL.RequestURI()
		//nolint:gosec // G704: test-owned stub forwarding to the test fixture
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(prox.Close)

	if _, err := Install(context.Background(), InstallOptions{
		CacheDir: t.TempDir(),
		APIBase:  fx.apiURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
		ProxyURL: prox.URL,
	}); err != nil {
		t.Fatalf("install through the configured proxy: %v", err)
	}
	if seen.Load() == 0 {
		t.Fatal("the download traffic never touched the configured proxy")
	}
}
