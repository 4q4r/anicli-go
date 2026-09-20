package cfbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// overrideProbe swaps the probeBinary seam for the duration of one
// test (the cfbrowser suite runs sequentially — no t.Parallel).
func overrideProbe(t *testing.T, fn func(ctx context.Context, binaryPath string) probeOutcome, calls *int) {
	t.Helper()
	old := probeBinary
	t.Cleanup(func() { probeBinary = old })
	probeBinary = func(ctx context.Context, binaryPath string) probeOutcome {
		if calls != nil {
			*calls++
		}
		return fn(ctx, binaryPath)
	}
}

// probeAlways injects a probe fake answering ok for every candidate.
// The fixture binaries are fake ELF files — the real probe would
// launch them and record exec-format-error bad verdicts, so every
// install/update/resolution test that walks candidates injects this
// (or a variant of overrideProbe) explicitly.
func probeAlways(t *testing.T) {
	t.Helper()
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, nil)
}

// overrideChromedpVersion pins the store's chromedp bucket key.
func overrideChromedpVersion(t *testing.T, v string) {
	t.Helper()
	old := chromedpModuleVersion
	t.Cleanup(func() { chromedpModuleVersion = old })
	chromedpModuleVersion = func() string { return v }
}

// writeVerdictStoreRaw drops a hand-crafted compat-verdicts.json so
// TTL/bucket semantics are pinned against the on-disk contract.
func writeVerdictStoreRaw(t *testing.T, cacheDir, chromedp string, bucket any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{chromedp: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, verdictStoreFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerdictStorePersistsGoodAndBad(t *testing.T) {
	cache := t.TempDir()
	s := loadVerdictStore(cache)
	major, _ := versionMajor("151.0.7922.108.6")
	s.recordGood(major, lastKnownGood{Version: "151.0.7922.108.6", Channel: channelFree, Path: "/x/chrome", Major: major})
	s.recordBad(major+1, "probe: start chromium: exit 76")

	reloaded := loadVerdictStore(cache)
	got, ok := reloaded.verdictFor(major)
	if !ok || got.Verdict != "good" {
		t.Errorf("verdictFor(%d) = (%+v, %v), want good", major, got, ok)
	}
	if got.ReProbeAfter == nil || time.Until(*got.ReProbeAfter) <= goodVerdictTTL-time.Minute {
		t.Errorf("good verdict must carry a ~%v re-probe horizon, got %v", goodVerdictTTL, got.ReProbeAfter)
	}
	bad, ok := reloaded.verdictFor(major + 1)
	if !ok || bad.Verdict != "bad" {
		t.Fatalf("verdictFor(%d) = (%+v, %v), want bad", major+1, bad, ok)
	}
	if bad.Reason == "" || !strings.Contains(bad.Reason, "exit 76") {
		t.Errorf("bad verdict must carry the typed reason, got %q", bad.Reason)
	}
	if bad.ReProbeAfter == nil || time.Until(*bad.ReProbeAfter) <= badVerdictTTL-time.Minute {
		t.Errorf("bad verdict re-probe-after must be ~now+%v, got %v", badVerdictTTL, bad.ReProbeAfter)
	}
}

// TestGoodVerdictRequiresTwoConsecutivePasses is the review BLOCKER
// regression: one lucky probe must never persist a TTL-less good. A
// verdict good requires two consecutive passes (a fresh launch each
// time); an ok-then-fail pair records bad, and a first-pass failure
// never spends a second launch.
func TestGoodVerdictRequiresTwoConsecutivePasses(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	bin := &BinaryInfo{Path: "/x/chrome", Version: "151.0.0.0.1", Channel: channelFree}

	t.Run("ok then ok locks good", func(t *testing.T) {
		cache := t.TempDir()
		var calls int
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{ok: true}
		}, &calls)
		out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
		if !out.ok || calls != 2 {
			t.Fatalf("probe = %+v, calls = %d, want two passes and good", out, calls)
		}
		if e, has := loadVerdictStore(cache).verdictFor(151); !has || e.Verdict != "good" {
			t.Errorf("verdict = (%+v, %v), want good", e, has)
		}
	})

	t.Run("ok then fail records bad — no lucky good", func(t *testing.T) {
		cache := t.TempDir()
		// The flaky-major simulation: the probe alternates. The single
		// pass must never lock a good off one lucky launch.
		results := []probeOutcome{{ok: true}, {reason: "navigate: context canceled"}}
		overrideProbe(t, func(context.Context, string) probeOutcome {
			r := results[0]
			results = results[1:]
			return r
		}, nil)
		out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
		if out.ok || out.inconclusive {
			t.Fatalf("probe = %+v, want bad", out)
		}
		e, has := loadVerdictStore(cache).verdictFor(151)
		if !has || e.Verdict != "bad" {
			t.Fatalf("verdict = (%+v, %v), want bad after ok-then-fail", e, has)
		}
		if !strings.Contains(e.Reason, "context canceled") {
			t.Errorf("reason %q must carry the second pass failure", e.Reason)
		}
		if lkg := loadVerdictStore(cache).lastKnownGoodFor(); lkg != nil && lkg.Path == bin.Path {
			t.Error("an ok-then-fail major must never become last-known-good")
		}
	})

	t.Run("first-pass failure spends no second launch", func(t *testing.T) {
		cache := t.TempDir()
		var calls int
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{reason: "exit 76 (seccomp)"}
		}, &calls)
		out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
		if out.ok || calls != 1 {
			t.Fatalf("probe = %+v, calls = %d, want bad after exactly one launch", out, calls)
		}
	})
}

// TestGoodVerdictTTLExpires: good verdicts are not forever — after
// the TTL the next resolution re-probes (flaky majors must not stay
// locked; bad keeps its own longer 7d horizon).
func TestGoodVerdictTTLExpires(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	fresh := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	expired := time.Now().Add(-goodVerdictTTL - time.Hour).Format(time.RFC3339Nano)
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"151": map[string]any{"verdict": "good", "checked_at": fresh, "re_probe_after": fresh},
			"152": map[string]any{"verdict": "good", "checked_at": expired, "re_probe_after": expired},
		},
	})
	s := loadVerdictStore(cache)
	if e, ok := s.verdictFor(151); !ok || e.Verdict != "good" {
		t.Errorf("fresh good must bind, got (%+v, %v)", e, ok)
	}
	if e, ok := s.verdictFor(152); ok {
		t.Errorf("expired good must read as no-verdict, got (%+v, %v)", e, ok)
	}
	// Legacy format (no re_probe_after at all — written by the
	// pre-review build): treated as expired, self-heals on re-probe.
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"150": map[string]any{"verdict": "good", "checked_at": fresh},
		},
	})
	if _, ok := loadVerdictStore(cache).verdictFor(150); ok {
		t.Error("a legacy good without a re-probe horizon must not bind")
	}
}

// TestFlakyMajorDoesNotLockGood is the end-to-end flake regression:
// across repeated evaluations with alternating probe results, the
// walk must never end up serving (or storing) a good locked off a
// single lucky pass.
func TestFlakyMajorDoesNotLockGood(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	seed := func(t *testing.T, cache string, versions ...string) {
		t.Helper()
		for _, v := range versions {
			fakeInstalledBinary(t, cache, v)
		}
	}
	seed(t, cache, "152.0.0.0.1", "146.0.7680.177.5")
	cands := []*BinaryInfo{
		{Path: filepath.Join(cache, VersionDirName("152.0.0.0.1"), "chrome"),
			Dir: filepath.Join(cache, VersionDirName("152.0.0.0.1")), Version: "152.0.0.0.1", Channel: channelFree},
		{Path: filepath.Join(cache, VersionDirName("146.0.7680.177.5"), "chrome"),
			Dir: filepath.Join(cache, VersionDirName("146.0.7680.177.5")), Version: "146.0.7680.177.5", Channel: channelFree},
	}
	// The flaky major alternates: pass, fail, pass, fail, …
	pass := true
	overrideProbe(t, func(_ context.Context, p string) probeOutcome {
		if strings.Contains(p, "152") {
			pass = !pass
			if pass {
				return probeOutcome{ok: true}
			}
			return probeOutcome{reason: "context canceled"}
		}
		return probeOutcome{ok: true} // 146 is solid
	}, nil)

	for i := range 3 {
		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands)
		if err != nil || bin == nil {
			t.Fatalf("round %d: bin = %+v, err = %v", i, bin, err)
		}
		if bin.Version != "146.0.7680.177.5" {
			t.Fatalf("round %d: served %s, want the solid 146 (flaky 152 must not win)", i, bin.Version)
		}
		if e, has := loadVerdictStore(cache).verdictFor(152); !has || e.Verdict != "bad" {
			t.Fatalf("round %d: 152 verdict = (%+v, %v), want bad (never good off one pass)", i, e, has)
		}
	}
}

func TestVerdictBadTTLExpires(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	fresh := time.Now().Format(time.RFC3339Nano)
	horizon := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	expired := time.Now().Add(-badVerdictTTL - time.Hour).Format(time.RFC3339Nano)
	// 151 = fresh bad; 152 = bad whose TTL expired.
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"151": map[string]any{"verdict": "bad", "checked_at": fresh, "re_probe_after": horizon, "reason": "seccomp"},
			"152": map[string]any{"verdict": "bad", "checked_at": expired, "re_probe_after": expired, "reason": "seccomp"},
		},
	})
	s := loadVerdictStore(cache)
	if e, ok := s.verdictFor(151); !ok || e.Verdict != "bad" {
		t.Errorf("fresh bad must still bind, got (%+v, %v)", e, ok)
	}
	// The expired bad verdict is GONE for resolution purposes: a
	// re-probe must decide again (kernel swaps resurrect majors).
	if e, ok := s.verdictFor(152); ok {
		t.Errorf("expired bad must read as no-verdict, got (%+v, %v)", e, ok)
	}
}

func TestVerdictStoreKeyedByChromedpModuleVersion(t *testing.T) {
	cache := t.TempDir()
	overrideChromedpVersion(t, "v0.16.0")
	s := loadVerdictStore(cache)
	s.recordGood(151, lastKnownGood{Version: "151.0.0.0.1", Channel: channelFree, Path: "/x/chrome", Major: 151})

	// A chromedp module bump invalidates every verdict: the new
	// bucket starts empty — the module, not the upstream release,
	// decides what the driver can control.
	overrideChromedpVersion(t, "v0.17.0")
	s2 := loadVerdictStore(cache)
	if _, ok := s2.verdictFor(151); ok {
		t.Error("verdicts must not leak across chromedp module versions")
	}
	if s2.lastKnownGoodFor() != nil {
		t.Error("last-known-good must not leak across chromedp module versions")
	}
	// The old bucket is still on disk (history, not authority).
	s3 := loadVerdictStore(cache)
	overrideChromedpVersion(t, "v0.16.0")
	if _, ok := loadVerdictStore(cache).verdictFor(151); !ok {
		t.Error("old bucket must survive untouched")
	}
	_ = s3
}

func TestVerdictStoreAbsentOrCorruptIsEmpty(t *testing.T) {
	absent := loadVerdictStore(t.TempDir())
	if _, ok := absent.verdictFor(151); ok || absent.lastKnownGoodFor() != nil {
		t.Error("absent store must read empty")
	}

	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, verdictStoreFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := loadVerdictStore(cache)
	if _, ok := corrupt.verdictFor(151); ok {
		t.Error("corrupt store must read empty, never crash")
	}
}

func TestChromedpModuleVersionDetected(t *testing.T) {
	// The test binary carries build info; the chromedp module version
	// is the store's bucket key and must resolve to something.
	if v := chromedpModuleVersion(); v == "" {
		t.Error("chromedpModuleVersion() = \"\", want the build-info module version")
	}
}

func TestLastKnownGoodRungHonorsChannelFilter(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	proPath := fakeInstalledBinary(t, cache, "150.0.0.0.1")
	freePath := fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{},
		"last_known_good": map[string]any{
			"version": "150.0.0.0.1", "channel": channelPro, "path": proPath,
			"major": 150, "chromedp": "test-chromedp", "checked_at": time.Now().Format(time.RFC3339Nano),
		},
	})

	// auto honors any channel.
	if bin, ok := lastKnownGoodRung(cache, ChannelAuto, testLogger(t)); !ok || bin.Path != proPath {
		t.Errorf("auto LKG = (%+v, %v), want the pro record", bin, ok)
	}
	if bin, ok := lastKnownGoodRung(cache, channelPro, testLogger(t)); !ok || bin.Path != proPath {
		t.Errorf("pro LKG = (%+v, %v), want the pro record", bin, ok)
	}
	// The exemption: an off-filter LKG whose binary IS installed is
	// served anyway — the emergency rung forces the last working
	// browser over channel bookkeeping (loud, never silent).
	if bin, ok := lastKnownGoodRung(cache, channelFree, testLogger(t)); !ok || bin.Path != proPath {
		t.Errorf("free + installed off-channel LKG = (%+v, %v), want it served (exemption: installed)", bin, ok)
	}

	// Exemption: the off-channel LKG is served ONLY when it is
	// actually installed — the recorded path exists on disk.
	gone := filepath.Join(cache, "chromium-149.0.0.0.1", "chrome")
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{},
		"last_known_good": map[string]any{
			"version": "149.0.0.0.1", "channel": channelFree, "path": gone,
			"major": 149, "chromedp": "test-chromedp", "checked_at": time.Now().Format(time.RFC3339Nano),
		},
	})
	if _, ok := lastKnownGoodRung(cache, ChannelAuto, testLogger(t)); ok {
		t.Error("a pruned (nonexistent) LKG path must never be served")
	}

	// A free-line LKG satisfies the free filter.
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{},
		"last_known_good": map[string]any{
			"version": "146.0.7680.177.5", "channel": channelFree, "path": freePath,
			"major": 146, "chromedp": "test-chromedp", "checked_at": time.Now().Format(time.RFC3339Nano),
		},
	})
	if bin, ok := lastKnownGoodRung(cache, channelFree, testLogger(t)); !ok || bin.Path != freePath {
		t.Errorf("free LKG = (%+v, %v), want the free record", bin, ok)
	}
}

// TestLastKnownGoodRungRefusesFreshBadMajor is the review minor: an
// LKG record whose major carries a FRESH bad verdict is a guaranteed
// failed launch — it must fall through to the loud typed error, not
// serve.
func TestLastKnownGoodRungRefusesFreshBadMajor(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	lkgPath := fakeInstalledBinary(t, cache, "151.0.7922.108.6")
	now := time.Now().Format(time.RFC3339Nano)
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"151": map[string]any{"verdict": "bad", "checked_at": now,
				"re_probe_after": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "reason": "seccomp"},
		},
		"last_known_good": map[string]any{
			"version": "151.0.7922.108.6", "channel": channelFree, "path": lkgPath,
			"major": 151, "chromedp": "test-chromedp", "checked_at": now,
		},
	})
	if bin, ok := lastKnownGoodRung(cache, ChannelAuto, testLogger(t)); ok {
		t.Errorf("a fresh-bad LKG = (%+v, %v), want a miss (guaranteed failed launch)", bin, ok)
	}
	// An expired bad on the LKG major does not block the rung: the
	// expiry IS the decision that the old bad no longer binds.
	expired := time.Now().Add(-badVerdictTTL - time.Hour).Format(time.RFC3339Nano)
	writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
		"verdicts": map[string]any{
			"151": map[string]any{"verdict": "bad", "checked_at": expired,
				"re_probe_after": expired, "reason": "seccomp"},
		},
		"last_known_good": map[string]any{
			"version": "151.0.7922.108.6", "channel": channelFree, "path": lkgPath,
			"major": 151, "chromedp": "test-chromedp", "checked_at": now,
		},
	})
	if bin, ok := lastKnownGoodRung(cache, ChannelAuto, testLogger(t)); !ok || bin.Path != lkgPath {
		t.Errorf("expired-bad LKG = (%+v, %v), want it served", bin, ok)
	}
}

func TestProbeWithChromedpLaunchFailureIsBad(t *testing.T) {
	// A nonexistent binary cannot even start: that is a BAD verdict
	// (browser-side), not an inconclusive one.
	out := probeWithChromedp(context.Background(), filepath.Join(t.TempDir(), "no-such-chrome"))
	if out.ok || out.inconclusive {
		t.Errorf("launch failure = %+v, want bad (not inconclusive)", out)
	}
	if out.reason == "" {
		t.Error("bad verdict must carry a reason")
	}
}

func TestProbeGoodPersistsVerdictAndLastKnownGood(t *testing.T) {
	cache := t.TempDir()
	bin := &BinaryInfo{Path: "/x/chrome", Version: "151.0.7922.108.6", Channel: channelFree}
	var calls int
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, &calls)

	out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
	if !out.ok || calls != 2 {
		t.Fatalf("probe = %+v, calls = %d, want good after two consecutive passes", out, calls)
	}
	s := loadVerdictStore(cache)
	major, _ := versionMajor(bin.Version)
	e, has := s.verdictFor(major)
	if !has || e.Verdict != "good" {
		t.Fatalf("good verdict not persisted: (%+v, %v)", e, has)
	}
	if e.ReProbeAfter == nil || time.Until(*e.ReProbeAfter) <= goodVerdictTTL-time.Minute {
		t.Errorf("good verdict must carry the ~%v TTL, got %v", goodVerdictTTL, e.ReProbeAfter)
	}
	lkg := s.lastKnownGoodFor()
	if lkg == nil || lkg.Path != bin.Path || lkg.Version != bin.Version || lkg.Channel != channelFree || lkg.Major != major {
		t.Errorf("last-known-good = %+v, want the probed binary's record", lkg)
	}
	if lkg != nil && lkg.Chromedp == "" {
		t.Error("last-known-good must record the chromedp module version")
	}
}

// TestGoodVerdictJSONOmitsZeroReProbe: a good entry serializes without
// a "re_probe_after" key (the zero time must never leak into the
// store file).
func TestGoodVerdictJSONOmitsZeroReProbe(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()
	loadVerdictStore(cache).recordGood(151, lastKnownGood{Version: "151.0.0.0.1", Channel: channelFree, Path: "/x/chrome"})
	raw, err := os.ReadFile(filepath.Join(cache, verdictStoreFile)) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "0001-01-01") {
		t.Errorf("zero time leaked into the store:\n%s", raw)
	}
	if strings.Contains(string(raw), `"re_probe_after": ""`) {
		t.Errorf("empty re_probe_after leaked into the store:\n%s", raw)
	}
}

func TestProbeBadPersistsReasonAndTTL(t *testing.T) {
	cache := t.TempDir()
	bin := &BinaryInfo{Path: "/x/chrome", Version: "152.0.0.0.1", Channel: channelFree}
	overrideProbe(t, func(context.Context, string) probeOutcome {
		return probeOutcome{reason: "start chromium: exit 76 (seccomp)"}
	}, nil)

	out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
	if out.ok || out.inconclusive {
		t.Fatalf("probe = %+v, want bad", out)
	}
	s := loadVerdictStore(cache)
	major, _ := versionMajor(bin.Version)
	e, ok := s.verdictFor(major)
	if !ok || e.Verdict != "bad" || !strings.Contains(e.Reason, "exit 76") {
		t.Errorf("bad verdict = (%+v, %v), want persisted reason", e, ok)
	}
	if s.lastKnownGoodFor() != nil {
		t.Error("a failed probe must never touch last-known-good")
	}
}

func TestProbeInconclusivePersistsNothing(t *testing.T) {
	cache := t.TempDir()
	storePath := filepath.Join(cache, verdictStoreFile)
	bin := &BinaryInfo{Path: "/x/chrome", Version: "151.0.0.0.1", Channel: channelFree}
	overrideProbe(t, func(context.Context, string) probeOutcome {
		return probeOutcome{inconclusive: true, reason: "page load error ERR_INTERNET_DISCONNECTED"}
	}, nil)

	out := probeAndRecord(context.Background(), cache, bin, testLogger(t))
	if !out.inconclusive {
		t.Fatalf("probe = %+v, want inconclusive", out)
	}
	// Offline must not poison the store: no verdict, no LKG.
	if s := loadVerdictStore(cache); s.lastKnownGoodFor() != nil {
		t.Error("inconclusive probe must not write last-known-good")
	}
	if raw, err := os.ReadFile(storePath); err == nil && strings.Contains(string(raw), "\"good\"") { //nolint:gosec // test-owned temp path
		t.Errorf("inconclusive probe persisted a verdict: %s", raw)
	}

	// ok-then-inconclusive: the second pass could not navigate — no
	// verdict either (the first pass alone proves nothing).
	results := []probeOutcome{{ok: true}, {inconclusive: true, reason: "page load error ERR_TIMEOUT"}}
	overrideProbe(t, func(context.Context, string) probeOutcome {
		r := results[0]
		results = results[1:]
		return r
	}, nil)
	out2 := probeAndRecord(context.Background(), cache, bin, testLogger(t))
	if !out2.inconclusive {
		t.Fatalf("ok-then-inconclusive = %+v, want inconclusive", out2)
	}
	if s := loadVerdictStore(cache); s.lastKnownGoodFor() != nil {
		t.Error("ok-then-inconclusive must not write last-known-good")
	}
}

func TestVerdictSummaryLine(t *testing.T) {
	overrideChromedpVersion(t, "test-chromedp")
	cache := t.TempDir()

	// Empty store: no advisory line.
	if got := VerdictSummaryLine(cache); got != "" {
		t.Errorf("VerdictSummaryLine = %q, want empty for an empty store", got)
	}

	// Records present: the line names the last-known-good and counts.
	s := loadVerdictStore(cache)
	s.recordGood(151, lastKnownGood{Version: "151.0.7922.108.6", Channel: channelFree, Path: "/x/chrome"})
	s.recordBad(152, "seccomp")
	got := VerdictSummaryLine(cache)
	for _, want := range []string{"151.0.7922.108.6", channelFree, "good=1", "bad=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("VerdictSummaryLine %q must mention %q", got, want)
		}
	}
}

func TestEvaluateCandidatesMatrix(t *testing.T) {
	// The resolution order, one table: good verdicts short-circuit,
	// fresh bad skips, missing verdicts probe, exhausted candidates
	// fall to last-known-good, total failure is loud and typed.
	newest := "152.0.0.0.1"
	older := "151.0.7922.108.6"
	oldest := "146.0.7680.177.5"

	newState := func(t *testing.T) (string, *int) {
		overrideChromedpVersion(t, "test-chromedp")
		cache := t.TempDir()
		var calls int
		return cache, &calls
	}
	seed := func(t *testing.T, cache string, versions ...string) {
		t.Helper()
		for _, v := range versions {
			fakeInstalledBinary(t, cache, v)
		}
	}
	cands := func(cache string, versions ...string) []*BinaryInfo {
		out := make([]*BinaryInfo, 0, len(versions))
		for _, v := range versions {
			out = append(out, &BinaryInfo{
				Path: filepath.Join(cache, VersionDirName(v), "chrome"),
				Dir:  filepath.Join(cache, VersionDirName(v)), Version: v, Channel: channelFree,
			})
		}
		return out
	}

	t.Run("good verdict short-circuits without a probe", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest, older)
		horizon := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "good", "checked_at": horizon, "re_probe_after": horizon},
			},
		})
		overrideProbe(t, func(context.Context, string) probeOutcome {
			t.Error("a good verdict must never trigger a probe")
			return probeOutcome{}
		}, calls)

		bin, attempts, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest, older))
		if err != nil || bin == nil || bin.Version != newest {
			t.Fatalf("bin = %+v, err = %v, want %s", bin, err, newest)
		}
		if len(attempts) == 0 || attempts[0].Outcome != "good-verdict" {
			t.Errorf("attempts = %+v, want the good-verdict outcome", attempts)
		}
	})

	t.Run("fresh bad skips to the next candidate", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest, older)
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "bad", "checked_at": time.Now().Format(time.RFC3339Nano),
					"re_probe_after": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "reason": "seccomp"},
			},
		})
		overrideProbe(t, func(_ context.Context, p string) probeOutcome {
			if strings.Contains(p, "152") {
				t.Errorf("the fresh-bad 152 must skip without probing (path %q)", p)
			}
			return probeOutcome{ok: true} // 151 has no verdict: probe it
		}, calls)

		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest, older))
		if err != nil || bin == nil || bin.Version != older {
			t.Fatalf("bin = %+v, err = %v, want %s", bin, err, older)
		}
	})

	t.Run("missing verdict probes and persists good", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest)
		overrideProbe(t, func(_ context.Context, p string) probeOutcome {
			if p != filepath.Join(cache, VersionDirName(newest), "chrome") {
				t.Errorf("probe path = %q, want the newest candidate", p)
			}
			return probeOutcome{ok: true}
		}, calls)

		bin, attempts, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if err != nil || bin == nil || bin.Version != newest {
			t.Fatalf("bin = %+v, err = %v", bin, err)
		}
		if *calls != 2 || attempts[0].Outcome != "probe-good" {
			t.Errorf("calls = %d, attempts = %+v, want exactly two probe calls (consecutive passes) and probe-good", *calls, attempts)
		}
		if s := loadVerdictStore(cache); s.lastKnownGoodFor() == nil {
			t.Error("probe-good must record last-known-good")
		}
	})

	t.Run("probe failure falls back to last-known-good", func(t *testing.T) {
		cache, calls := newState(t)
		// One free candidate (152): probed bad. The LKG points to a
		// DIFFERENT major (146) with no verdict — the review minor
		// refuses an LKG whose own major holds a fresh bad.
		seed(t, cache, newest, oldest)
		overrideProbe(t, func(_ context.Context, p string) probeOutcome {
			if strings.Contains(p, "152") {
				return probeOutcome{reason: "exit 76"}
			}
			t.Errorf("only the newest candidate should be probed, got %q", p)
			return probeOutcome{ok: true}
		}, calls)

		// Pre-seed the LKG the store would carry from a past good probe.
		lkgPath := filepath.Join(cache, VersionDirName(oldest), "chrome")
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{},
			"last_known_good": map[string]any{
				"version": oldest, "channel": channelFree, "path": lkgPath,
				"major": 146, "chromedp": "test-chromedp", "checked_at": time.Now().Format(time.RFC3339Nano),
			},
		})

		// The walk only ever sees 152 (the caller's candidate list);
		// 146's dir exists on disk for the LKG path.
		bin, attempts, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if err != nil || bin == nil || bin.Version != oldest {
			t.Fatalf("bin = %+v, err = %v, want LKG %s", bin, err, oldest)
		}
		if *calls != 1 {
			t.Errorf("probe calls = %d, want one (the newest candidate only)", *calls)
		}
		last := attempts[len(attempts)-1]
		if last.Outcome != "last-known-good" {
			t.Errorf("final attempt = %+v, want last-known-good", last)
		}
		// The failed probe was persisted as a bad verdict.
		if s := loadVerdictStore(cache); func() bool { _, ok := s.verdictFor(152); return !ok }() {
			t.Error("probe failure must persist the bad verdict")
		}
	})

	t.Run("LKG whose own major is fresh-bad falls through to the loud error", func(t *testing.T) {
		// The review minor: serving an LKG whose major carries a fresh
		// bad verdict is a guaranteed failed launch — the rung must
		// refuse it and the resolution must fail loud and typed.
		cache, calls := newState(t)
		seed(t, cache, newest)
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{reason: "exit 76"}
		}, calls)
		lkgPath := filepath.Join(cache, VersionDirName(newest), "chrome")
		now := time.Now().Format(time.RFC3339Nano)
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"last_known_good": map[string]any{
				"version": newest, "channel": channelFree, "path": lkgPath,
				"major": 152, "chromedp": "test-chromedp", "checked_at": now,
			},
		})

		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if bin != nil || err == nil {
			t.Fatalf("bin = %+v, err = %v, want the loud refusal", bin, err)
		}
		var compat *CompatError
		if !errors.As(err, &compat) {
			t.Fatalf("want *CompatError, got %T: %v", err, err)
		}
	})

	t.Run("offline probe serves unverified without persisting", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest)
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{inconclusive: true, reason: "page load error ERR_INTERNET_DISCONNECTED"}
		}, calls)

		bin, attempts, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if err != nil || bin == nil || bin.Version != newest {
			t.Fatalf("bin = %+v, err = %v, want the unverified newest", bin, err)
		}
		if attempts[0].Outcome != "unverified-offline" {
			t.Errorf("outcome = %q, want unverified-offline", attempts[0].Outcome)
		}
		if s := loadVerdictStore(cache); s.lastKnownGoodFor() != nil {
			t.Error("an inconclusive probe must not persist anything")
		}
	})

	t.Run("expired bad re-probes", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest)
		expired := time.Now().Add(-badVerdictTTL - time.Hour).Format(time.RFC3339Nano)
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "bad", "checked_at": expired,
					"re_probe_after": expired, "reason": "old kernel"},
			},
		})
		overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, calls)

		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if err != nil || bin == nil || bin.Version != newest {
			t.Fatalf("bin = %+v, err = %v, want %s re-verified", bin, err, newest)
		}
		if *calls != 2 {
			t.Errorf("probe calls = %d, want the expired bad re-probed with two consecutive passes", *calls)
		}
	})

	t.Run("expired good re-probes", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest)
		expired := time.Now().Add(-goodVerdictTTL - time.Hour).Format(time.RFC3339Nano)
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "good", "checked_at": expired, "re_probe_after": expired},
			},
		})
		var seq []probeOutcome
		overrideProbe(t, func(context.Context, string) probeOutcome {
			r := seq[0]
			seq = seq[1:]
			return r
		}, calls)

		// A flaky flip after expiry: pass then fail — no good is
		// re-locked; the walk falls through.
		seq = []probeOutcome{{ok: true}, {reason: "context canceled"}}
		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if bin != nil || err == nil {
			t.Fatalf("bin = %+v, err = %v, want the loud total failure (no candidates left)", bin, err)
		}
		if e, has := loadVerdictStore(cache).verdictFor(152); !has || e.Verdict != "bad" {
			t.Errorf("152 verdict = (%+v, %v), want bad after pass-fail", e, has)
		}

		// A genuinely healed major re-locks good with two passes.
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "good", "checked_at": expired, "re_probe_after": expired},
			},
		})
		seq = []probeOutcome{{ok: true}, {ok: true}}
		bin, _, err = evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest))
		if err != nil || bin == nil || bin.Version != newest {
			t.Fatalf("bin = %+v, err = %v, want %s re-verified good", bin, err, newest)
		}
		h := loadVerdictStore(cache)
		e, has := h.verdictFor(152)
		if !has || e.Verdict != "good" {
			t.Errorf("152 verdict = (%+v, %v), want fresh good", e, has)
		}
	})

	t.Run("total failure is a typed loud error naming every attempt", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest, older)
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{reason: "exit 76 (seccomp)"}
		}, calls)

		bin, _, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest, older))
		if bin != nil || err == nil {
			t.Fatalf("bin = %+v, err = %v, want a loud error", bin, err)
		}
		var compat *CompatError
		if !errors.As(err, &compat) {
			t.Fatalf("want *CompatError, got %T: %v", err, err)
		}
		msg := compat.Error()
		for _, want := range []string{newest, older, "exit 76 (seccomp)", EnvBinaryPath, EnvVersion} {
			if !strings.Contains(msg, want) {
				t.Errorf("compat error %q must mention %q", msg, want)
			}
		}
		// PR75 honesty: no promised chromedp fix.
		if strings.Contains(msg, "обновите chromedp") {
			t.Errorf("compat error %q must not promise a chromedp fix", msg)
		}
	})

	t.Run("unparsable version fails closed", func(t *testing.T) {
		cache, _ := newState(t)
		seed(t, cache, "146.0.0.0.1")
		bad := []*BinaryInfo{{Path: "/x/chrome", Version: "banana.1", Channel: channelFree}}
		overrideProbe(t, func(context.Context, string) probeOutcome {
			t.Error("an unparsable version must never be probed")
			return probeOutcome{}
		}, nil)
		bin2, attempts, err2 := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), bad)
		if bin2 != nil || err2 == nil {
			t.Fatalf("unparsable candidate = (%+v, %v), want a loud miss", bin2, err2)
		}
		if len(attempts) != 1 || attempts[0].Outcome != outcomeUnparsable {
			t.Errorf("attempts = %+v, want the unparsable-skip record", attempts)
		}
	})
}

func TestEvaluateCandidatesGroupOrderProBeforeFree(t *testing.T) {
	// auto posture: the pro group is walked before the free group; the
	// first usable candidate of the earliest group wins.
	cache := t.TempDir()
	proDir := filepath.Join(cache, VersionDirName("150.0.0.0.1"))
	fakeInstalledBinary(t, cache, "150.0.0.0.1")
	markProBinary(t, proDir)
	fakeInstalledBinary(t, cache, "151.0.0.0.1") // free, newer

	pro := []*BinaryInfo{{Path: filepath.Join(proDir, "chrome"), Dir: proDir, Version: "150.0.0.0.1", Channel: channelPro}}
	free := []*BinaryInfo{{Path: filepath.Join(cache, VersionDirName("151.0.0.0.1"), "chrome"),
		Dir: filepath.Join(cache, VersionDirName("151.0.0.0.1")), Version: "151.0.0.0.1", Channel: channelFree}}
	overrideProbe(t, func(context.Context, string) probeOutcome { return probeOutcome{ok: true} }, nil)

	bin, _, err := evaluateCandidates(context.Background(), cache, ChannelAuto, false, testLogger(t), pro, free)
	if err != nil || bin == nil || bin.Channel != channelPro {
		t.Errorf("bin = %+v, err = %v, want the pro group served first", bin, err)
	}
}
