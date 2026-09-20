package cfbrowser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedFreshBadVerdict records a fresh (non-expired) bad verdict for
// a major in the test store. The bucket key must be pinned via
// overrideChromedpVersion by the caller.
func seedFreshBadVerdict(t *testing.T, cacheDir string, major int, reason string) {
	t.Helper()
	now := time.Now().Format(time.RFC3339Nano)
	writeVerdictStoreRaw(t, cacheDir, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			itoa(major): map[string]any{
				"verdict": "bad", "checked_at": now,
				"re_probe_after": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
				"reason":         reason,
			},
		},
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

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
	// verdict-usable pro directory outranks the free scan even when a
	// free dir is newer. Neither dir has a verdict yet — both would
	// probe in principle, but the pro group is walked first and wins.
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(_ context.Context, _ string) probeOutcome { return probeOutcome{ok: true} }, nil)

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

// TestResolveCurrentBinaryAutoSkipsFreshBadProDir is the PR76 shape
// of the PR73 test: a valid key plus a pro-marked 152 carrying a
// FRESH BAD verdict (the store's memory of a failed probe), next to
// a working free 146. Auto must launch the free line — never the
// freshly rejected pro build.
func TestResolveCurrentBinaryAutoSkipsFreshBadProDir(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(_ context.Context, p string) probeOutcome {
		if strings.Contains(p, "152") {
			t.Errorf("the fresh-bad pro 152 must not even be probed (path %q)", p)
		}
		return probeOutcome{ok: true}
	}, nil)

	cache := t.TempDir()
	seedValidLicenseCache(t, cache)
	proDir := filepath.Join(cache, VersionDirName("152.0.0.0.1"))
	fakeInstalledBinary(t, cache, "152.0.0.0.1")
	markProBinary(t, proDir)
	want := fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	seedFreshBadVerdict(t, cache, 152, "seccomp exit 76")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: ChannelAuto})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want || bin.Channel != channelFree {
		t.Errorf("bin = %+v, want the usable free line %q", bin, want)
	}
}

func TestResolveCurrentBinaryFreeChannelNeverPrefersPro(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, nil)

	// channel=free ignores the pro preference even under a valid
	// cached license — a pro dir stays untouched.
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

func TestResolveCurrentBinaryAllFreshBadIsLoudCompatError(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome {
		t.Error("a fresh-bad verdict must skip without probing")
		return probeOutcome{}
	}, nil)

	// The lone free dir carries a fresh bad verdict and no
	// last-known-good exists: the resolution fails loud and typed.
	cache := t.TempDir()
	fakeInstalledBinary(t, cache, "152.0.0.0.1")
	seedFreshBadVerdict(t, cache, 152, "seccomp exit 76")

	_, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: ChannelAuto})
	var compat *CompatError
	if !errors.As(err, &compat) {
		t.Fatalf("want *CompatError, got %T: %v", err, err)
	}
	if compat.Newest != "152.0.0.0.1" {
		t.Errorf("compat = %+v", compat)
	}
	for _, want := range []string{"152.0.0.0.1", EnvBinaryPath} {
		if !strings.Contains(compat.Error(), want) {
			t.Errorf("compat error %q must mention %q", compat.Error(), want)
		}
	}
}

func TestResolveCurrentBinaryFallsBackToLastKnownGood(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome {
		t.Error("an all-fresh-bad cache must fall to the LKG rung without probing")
		return probeOutcome{}
	}, nil)

	// Every candidate freshly rejected; the store remembers 146 as
	// the last binary a probe verified working. The LKG rung serves
	// it — the owner's mechanism: when the new one fails, force the
	// last one that worked.
	cache := t.TempDir()
	p152 := fakeInstalledBinary(t, cache, "152.0.0.0.1")
	lkgPath := fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	now := time.Now().Format(time.RFC3339Nano)
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"152": map[string]any{"verdict": "bad", "checked_at": now,
				"re_probe_after": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "reason": "seccomp"},
			"146": map[string]any{"verdict": "bad", "checked_at": now,
				"re_probe_after": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "reason": "also broken"},
		},
		"last_known_good": map[string]any{
			"version": "146.0.7680.177.5", "channel": channelFree, "path": lkgPath,
			"major": 146, "chromedp": "test-chromedp", "checked_at": now,
		},
	})

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, Channel: ChannelAuto})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != lkgPath || bin.Version != "146.0.7680.177.5" {
		t.Errorf("bin = %+v (p152=%s), want the last-known-good %q", bin, p152, lkgPath)
	}
}

func TestResolveCurrentBinaryMissingVerdictProbesAndPersists(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	var calls int
	overrideProbe(t, func(_ context.Context, p string) probeOutcome {
		if !strings.HasSuffix(p, "chrome") {
			t.Errorf("probe path %q lacks the executable", p)
		}
		return probeOutcome{ok: true}
	}, &calls)

	// First-ever resolution of a verdict-less cache: the probe
	// decides (good) and the verdict persists — the next resolution
	// short-circuits without launching anything.
	cache := t.TempDir()
	want := fakeInstalledBinary(t, cache, "151.0.7922.108.6")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want || calls != 1 {
		t.Fatalf("bin = %+v, probe calls = %d, want the candidate probed once", bin, calls)
	}
	s := loadVerdictStore(cache)
	if e, ok := s.verdictFor(151); !ok || e.Verdict != "good" {
		t.Fatalf("verdict not persisted: (%+v, %v)", e, ok)
	}

	// The second resolution rides the persisted verdict.
	overrideProbe(t, func(context.Context, string) probeOutcome {
		t.Error("a persisted good verdict must not re-probe")
		return probeOutcome{}
	}, nil)
	if bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache}); err != nil || bin.Path != want {
		t.Errorf("second resolve = (%+v, %v), want the same binary via the verdict", bin, err)
	}
}

func TestResolveCurrentBinaryNoProbeServesWithoutVerdict(t *testing.T) {
	// The advisory mode (cf status): resolution must never launch a
	// browser. A verdict-less candidate is served tentatively and
	// nothing is persisted; a fresh-bad verdict still skips.
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome {
		t.Error("NoProbe resolution must not launch the probe")
		return probeOutcome{}
	}, nil)

	cache := t.TempDir()
	want := fakeInstalledBinary(t, cache, "151.0.7922.108.6")

	bin, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, NoProbe: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if bin.Path != want {
		t.Errorf("bin = %+v, want the newest candidate served tentatively", bin)
	}
	if s := loadVerdictStore(cache); s.lastKnownGoodFor() != nil {
		t.Error("NoProbe resolution must not persist verdicts")
	}

	// Fresh-bad still binds in advisory mode.
	seedFreshBadVerdict(t, cache, 151, "seccomp")
	if _, err := ResolveCurrentBinary(ResolveOptions{CacheDir: cache, NoProbe: true}); !errors.As(err, new(*CompatError)) {
		t.Errorf("resolve = %v, want the fresh-bad loud refusal", err)
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
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, nil)

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
	// see TestResolveCurrentBinaryAutoSkipsFreshBadProDir.)
	overrideChromedpVersion(t, "test-chromedp")
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, nil)

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
