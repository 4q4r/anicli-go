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
	bad, ok := reloaded.verdictFor(major + 1)
	if !ok || bad.Verdict != "bad" {
		t.Fatalf("verdictFor(%d) = (%+v, %v), want bad", major+1, bad, ok)
	}
	if bad.Reason == "" || !strings.Contains(bad.Reason, "exit 76") {
		t.Errorf("bad verdict must carry the typed reason, got %q", bad.Reason)
	}
	if bad.ReProbeAfter.IsZero() || time.Until(bad.ReProbeAfter) <= badVerdictTTL-time.Minute {
		t.Errorf("bad verdict re-probe-after must be ~now+%v, got %v", badVerdictTTL, bad.ReProbeAfter)
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
	if !out.ok || calls != 1 {
		t.Fatalf("probe = %+v, calls = %d", out, calls)
	}
	s := loadVerdictStore(cache)
	major, _ := versionMajor(bin.Version)
	if e, ok := s.verdictFor(major); !ok || e.Verdict != "good" {
		t.Errorf("good verdict not persisted: (%+v, %v)", e, ok)
	}
	lkg := s.lastKnownGoodFor()
	if lkg == nil || lkg.Path != bin.Path || lkg.Version != bin.Version || lkg.Channel != channelFree || lkg.Major != major {
		t.Errorf("last-known-good = %+v, want the probed binary's record", lkg)
	}
	if lkg != nil && lkg.Chromedp == "" {
		t.Error("last-known-good must record the chromedp module version")
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
		writeVerdictStoreRaw(t, cache, "test-chromedp", map[string]any{
			"verdicts": map[string]any{
				"152": map[string]any{"verdict": "good", "checked_at": time.Now().Format(time.RFC3339Nano)},
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
		if *calls != 1 || attempts[0].Outcome != "probe-good" {
			t.Errorf("calls = %d, attempts = %+v, want exactly one probe-good", *calls, attempts)
		}
		if s := loadVerdictStore(cache); s.lastKnownGoodFor() == nil {
			t.Error("probe-good must record last-known-good")
		}
	})

	t.Run("probe failure falls back to last-known-good", func(t *testing.T) {
		cache, calls := newState(t)
		seed(t, cache, newest, oldest)
		overrideProbe(t, func(context.Context, string) probeOutcome {
			return probeOutcome{reason: "exit 76"}
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

		bin, attempts, err := evaluateCandidates(context.Background(), cache, channelFree, false, testLogger(t), cands(cache, newest, oldest))
		if err != nil || bin == nil || bin.Version != oldest {
			t.Fatalf("bin = %+v, err = %v, want LKG %s", bin, err, oldest)
		}
		if *calls != 2 {
			t.Errorf("probe calls = %d, want one per candidate (2)", *calls)
		}
		last := attempts[len(attempts)-1]
		if last.Outcome != "last-known-good" {
			t.Errorf("final attempt = %+v, want last-known-good", last)
		}
		// The failed probes were persisted as bad verdicts.
		if s := loadVerdictStore(cache); func() bool { _, ok := s.verdictFor(152); return !ok }() {
			t.Error("probe failure must persist the bad verdict")
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
		if *calls != 1 {
			t.Errorf("probe calls = %d, want the expired bad re-probed once", *calls)
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
