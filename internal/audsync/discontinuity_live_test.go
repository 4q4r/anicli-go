//go:build live

package audsync

// PR165b T6 live half: the discontinuous-HLS decode proofs against real
// ffmpeg-built fixtures.

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveDiscontinuousHLSDecode: the three §1.3 claims on a REAL
// discontinuous HLS (segment A = seed-42 voice +3 s @48 kHz; segment
// B = the SAME voice timeline continuing @44.1 kHz, different
// geometry — the timestamp discontinuity is real, the content is
// continuous, so every window can still vote).
func TestLiveDiscontinuousHLSDecode(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()

	// Segment A: 3 s silence + seed-42 content 0..157 @48 kHz (160 s).
	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=160,adelay=3000:all=1",
		"-c:a", "aac", "-b:a", "96k", "-ar", "48000", "-ac", "2", "-f", "mpegts",
		filepath.Join(dir, "segA.ts"))
	// Segment B: the SAME seed-42 timeline, continuing from 160 s,
	// RESAMPLED to 44.1 kHz (different geometry) — 160 s.
	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=320,atrim=start=160,asetpts=PTS-STARTPTS",
		"-c:a", "aac", "-b:a", "96k", "-ar", "44100", "-ac", "2", "-f", "mpegts",
		filepath.Join(dir, "segB.ts"))
	playlist, err := discontinuousPlaylist(dir, []string{"segA.ts", "segB.ts"}, []float64{160, 160})
	if err != nil {
		t.Fatal(err)
	}

	// The continuous reference episode (the same voice, no shift).
	refPath, _ := liveRefFixture(t, bin, dir, 320)

	a := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ref, err := a.Reference(ctx, srcURL(refPath))
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}

	// Claim 1: the PIPELINE decode of the discontinuous HLS is
	// THROUGH — the WAV spans both segments with no holes.
	wavPath := filepath.Join(dir, "full.wav")
	if _, _, err := a.exec(ctx, a.ffmpeg, decodeWAVArgs(srcURL(playlist), wavPath)); err != nil {
		t.Fatalf("pipeline decode: %v", err)
	}
	wf, err := os.Open(wavPath) //nolint:gosec // G304: the test reads its own temp wav
	if err != nil {
		t.Fatal(err)
	}
	layout, err := readWAVLayout(wf)
	_ = wf.Close()
	if err != nil {
		t.Fatalf("decoded wav: %v", err)
	}
	secs := float64(layout.Frames) / PipelineRate
	t.Logf("pipeline decode = %.2fs (segments sum 323s)", secs)
	if math.Abs(secs-323) > 1.5 {
		t.Fatalf("decoded %.2fs, want 323 ± 1.5 (a hole would shorten it)", secs)
	}

	// Claim 2: the analysis window at t=90 lands in segment A's
	// content — it correlates with the reference's own window at 90.
	// Both windows start at their stream's t=90, and the HLS stream
	// carries A's 3-second silence head, so the HLS window holds the
	// reference content 3 s EARLIER: the RAW delay reads +3 s (the
	// offset formula's −60 head-start term does not apply to two
	// equal windows — the prominence is the claim's substance).
	hlsWin := mustDecodeWindow(t, a, ctx, playlist, 90, 60)
	refWin := ref.Windows[0] // the reference's t=90 window
	delay, prom, err := CrossCorrelate(refWin, hlsWin)
	if err != nil {
		t.Fatal(err)
	}
	lagSec := float64(delay) / SampleRate
	t.Logf("window@90: prominence %.1f, raw delay %.3fs (A's head delay)", prom, lagSec)
	if prom < MinProminence {
		t.Fatalf("window@90 prominence %.1f < %.0f — the span is not the expected content", prom, MinProminence)
	}
	if math.Abs(lagSec-3) > 0.125 {
		t.Fatalf("window@90 raw delay %.3fs, want ≈ +3 (A's head delay)", lagSec)
	}

	// Claim 3: the FULL verdict over the discontinuous donor — the
	// boundary-crossing middle window (search 100..280 crosses the
	// t=160 discontinuity) either votes correctly or abstains; the
	// consensus stands and lands +3.
	off, err := a.Offset(ctx, ref, srcURL(playlist))
	if err != nil {
		t.Fatalf("Offset over discontinuous donor: %v", err)
	}
	t.Logf("verdict = %.3fs, agree %d, confidence %.2f", off.Seconds, off.WindowsAgree, off.Confidence)
	if math.Abs(off.Seconds-3) > 0.125 {
		t.Fatalf("verdict %.3f, want ≈ +3 ± 0.125", off.Seconds)
	}
	if off.WindowsAgree < 2 {
		t.Fatalf("WindowsAgree = %d, want ≥2 (the decode is PTS-blind)", off.WindowsAgree)
	}
}

// TestLiveDiscontinuousHLSContentBreakDegradesHonestly: when the
// content (not just the timestamps) breaks at the discontinuity, the
// windows over the foreign part must NOT vote — the verdict is either
// no-confidence or the true +3, never a made-up offset.
func TestLiveDiscontinuousHLSContentBreakDegradesHonestly(t *testing.T) {
	bin := liveBin(t)
	dir := t.TempDir()

	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=160,adelay=3000:all=1",
		"-c:a", "aac", "-b:a", "96k", "-ar", "48000", "-ac", "2", "-f", "mpegts",
		filepath.Join(dir, "segA.ts"))
	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=77:a=0.3:r=44100:d=160", // UNRELATED content
		"-c:a", "aac", "-b:a", "96k", "-ar", "44100", "-ac", "2", "-f", "mpegts",
		filepath.Join(dir, "segB.ts"))
	playlist, err := discontinuousPlaylist(dir, []string{"segA.ts", "segB.ts"}, []float64{163, 160})
	if err != nil {
		t.Fatal(err)
	}
	refPath, _ := liveRefFixture(t, bin, dir, 320)

	a := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ref, err := a.Reference(ctx, srcURL(refPath))
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	off, err := a.Offset(ctx, ref, srcURL(playlist))
	switch {
	case err == nil:
		t.Logf("verdict = %.3fs, agree %d (the A-span windows carried it)", off.Seconds, off.WindowsAgree)
		if math.Abs(off.Seconds-3) > 0.125 {
			t.Fatalf("verdict %.3f is a made-up offset, want ≈+3 or no-confidence", off.Seconds)
		}
	case errors.Is(err, ErrNoConfidence):
		t.Log("verdict = no confidence (the honest degradation)")
	default:
		t.Fatalf("unexpected error: %v", err)
	}
}

// mustDecodeWindow decodes one analysis window: the WHOLE source
// (decode order is the timeline), sliced locally at [start, start+dur).
func mustDecodeWindow(t *testing.T, a *Analyzer, ctx context.Context, url string, start, dur float64) []float64 {
	t.Helper()
	data, _, err := a.exec(ctx, a.ffmpeg, decodeArgs(srcURL(url)))
	if err != nil {
		t.Fatalf("decode window: %v", err)
	}
	win := sliceWindow(decodePCM(data), start, dur)
	if win == nil {
		t.Fatalf("window [%v, %v) past the decoded samples", start, dur)
	}
	return win
}
