package cfbrowser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PR81 benchmarks for the CF verdict store (the startup gate that
// remembers chromedp binary probes): read path (every resolve) and
// write path (every probe verdict).

// benchPopulatedVerdictStore writes a store with n verdict entries and
// returns its cache dir.
func benchPopulatedVerdictStore(b *testing.B, n int) string {
	b.Helper()

	dir := b.TempDir()
	horizon := time.Now().Add(goodVerdictTTL)
	bucket := &verdictBucket{Verdicts: map[string]verdictEntry{}}
	for i := range n {
		major := 100 + i
		bucket.Verdicts[fmt.Sprint(major)] = verdictEntry{
			Verdict:      "good",
			CheckedAt:    time.Now().UTC().Add(-time.Duration(i) * time.Minute),
			ReProbeAfter: &horizon,
		}
	}
	all := map[string]*verdictBucket{"v1.0.0": bucket}
	raw, err := json.Marshal(all)
	if err != nil {
		b.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, verdictStoreFile), raw, 0o600); err != nil {
		b.Fatalf("write store: %v", err)
	}
	return dir
}

var (
	benchSinkStore *verdictStore
	benchSinkEntry verdictEntry
	benchSinkOK    bool
)

// pinBenchChromedpVersion pins the store bucket key for the benchmark.
func pinBenchChromedpVersion(b *testing.B) {
	b.Helper()
	prev := chromedpModuleVersion
	chromedpModuleVersion = func() string { return "v1.0.0" }
	b.Cleanup(func() { chromedpModuleVersion = prev })
}

// BenchmarkVerdictStoreLoad reads a 128-entry store from disk — the
// startup/resolve-time cost of consulting past probe verdicts.
func BenchmarkVerdictStoreLoad(b *testing.B) {
	b.ReportAllocs()
	dir := benchPopulatedVerdictStore(b, 128)
	pinBenchChromedpVersion(b)
	for b.Loop() {
		benchSinkStore = loadVerdictStore(dir)
	}
}

// BenchmarkVerdictStoreRecordSave records one good verdict and saves
// the 128-entry store back — the per-probe write cost.
func BenchmarkVerdictStoreRecordSave(b *testing.B) {
	b.ReportAllocs()
	dir := benchPopulatedVerdictStore(b, 128)
	pinBenchChromedpVersion(b)

	lkg := lastKnownGood{
		Version: "130.0.9999.100", Channel: "stable",
		Path: "/tmp/chrome", CheckedAt: time.Now().UTC(),
	}
	for b.Loop() {
		s := loadVerdictStore(dir)
		s.recordGood(130, lkg)
		s.save()
		entry, ok := s.verdictFor(130)
		benchSinkEntry = entry
		benchSinkOK = ok
	}
}
