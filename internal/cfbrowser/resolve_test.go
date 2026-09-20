package cfbrowser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedValidLicenseCache primes the offline license state: KEY-1 in
// the env plus a fresh valid .license_cache entry, so
// cachedLicenseValid resolves true without network.
func seedValidLicenseCache(t *testing.T, cacheDir string) {
	t.Helper()
	t.Setenv(EnvLicenseKey, "KEY-1")
	writeLicenseCache(cacheDir, "KEY-1", LicenseStatus{Valid: true, Plan: "pro", Expires: "2099-01-01"})
}

func TestResolveCurrentBinaryPrefersProDirWithValidLicense(t *testing.T) {
	// Solve sessions pick pro when the license is valid: a
	// chromedp-compatible pro directory outranks the free scan even
	// when a free dir is newer. (The default bound now admits 150 but
	// still refuses 152 — the test raises it to 152 so BOTH sides are
	// compatible and the preference order is what's exercised.)
	old := maxKnownGoodChromiumMajor
	t.Cleanup(func() { maxKnownGoodChromiumMajor = old })
	maxKnownGoodChromiumMajor = 152

	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	fakeInstalledBinary(t, cache, "152.0.0.0.1") // free, newest
	proDir := filepath.Join(cache, VersionDirName("150.0.0.0.1"))
	fakeInstalledBinary(t, cache, "150.0.0.0.1")
	if err := os.WriteFile(filepath.Join(proDir, ".channel"), []byte("pro"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Version != "150.0.0.0.1" || bin.Channel != channelPro {
		t.Errorf("bin = %+v, want pro 150.0.0.0.1 (pro line outranks the newer free dir)", bin)
	}
}

// TestResolveCurrentBinaryAutoSkipsIncompatibleProDir is the post-lift
// live shape: a valid key plus a pro-marked 152 the verified bound
// (151) still refuses, next to a working free 146. Auto must launch
// the free line — never the above-bound pro build.
func TestResolveCurrentBinaryAutoSkipsIncompatibleProDir(t *testing.T) {
	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	proDir := filepath.Join(cache, VersionDirName("152.0.0.0.1"))
	fakeInstalledBinary(t, cache, "152.0.0.0.1")
	markProBinary(t, proDir)
	want := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: ChannelAuto})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want || bin.Channel != channelFree {
		t.Errorf("bin = %+v, want the compatible free line %q", bin, want)
	}
}

func TestResolveCurrentBinaryFreeChannelNeverPrefersPro(t *testing.T) {
	// channel=free ignores the pro preference even under a valid
	// cached license — a compatible pro dir stays untouched.
	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	proDir := filepath.Join(cache, VersionDirName("146.0.7680.177.9"))
	fakeInstalledBinary(t, cache, "146.0.7680.177.9")
	markProBinary(t, proDir)
	want := fakeInstalledBinary(t, cache, "145.0.0.0.1")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: channelFree})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want || bin.Channel != channelFree {
		t.Errorf("bin = %+v, want the free line %q (pro preference is not a free-channel thing)", bin, want)
	}
}

func TestResolveCurrentBinaryOnlyIncompatibleFreeIsCompatError(t *testing.T) {
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "152.0.0.0.1")

	_, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: ChannelAuto})
	var compat *CompatError
	if !errors.As(err, &compat) {
		t.Fatalf("want *CompatError, got %T: %v", err, err)
	}
	if compat.Newest != "152.0.0.0.1" {
		t.Errorf("compat = %+v", compat)
	}
}

func TestResolveCurrentBinaryUnknownChannelFailsLoud(t *testing.T) {
	_, err := ResolveCurrentBinary(ResolveOptions{CacheDir: t.TempDir(), Channel: "banana"})
	if err == nil || !strings.Contains(err.Error(), "unknown channel") {
		t.Fatalf("err = %v, want a loud unknown-channel error", err)
	}
}

func TestResolveCurrentBinaryValidLicenseOnlyFreeReportsFree(t *testing.T) {
	// A valid license with only a free-line binary cached: the solve
	// must keep working on the free binary and report it as what it
	// is — the free line — never a "pro" label on free bytes.
	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	free := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != free {
		t.Errorf("path = %q, want the free fallback %q (solves must keep working)", bin.Path, free)
	}
	if bin.Channel != channelFree {
		t.Errorf("channel = %q, want free (factual line of the resolved dir)", bin.Channel)
	}
}

func TestResolveCurrentBinaryPinnedReportsFactualChannel(t *testing.T) {
	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	fakeInstalledBinary(t, cache, "147.0.0.0.1") // free-line dir
	t.Setenv(EnvVersion, "147.0.0.0.1")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Version != "147.0.0.0.1" || bin.Channel != channelFree {
		t.Errorf("bin = %+v, want the pinned dir reported by its factual line (free)", bin)
	}
}

func TestResolveCurrentBinaryNoLicenseReportsFactualChannel(t *testing.T) {
	// Without a license the newest dir wins — reported by its actual
	// line, which may legitimately be a pro build downloaded earlier.
	// (channel=pro's degraded rung keeps the pre-PR73 unfiltered
	// scan; the free and auto channels filter pro-marked dirs out —
	// see TestResolveCurrentBinaryAutoSkipsIncompatibleProDir.)
	cache := t.TempDir()
	proDir := filepath.Join(cache, VersionDirName("151.0.0.0.1"))
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	fakeInstalledBinary(t, cache, "151.0.0.0.1")
	if err := os.WriteFile(filepath.Join(proDir, ".channel"), []byte("pro"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: channelPro})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Version != "151.0.0.0.1" || bin.Channel != channelPro {
		t.Errorf("bin = %+v, want the newest dir reported by its factual line (pro)", bin)
	}
}
