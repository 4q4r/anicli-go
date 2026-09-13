package cfbrowser

import (
	"os"
	"path/filepath"
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
	// Solve sessions pick pro when the license is valid: a pro
	// directory outranks the generic free scan even when a free dir
	// is newer.
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
	cache := t.TempDir()
	proDir := filepath.Join(cache, VersionDirName("151.0.0.0.1"))
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	fakeInstalledBinary(t, cache, "151.0.0.0.1")
	if err := os.WriteFile(filepath.Join(proDir, ".channel"), []byte("pro"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Version != "151.0.0.0.1" || bin.Channel != channelPro {
		t.Errorf("bin = %+v, want the newest dir reported by its factual line (pro)", bin)
	}
}
