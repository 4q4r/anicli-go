//go:build live

package audsync

// PR165b T6: the closed-loop SIGN proof (research §6.4). The pin that
// closes «пик ≈ 0 после применения»: measure a +3 s donor, apply the
// measured shift byte-exact to the WAV payload, re-encode, and
// measure again — the residual must vanish (|found2| ≤ 0.125, ITU).
// found1 / shifted / found2 land in the test log as the proof form.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveMeasuredShiftedRemeasured(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()
	refPath, donorPath := liveRefFixture(t, bin, dir, 320)

	a := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := a.Reference(ctx, srcURL(refPath))
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}

	// 1. The shifted donor measures ≈ +3.
	off1, err := a.Offset(ctx, ref, srcURL(donorPath))
	if err != nil {
		t.Fatalf("Offset(donor+3): %v", err)
	}
	t.Logf("found1 = %.3fs (want ≈ +3), confidence %.2f, agree %d",
		off1.Seconds, off1.Confidence, off1.WindowsAgree)
	if math.Abs(off1.Seconds-3) > 0.125 {
		t.Fatalf("found1 = %.3f, want ≈ +3 ± 0.125", off1.Seconds)
	}

	// 2. The pipeline legs on real files: decode → byte shift → m4a.
	wavPath := filepath.Join(dir, "donor.wav")
	if _, _, err := a.exec(ctx, a.ffmpeg, DecodeWAVArgs(srcURL(donorPath), wavPath)); err != nil {
		t.Fatalf("pipeline decode: %v", err)
	}
	if _, err := os.Stat(wavPath); err != nil {
		t.Fatalf("no decoded wav: %v", err)
	}
	shiftedPath := filepath.Join(dir, "donor.shifted.wav")
	frames := FramesFor(off1.Seconds, PipelineRate)
	if err := ShiftWAVFile(wavPath, shiftedPath, frames); err != nil {
		t.Fatalf("shift %d frames: %v", frames, err)
	}
	t.Logf("shifted = %d frames (%.3fs @48 kHz)", frames, off1.Seconds)
	m4aPath := filepath.Join(dir, "donor.m4a")
	if _, _, err := a.exec(ctx, a.ffmpeg, EncodeM4AArgs(shiftedPath, m4aPath)); err != nil {
		t.Fatalf("m4a encode: %v", err)
	}

	// 3. The shifted donor re-measures ≈ 0 (within the ITU 125 ms
	// window; AAC priming is ±21-23 ms and the snap eats it).
	off2, err := a.Offset(ctx, ref, srcURL(m4aPath))
	if err != nil {
		t.Fatalf("Offset(shifted m4a): %v", err)
	}
	t.Logf("found2 = %.3fs (want ≈ 0 ± 0.125)", off2.Seconds)
	if math.Abs(off2.Seconds) > 0.125 {
		t.Fatalf("found2 = %.3f, want |residual| ≤ 0.125", off2.Seconds)
	}
}
