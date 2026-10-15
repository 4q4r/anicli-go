//go:build live

package audsync

// PR173 v2 live proof: the owner's closed loop on REAL ffmpeg media.
// A real episode and a donor lagging by exactly +3 s (both mp4, aac):
// the audio-reference hierarchy builds the envelope reference, the
// measured lag lands ≈+3, the closed loop applies lag + priming (and
// corrects a deliberately wrong measurement), and the re-encoded m4a
// re-measures ≈0 — the owner's «подгонка в идеал» (0.00 ms residual).
// Requires ffmpeg on PATH.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveClosedLoopPerfectFit(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()
	refPath, donorPath := liveRefFixture(t, bin, dir, 320)

	a := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// 1. The reference hierarchy: no subtitles, the episode's own
	// audio (not jpn-tagged) wins as «любая другая исходная».
	ref, err := a.Reference(ctx, srcURL(refPath), "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefAudio {
		t.Fatalf("reference kind = %v, want RefAudio", ref.Kind)
	}

	// 2. The measured lag lands ≈ +3.
	off, err := a.Offset(ctx, ref, srcURL(donorPath))
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	t.Logf("found1 = %.3fs (want ≈ +3)", off.Seconds)
	if math.Abs(off.Seconds-3) > 0.125 {
		t.Fatalf("found1 = %.3f, want ≈ +3 ± 0.125", off.Seconds)
	}

	// 3. The pipeline legs: decode → closed loop → m4a.
	wavPath := filepath.Join(dir, "donor.wav")
	if _, _, err := a.exec(ctx, a.ffmpeg, DecodeWAVArgs(srcURL(donorPath), wavPath)); err != nil {
		t.Fatalf("pipeline decode: %v", err)
	}
	if _, err := os.Stat(wavPath); err != nil {
		t.Fatalf("no decoded wav: %v", err)
	}
	shiftedPath := filepath.Join(dir, "donor.shifted.wav")
	applied, err := a.ClosedLoop(ctx, ref, wavPath, shiftedPath, off.Seconds)
	if err != nil {
		t.Fatalf("ClosedLoop: %v", err)
	}
	t.Logf("applied = %.4fs (want ≈ 3 + priming = %.4f)", applied, 3.0+AACPrimingSec)
	if math.Abs(applied-(3.0+AACPrimingSec)) > 0.02 {
		t.Fatalf("applied = %.4f, want ≈ %.4f (the correct measurement never corrects)", applied, 3.0+AACPrimingSec)
	}
	m4aPath := filepath.Join(dir, "donor.m4a")
	if _, _, err := a.exec(ctx, a.ffmpeg, EncodeM4AArgs(shiftedPath, m4aPath)); err != nil {
		t.Fatalf("m4a encode: %v", err)
	}

	// 4. The muxed product re-measures ≈ 0 — the priming the shift
	// over-trimmed came back through the m4a encode (the owner's
	// «Итоговый лаг: +0.00»).
	off2, err := a.Offset(ctx, ref, srcURL(m4aPath))
	if err != nil {
		t.Fatalf("Offset(m4a): %v", err)
	}
	t.Logf("found2 = %.3fs (want ≈ 0)", off2.Seconds)
	if math.Abs(off2.Seconds) > 0.125 {
		t.Fatalf("found2 = %.3f, want |residual| ≤ 0.125", off2.Seconds)
	}
}

// TestLiveClosedLoopCorrectsWrongMeasurement: a deliberately wrong
// measurement (+50 ms) trips the tolerance and the ONE correction
// pass lands the true shift — the «подгонка в идеал» branch, live.
func TestLiveClosedLoopCorrectsWrongMeasurement(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()
	refPath, donorPath := liveRefFixture(t, bin, dir, 320)

	a := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	ref, err := a.Reference(ctx, srcURL(refPath), "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	wavPath := filepath.Join(dir, "donor.wav")
	if _, _, err := a.exec(ctx, a.ffmpeg, DecodeWAVArgs(srcURL(donorPath), wavPath)); err != nil {
		t.Fatalf("pipeline decode: %v", err)
	}
	shiftedPath := filepath.Join(dir, "donor.shifted.wav")
	applied, err := a.ClosedLoop(ctx, ref, wavPath, shiftedPath, 3.05)
	if err != nil {
		t.Fatalf("ClosedLoop: %v", err)
	}
	t.Logf("applied = %.4fs (want ≈ 3 + priming = %.4f)", applied, 3.0+AACPrimingSec)
	if math.Abs(applied-(3.0+AACPrimingSec)) > 0.045 {
		t.Fatalf("applied = %.4f, want ≈ %.4f ± one bin (the correction fired)", applied, 3.0+AACPrimingSec)
	}
	// The corrected product re-measures ≈ 0 through the m4a.
	m4aPath := filepath.Join(dir, "donor.m4a")
	if _, _, err := a.exec(ctx, a.ffmpeg, EncodeM4AArgs(shiftedPath, m4aPath)); err != nil {
		t.Fatalf("m4a encode: %v", err)
	}
	off2, err := a.Offset(ctx, ref, srcURL(m4aPath))
	if err != nil {
		t.Fatalf("Offset(m4a): %v", err)
	}
	t.Logf("found2 = %.3fs (want ≈ 0)", off2.Seconds)
	if math.Abs(off2.Seconds) > 0.125 {
		t.Fatalf("found2 = %.3f, want |residual| ≤ 0.125", off2.Seconds)
	}
}
