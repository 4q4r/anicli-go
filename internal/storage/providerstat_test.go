package storage

import (
	"context"
	"testing"
	"time"
)

func TestProviderPriorityScore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		stat ProviderStat
	}{
		// Port of the python test_provider_stats_scoring scenario:
		// fast+successful must outrank slow+erroring.
		{"fast ok", ProviderStat{Successes: 20, Failures: 1, AvgLatencyMS: 300}},
		{"slow bad", ProviderStat{Successes: 10, Failures: 11, AvgLatencyMS: 1900}},
	}
	scores := make([]float64, len(cases))
	for i, tc := range cases {
		scores[i] = tc.stat.PriorityScore()
	}
	// Each case must score strictly below the one before it.
	for i := 1; i < len(scores); i++ {
		if scores[i] >= scores[i-1] {
			t.Errorf("%s score %v must be lower than %s score %v",
				cases[i].name, scores[i], cases[i-1].name, scores[i-1])
		}
	}

	// Zero attempts must not divide by zero and must be the neutral score.
	var fresh ProviderStat
	if s := fresh.PriorityScore(); s != 0 {
		t.Errorf("fresh provider score = %v, want 0", s)
	}

	// Monotonicity: more successes raises the score.
	low := ProviderStat{Successes: 10, Failures: 2, AvgLatencyMS: 100}
	high := ProviderStat{Successes: 20, Failures: 2, AvgLatencyMS: 100}
	if high.PriorityScore() <= low.PriorityScore() {
		t.Errorf("score with more successes = %v, want above %v",
			high.PriorityScore(), low.PriorityScore())
	}

	// Monotonicity: more failures lowers the score.
	withFailures := ProviderStat{Successes: 10, Failures: 5, AvgLatencyMS: 100}
	if withFailures.PriorityScore() >= low.PriorityScore() {
		t.Errorf("score with more failures = %v, want below %v",
			withFailures.PriorityScore(), low.PriorityScore())
	}

	// Monotonicity: higher latency lowers the score.
	slow := ProviderStat{Successes: 10, Failures: 2, AvgLatencyMS: 500}
	if slow.PriorityScore() >= low.PriorityScore() {
		t.Errorf("score with higher latency = %v, want below %v",
			slow.PriorityScore(), low.PriorityScore())
	}
}

func TestProviderStatRecordResult(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	if err := st.ProviderStats.RecordResult(ctx, "animego", true, 100); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := st.ProviderStats.RecordResult(ctx, "animego", true, 300); err != nil {
		t.Fatalf("record second: %v", err)
	}
	if err := st.ProviderStats.RecordResult(ctx, "animego", false, 200); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	stats, err := st.ProviderStats.TopProviders(ctx, 10)
	if err != nil {
		t.Fatalf("top providers: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("rows = %d, want 1", len(stats))
	}
	s := stats[0]
	if s.ProviderID != "animego" {
		t.Errorf("provider = %q, want animego", s.ProviderID)
	}
	if s.Successes != 2 || s.Failures != 1 {
		t.Errorf("counters = %d/%d, want 2/1", s.Successes, s.Failures)
	}
	// Running average over three attempts: (100+300+200)/3.
	if s.AvgLatencyMS != 200 {
		t.Errorf("avg latency = %v, want 200", s.AvgLatencyMS)
	}
	if s.LastUsedAt.IsZero() {
		t.Error("last_used_at not set")
	}
}

func TestProviderStatRecordResultFirstIsFailure(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	if err := st.ProviderStats.RecordResult(ctx, "kodik", false, 500); err != nil {
		t.Fatalf("record failure first: %v", err)
	}
	stats, err := st.ProviderStats.TopProviders(ctx, 10)
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("rows = %d, want 1", len(stats))
	}
	if stats[0].Successes != 0 || stats[0].Failures != 1 {
		t.Errorf("counters = %d/%d, want 0/1", stats[0].Successes, stats[0].Failures)
	}
}

func TestProviderStatTopProvidersOrdering(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	// fastgood outranks slowbad (score). tie-old and tie-new share
	// identical stats: recency (last_used_at) breaks the tie, newer first.
	recencyBase := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seed := []struct {
		id       string
		attempts int
		success  bool
		latency  float64
		lastUsed time.Time
	}{
		{"fastgood", 10, true, 100, recencyBase.Add(1 * time.Hour)},
		{"slowbad", 5, true, 2000, recencyBase.Add(2 * time.Hour)},
		{"tie-old", 5, true, 100, recencyBase},
		{"tie-new", 5, true, 100, recencyBase.Add(3 * time.Hour)},
	}
	for _, s := range seed {
		for range s.attempts {
			if err := st.ProviderStats.RecordResult(ctx, s.id, s.success, s.latency); err != nil {
				t.Fatalf("record %s: %v", s.id, err)
			}
		}
		// Pin last_used_at deterministically for the tiebreak assertion.
		if _, err := st.db.ExecContext(ctx,
			`UPDATE provider_search_stat SET last_used_at = ? WHERE provider_id = ?`,
			fmtTime(s.lastUsed), s.id); err != nil {
			t.Fatalf("pin last_used_at %s: %v", s.id, err)
		}
	}

	got, err := st.ProviderStats.TopProviders(ctx, 10)
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	wantOrder := []string{"fastgood", "tie-new", "tie-old", "slowbad"}
	if len(got) != len(wantOrder) {
		t.Fatalf("rows = %d, want %d", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].ProviderID != id {
			t.Fatalf("order = %v, want %v", got, wantOrder)
		}
	}

	// Limit caps the result.
	got, err = st.ProviderStats.TopProviders(ctx, 2)
	if err != nil {
		t.Fatalf("top limited: %v", err)
	}
	if len(got) != 2 || got[0].ProviderID != "fastgood" || got[1].ProviderID != "tie-new" {
		t.Errorf("limited top = %+v, want [fastgood tie-new]", got)
	}
}

func TestProviderStatTopProvidersEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	got, err := st.ProviderStats.TopProviders(context.Background(), 10)
	if err != nil {
		t.Fatalf("top on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %d, want 0", len(got))
	}
}
