//go:build live

// PR76 live probe: the REAL launch probe against the REAL installed
// stealth-Chromium binary — the same chromedpDriver posture the solve
// path uses, navigating https://example.com and asserting the title
// renders, with the verdict store emptied first. On success the
// verdict good and the last-known-good record land in
// ~/.cloakbrowser/compat-verdicts.json (or $CLOAKBROWSER_CACHE_DIR).
//
// Excluded from the hermetic default suite by the `live` build tag:
//
//	go test -tags live -run TestLiveProbeInstalledBinary -count=1 -v ./internal/cfbrowser/
package cfbrowser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveProbeInstalledBinary(t *testing.T) {
	cacheDir := os.Getenv("CLOAKBROWSER_CACHE_DIR")
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		cacheDir = filepath.Join(home, ".cloakbrowser")
	}
	spec, err := CurrentPlatform()
	if err != nil {
		t.Fatal(err)
	}

	// The store starts EMPTY for this proof: the probe (not a stale
	// verdict) must decide.
	if err := os.Remove(filepath.Join(cacheDir, verdictStoreFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	cands := scanCacheOrdered(cacheDir, spec)
	if len(cands) == 0 {
		t.Skipf("no chromium dirs in %s — run anicli cf install first", cacheDir)
	}
	for _, c := range cands {
		t.Logf("candidate: %s (%s, channel %s)", c.Path, c.Version, c.Channel)
	}

	// The full resolution walk — exactly what the solve path runs:
	// each verdict-less candidate is probed for real, verdicts
	// persist, and the last-known-good rung stands by.
	bin, attempts, err := evaluateCandidates(context.Background(), cacheDir, ChannelAuto, false, testLogger(t), cands)
	for _, a := range attempts {
		t.Logf("attempt: %s → %s %s", a.Version, a.Outcome, a.Detail)
	}
	if err != nil {
		t.Fatalf("live resolution failed: %v", err)
	}
	if bin == nil {
		t.Fatal("no binary served")
	}
	t.Logf("served: %s (%s)", bin.Version, bin.Path)

	s := loadVerdictStore(cacheDir)
	for _, a := range attempts {
		if a.Outcome != outcomeProbeGood && a.Outcome != outcomeProbeBad && a.Outcome != outcomeFreshBad {
			continue
		}
		major, _ := versionMajor(a.Version)
		e, has := s.verdictFor(major)
		if !has {
			t.Errorf("attempt %s: verdict missing after the walk", a.Version)
			continue
		}
		if a.Outcome == outcomeProbeGood && e.Verdict != "good" {
			t.Errorf("attempt %s: want good, got %+v", a.Version, e)
		}
		if a.Outcome == outcomeProbeBad && e.Verdict != "bad" {
			t.Errorf("attempt %s: want bad, got %+v", a.Version, e)
		}
	}
	if lkg := s.lastKnownGoodFor(); lkg == nil || lkg.Path != bin.Path {
		t.Errorf("last-known-good = %+v, want the served binary", lkg)
	}
	t.Logf("store: %s", VerdictSummaryLine(cacheDir))
}
