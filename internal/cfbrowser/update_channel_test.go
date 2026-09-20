package cfbrowser

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdaterFreeChannelNeverTouchesPro(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true // even a VALID key must not flip the free channel
	t.Setenv(EnvLicenseKey, "KEY-1")

	// Free line current: the check completes without any pro traffic.
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/probe",
		Channel:        channelFree,
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.licenseHits != 0 || fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("free channel must never touch pro: license=%d version=%d download=%d",
			fx.licenseHits, fx.proVersionHits, fx.proDownloadHits)
	}
	if st := up.Status(); st.InstalledVersion != "146.0.7680.177.5" || st.LatestVersion != "146.0.7680.177.5" {
		t.Errorf("status = %+v, want the free line recorded as current", st)
	}
}

func TestUpdaterAutoNoKeyKeepsFreeFlowWithoutProTraffic(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true // the server WOULD validate — no key configured though
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6" // pro is reachable — must not be consulted
	fx.mu.Unlock()
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:      true,
		CacheDir:     cache,
		APIBase:      fx.srv.URL,
		DownloadBase: fx.srv.URL,
		ProbeURL:     fx.srv.URL + "/probe",
		Channel:      ChannelAuto,
		Logger:       testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.licenseHits != 0 || fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("auto without a key must make no pro traffic: license=%d version=%d download=%d",
			fx.licenseHits, fx.proVersionHits, fx.proDownloadHits)
	}
}

func TestUpdaterAutoWithKeyIncompatibleProFallsToFreeFlowLoud(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")

	// The post-lift real-world shape: pro latest is 152, above the
	// verified bound (151). The cycle must note it loudly and record
	// the FREE line in the status — never the pro 152.
	fx.mu.Lock()
	fx.proVersion = "152.0.0.0.1"
	fx.mu.Unlock()
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	var buf bytes.Buffer
	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/probe",
		Channel:        ChannelAuto,
		Logger:         captureLogger(&buf),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	fx.mu.Lock()
	downloads := fx.proDownloadHits
	fx.mu.Unlock()
	if downloads != 0 {
		t.Errorf("proDownloadHits = %d, want 0 (an incompatible pro must not be pulled)", downloads)
	}
	st := up.Status()
	if st.InstalledVersion != "146.0.7680.177.5" || st.LatestVersion != "146.0.7680.177.5" {
		t.Errorf("status = %+v, want the free line recorded (the working binary), not pro 152", st)
	}
	note := buf.String()
	for _, want := range []string{"152.0.0.0.1", "заблокирован", "работаем на free"} {
		if !strings.Contains(note, want) {
			t.Errorf("log note %q must mention %q", note, want)
		}
	}
}

func TestUpdaterAutoWithKeyCompatibleProUpdatesPro(t *testing.T) {
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")

	const proVer = "146.0.7680.177.9"
	archive := freeArchive(t, proVer)
	fx.mu.Lock()
	fx.proVersion = proVer
	fx.proArchives[proVer] = archive
	fx.mu.Unlock()
	fx.signWithProManifest(proVer, linuxX64Asset, archive, priv)
	// No free release on the listing: if the cycle wrongly ran the
	// free flow after a successful pro upgrade, the check errors.

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/probe",
		Channel:        ChannelAuto,
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if st := up.Status(); st.UpdatedTo != proVer {
		t.Errorf("status = %+v, want the compatible pro upgrade to %s", st, proVer)
	}
	if _, err := os.Stat(filepath.Join(cache, VersionDirName(proVer))); err != nil {
		t.Fatalf("pro binary must install: %v", err)
	}
}

func TestUpdaterFreeBaselineIgnoresProMarkedDir(t *testing.T) {
	// The PR73 bug class: a pro-marked 151 in the cache must not
	// suppress the free line's updates (the old update-status.json
	// tracked pro 151 while the free line fell behind).
	fx := newProInstallFixture(t)
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))
	markProBinary(t, proDir)
	fakeInstalledBinary(t, cache, "145.0.0.0.1")

	up := NewUpdater(UpdaterConfig{
		Enabled:      true,
		CacheDir:     cache,
		APIBase:      fx.srv.URL,
		DownloadBase: fx.srv.URL,
		ProbeURL:     fx.srv.URL + "/probe",
		Channel:      channelFree,
		Logger:       testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if st := up.Status(); st.UpdatedTo != "146.0.7680.177.5" {
		t.Errorf("status = %+v, want the free update installed (the pro-marked 151 must not gate it)", st)
	}
	dir := filepath.Join(cache, VersionDirName("146.0.7680.177.5"))
	marker, err := os.ReadFile(filepath.Join(dir, ".channel")) //nolint:gosec // test-owned temp path
	if err != nil || string(marker) != channelFree {
		t.Errorf(".channel = %q, %v — want a free-marked install", marker, err)
	}
}

func TestUpdaterAutoCompatibleProSavesOnlyIncompatibleFreeCache(t *testing.T) {
	// Install parity corner: when the free cache holds ONLY
	// incompatible dirs (e.g. a lone unmarked 152 above the verified
	// bound 151), auto must still attempt the compatible pro upgrade
	// BEFORE the compat bail-out — loud-but-stuck is wrong when a
	// working pro pull exists.
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")

	const proVer = "146.0.7680.177.9"
	archive := freeArchive(t, proVer)
	fx.mu.Lock()
	fx.proVersion = proVer
	fx.proArchives[proVer] = archive
	fx.mu.Unlock()
	fx.signWithProManifest(proVer, linuxX64Asset, archive, priv)
	// No free release on the listing: if the cycle wrongly proceeded
	// to the free flow after skipping pro, the check errors.

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "152.0.0.0.1") // free line, above the bound

	up := NewUpdater(UpdaterConfig{
		Enabled:        true,
		CacheDir:       cache,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		ProbeURL:       fx.srv.URL + "/probe",
		Channel:        ChannelAuto,
		Logger:         testLogger(t),
	})
	if err := up.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if st := up.Status(); st.UpdatedTo != proVer {
		t.Errorf("status = %+v, want the compatible pro upgrade to %s (the compat bail must not preempt it)", st, proVer)
	}
	if _, err := os.Stat(filepath.Join(cache, VersionDirName(proVer))); err != nil {
		t.Fatalf("pro binary must install: %v", err)
	}
}

func TestUpdaterFreeLatestIncompatibleRecordedLoud(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.addFreeRelease("chromium-v152.0.0.0.1", "152.0.0.0.1", freeArchive(t, "152.0.0.0.1"))

	cache := t.TempDir()
	t.Setenv(EnvCacheDir, cache)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	up := NewUpdater(UpdaterConfig{
		Enabled:      true,
		CacheDir:     cache,
		APIBase:      fx.srv.URL,
		DownloadBase: fx.srv.URL,
		ProbeURL:     fx.srv.URL + "/probe",
		Channel:      channelFree,
		Logger:       testLogger(t),
	})
	err := up.CheckAndMaybeInstall(context.Background())
	var compat *CompatError
	if !asCompat(err, &compat) {
		t.Fatalf("err = %v, want *CompatError", err)
	}
	if compat.Newest != "152.0.0.0.1" {
		t.Errorf("compat = %+v", compat)
	}
	st := up.Status()
	if st.UpdatedTo != "" || st.InstalledVersion != "146.0.7680.177.5" {
		t.Errorf("status = %+v, want the working free 146 untouched", st)
	}
	if !strings.Contains(st.LastError, "152.0.0.0.1") {
		t.Errorf("status.LastError = %q, want the rejected version named", st.LastError)
	}
	if _, statErr := os.Stat(filepath.Join(cache, VersionDirName("152.0.0.0.1"))); !os.IsNotExist(statErr) {
		t.Error("an incompatible free latest must not install")
	}
}

// asCompat is a thin errors.As wrapper for the CompatError assertions.
func asCompat(err error, target **CompatError) bool {
	return errors.As(err, target)
}
