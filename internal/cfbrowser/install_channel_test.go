package cfbrowser

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLogger returns a logger writing into buf (the channel tests
// must assert the loud free-fallback notes verbatim).
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestInstallAutoNoKeyResolvesFreeWithoutProTraffic(t *testing.T) {
	probeAlways(t)
	fx := newProInstallFixture(t)
	fx.licenseValid = true // the server WOULD validate — no key configured though
	// No EnvLicenseKey: auto must not spend a single pro-API request.
	cache := t.TempDir()
	cached := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	opts := fx.opts(t, cache)
	opts.Channel = ChannelAuto
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != cached || info.Channel != channelFree {
		t.Errorf("info = %+v, want the cached free binary %q", info, cached)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.licenseHits != 0 || fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("auto without a key must make no pro traffic: license=%d version=%d download=%d",
			fx.licenseHits, fx.proVersionHits, fx.proDownloadHits)
	}
}

func TestInstallAutoWithValidKeyPullsCompatiblePro(t *testing.T) {
	probeAlways(t)
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

	cache := t.TempDir()
	opts := fx.opts(t, cache)
	opts.Channel = ChannelAuto
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Channel != channelPro || info.Version != proVer {
		t.Errorf("info = %+v, want the pro upgrade to %s", info, proVer)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.proDownloadHits != 1 {
		t.Errorf("proDownloadHits = %d, want 1", fx.proDownloadHits)
	}
}

func TestInstallAutoWithValidKeyIncompatibleProStaysFreeLoud(t *testing.T) {
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	probeAlways(t) // the free-base walk probes 146 (no verdict yet)

	// The PR76 real-world shape: pro latest is 152 and the verdict
	// store holds a FRESH BAD verdict for it (a previous probe
	// failed). Auto must skip the pro pull without downloading,
	// loudly, and keep the free binary working.
	overrideChromedpVersion(t, "test-chromedp")
	const badPro = "152.0.0.0.1"
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = badPro
	fx.proArchives[badPro] = archive
	fx.mu.Unlock()
	fx.signWithProManifest(badPro, linuxX64Asset, archive, priv)

	cache := t.TempDir()
	cached := fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	seedFreshBadVerdict(t, cache, 152, "seccomp exit 76")

	var buf bytes.Buffer
	opts := fx.opts(t, cache)
	opts.Channel = ChannelAuto
	opts.Logger = captureLogger(&buf)
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != cached || info.Channel != channelFree {
		t.Errorf("info = %+v, want the working free binary %q", info, cached)
	}
	fx.mu.Lock()
	downloads := fx.proDownloadHits
	fx.mu.Unlock()
	if downloads != 0 {
		t.Errorf("proDownloadHits = %d, want 0 (a fresh-bad pro must not be pulled)", downloads)
	}
	note := buf.String()
	for _, want := range []string{badPro, "заблокирован", "работаем на free"} {
		if !strings.Contains(note, want) {
			t.Errorf("log note %q must mention %q", note, want)
		}
	}
}

func TestInstallFreeChannelNeverTouchesPro(t *testing.T) {
	probeAlways(t)
	fx := newProInstallFixture(t)
	fx.licenseValid = true // even a VALID key must not flip the free channel
	t.Setenv(EnvLicenseKey, "KEY-1")

	cache := t.TempDir()
	cached := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	opts := fx.opts(t, cache)
	opts.Channel = channelFree
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != cached || info.Channel != channelFree {
		t.Errorf("info = %+v, want the free binary %q", info, cached)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.licenseHits != 0 || fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("free channel must never touch pro: license=%d version=%d download=%d",
			fx.licenseHits, fx.proVersionHits, fx.proDownloadHits)
	}
}

func TestInstallFreeChannelPinnedBypassWarnsLoud(t *testing.T) {
	probeAlways(t)
	// The pinned rung is a documented exemption from the launch-
	// verdict mechanism (explicit user intent) — but it must not be
	// silent: a pinned install logs the loud bypass note.
	fx := newProInstallFixture(t)
	const pin = "152.0.0.0.1"
	archive := freeArchive(t, pin)
	fx.addFreeRelease(tagPrefix+pin, pin, archive)

	var buf bytes.Buffer
	cache := t.TempDir()
	opts := fx.opts(t, cache)
	opts.Channel = channelFree
	opts.Version = pin
	opts.Logger = captureLogger(&buf)
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != pin || info.Channel != channelFree {
		t.Errorf("info = %+v, want the free-tag pin %s", info, pin)
	}
	note := buf.String()
	for _, want := range []string{pin, "исключение"} {
		if !strings.Contains(note, want) {
			t.Errorf("bypass note %q must mention %q", note, want)
		}
	}
}

func TestResolveCurrentBinaryPinnedBypassWarnsLoud(t *testing.T) {
	cache := t.TempDir()
	want := fakeInstalledBinary(t, cache, "152.0.0.0.1")
	t.Setenv(EnvVersion, "152.0.0.0.1")

	var buf bytes.Buffer
	bin, err := ResolveCurrentBinary(ResolveOptions{
		CacheDir: cache,
		Logger:   captureLogger(&buf),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want {
		t.Errorf("path = %q, want the pinned dir %q (a pin serves what the user asked for)", bin.Path, want)
	}
	note := buf.String()
	for _, want := range []string{"152.0.0.0.1", "исключение"} {
		if !strings.Contains(note, want) {
			t.Errorf("bypass note %q must mention %q", note, want)
		}
	}
}

func TestInstallFreeChannelPinnedUsesFreeTagOnly(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")

	const pin = "147.0.0.0.2"
	archive := freeArchive(t, pin)
	fx.addFreeRelease(tagPrefix+pin, pin, archive)
	fx.mu.Lock()
	fx.proVersion = "150.0.0.0.1" // available on pro — must not be consulted
	fx.mu.Unlock()

	cache := t.TempDir()
	opts := fx.opts(t, cache)
	opts.Channel = channelFree
	opts.Version = pin
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != pin || info.Channel != channelFree {
		t.Errorf("info = %+v, want the free-tag pin %s", info, pin)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.proVersionHits != 0 || fx.proDownloadHits != 0 {
		t.Errorf("free pinned install must skip pro entirely: version=%d download=%d",
			fx.proVersionHits, fx.proDownloadHits)
	}
}

func TestInstallUnknownChannelFailsLoud(t *testing.T) {
	opts := InstallOptions{
		CacheDir: t.TempDir(),
		APIBase:  "http://127.0.0.1:1",
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
		Channel:  "banana",
	}
	_, err := Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "unknown channel") {
		t.Fatalf("err = %v, want a loud unknown-channel error", err)
	}
}

func TestInstallFreeLatestProbeBadIsLoudCompatError(t *testing.T) {
	// PR76: an unverified free latest is downloaded (bytes verified),
	// then must pass the launch probe. A probe-bad candidate is
	// recorded fresh-bad and rejected with the typed CompatError.
	// The installed bytes stay cached ON PURPOSE: the +7d verdict
	// expiry re-probe must cost a launch, not a re-download.
	fx := newProInstallFixture(t)
	const bad = "152.0.0.0.1"
	fx.addFreeRelease(tagPrefix+bad, bad, freeArchive(t, bad))
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome {
		return probeOutcome{reason: "exit 76 (seccomp)"}
	}, nil)

	cache := t.TempDir()
	opts := fx.opts(t, cache)
	opts.Channel = ChannelAuto
	_, err := Install(context.Background(), opts)
	if err == nil {
		t.Fatal("expected the probe to reject the free latest")
	}
	var compat *CompatError
	if !errors.As(err, &compat) {
		t.Fatalf("want *CompatError, got %T: %v", err, err)
	}
	if compat.Newest != bad {
		t.Errorf("compat = %+v, want Newest %s", compat, bad)
	}
	// The verdict persisted: the next install skips the re-download
	// and jumps straight to the loud refusal (or the LKG rung).
	if e, has := loadVerdictStore(cache).verdictFor(152); !has || e.Verdict != "bad" {
		t.Errorf("verdictFor(152) = (%+v, %v), want fresh bad", e, has)
	}
	// The rejected candidate's bytes remain for the cheap re-probe.
	if _, statErr := os.Stat(filepath.Join(cache, VersionDirName(bad))); statErr != nil {
		t.Errorf("probed-bad bytes must stay cached for the TTL re-probe: %v", statErr)
	}
}

func TestInstallAutoSkipsProMarkedCachedDir(t *testing.T) {
	probeAlways(t)
	// The user's real cache shape: a pro-marked 151 (bound-compatible,
	// but marker-skipped) plus a free 146. Auto with no key must run
	// on the free line — the pro dir never satisfies the free scan,
	// whatever its version.
	fx := newProInstallFixture(t)
	cache := t.TempDir()
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))
	markProBinary(t, proDir)
	want := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	opts := fx.opts(t, cache)
	opts.Channel = ChannelAuto
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != want || info.Version != "146.0.7680.177.5" {
		t.Errorf("info = %+v, want the free 146 dir %q", info, want)
	}
	if info.Channel != channelFree {
		t.Errorf("channel = %q, want free", info.Channel)
	}
}
