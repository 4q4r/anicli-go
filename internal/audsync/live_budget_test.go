//go:build live

package audsync

// PR170b live proof: a real short file decodes well under the default
// DecodeBudget and the offset is still found — the budget separation
// (decode 120 s / per-pair analysis 15 s) never disturbs the success
// path. Requires ffmpeg on PATH.

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestLiveDecodeUnderBudgetRealShortFile(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()
	// The shortest fixture the schedule accepts (MinEpisodeDur 300 s):
	// a real mp4 episode and a donor lagging by exactly 3 s.
	refPath, donorPath := liveRefFixture(t, bin, dir, 310)

	a := New(Options{}) // default budgets: decode 120 s, analysis 15 s
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	ref, err := a.Reference(ctx, srcURL(refPath))
	decodeWall := time.Since(start)
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	t.Logf("reference wall = %v (DecodeBudget = %v)", decodeWall, DefaultDecodeBudget)
	if decodeWall > DefaultDecodeBudget {
		t.Fatalf("reference wall %v exceeded the decode budget %v", decodeWall, DefaultDecodeBudget)
	}

	off, err := a.Offset(ctx, ref, srcURL(donorPath))
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	t.Logf("found = %.3fs (want ≈ +3)", off.Seconds)
	if math.Abs(off.Seconds-3) > 0.125 {
		t.Fatalf("found = %.3f, want ≈ +3 ± 0.125", off.Seconds)
	}
}
