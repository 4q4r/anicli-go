package cfbrowser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// updaterProFixture reuses proInstallFixture and seeds a valid
// license + a current free-era binary, then drives the updater.
func TestUpdaterProChannelInstallsProVersion(t *testing.T) {
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)

	cache := t.TempDir()
	t.Setenv(EnvLicenseKey, "KEY-1")
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5") // free-era binary

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/api/license/validate",
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	st := up.Status()
	if st.UpdatedTo != "151.0.7922.108.6" {
		t.Errorf("status = %+v, want updated to the pro version", st)
	}
	if fx.proDownloadHits == 0 || fx.proVersionHits == 0 {
		t.Errorf("pro channel must have been used (version=%d download=%d)", fx.proVersionHits, fx.proDownloadHits)
	}
	if _, err := os.Stat(filepath.Join(cache, "chromium-151.0.7922.108.6")); err != nil {
		t.Fatalf("pro binary must install: %v", err)
	}
}

func TestUpdaterProChannelStaysCurrent(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	fx.mu.Lock()
	fx.proVersion = "146.0.7680.177.5"
	fx.mu.Unlock()

	cache := t.TempDir()
	t.Setenv(EnvLicenseKey, "KEY-1")
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/api/license/validate",
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if st := up.Status(); st.UpdatedTo != "" || st.Deferred {
		t.Errorf("status = %+v, want no-op on current version", st)
	}
	if fx.proDownloadHits != 0 {
		t.Errorf("current version must not re-download (hits=%d)", fx.proDownloadHits)
	}
}

func TestUpdaterProVerificationFailureDefersNoFreeDowngrade(t *testing.T) {
	// Bad-sig manifest on the pro channel: the updater records the
	// failure as deferred and NEVER installs the good free release
	// that sits ready on the same host.
	_, wrongSigner := manifestTestKey(t)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, wrongSigner)
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))

	cache := t.TempDir()
	t.Setenv(EnvLicenseKey, "KEY-1")
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.4")

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/api/license/validate",
		Logger:         testLogger(t),
	})
	err := up.CheckAndMaybeInstall(context.Background())
	if err == nil {
		t.Fatal("verification failure must surface")
	}
	st := up.Status()
	if !st.Deferred {
		t.Errorf("status = %+v, want deferred with the failure recorded", st)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "chromium-146.0.7680.177.5")); !os.IsNotExist(statErr) {
		t.Errorf("no free downgrade may install from a failed pro check")
	}
}

func TestUpdaterPinnedVersionDisablesSelfUpdate(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	cache := t.TempDir()
	t.Setenv(EnvLicenseKey, "KEY-1")
	t.Setenv(EnvCacheDir, cache)
	t.Setenv(EnvVersion, "146.0.7680.177.5")
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/api/license/validate",
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("a pinned version owns the channel: no update traffic allowed (v=%d d=%d)", fx.proVersionHits, fx.proDownloadHits)
	}
}

func TestUpdaterFreeLicenseOfflineDefersThenFreeChannel(t *testing.T) {
	// No license at all: the updater behaves exactly as before
	// (network-gated free flow) — the license machinery adds no
	// network dependency when no key exists.
	archive := freeArchive(t, "150.0.0.0.1")
	srv := newUpdateFixture(t,
		[]string{"chromium-v150.0.0.0.1"},
		map[string][]ghAsset{
			"chromium-v150.0.0.0.1": {{linuxX64Asset, int64(len(archive)), sha256Hex(archive), ""}},
		},
		map[string]string{linuxX64Asset: string(archive)},
	)
	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:      true,
		CacheDir:     cache,
		APIBase:      srv.srv.URL,
		DownloadBase: srv.srv.URL,
		ProbeURL:     srv.srv.URL + "/probe",
		Logger:       testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if st := up.Status(); st.UpdatedTo != "150.0.0.0.1" {
		t.Errorf("status = %+v, want the free update installed", st)
	}
}
