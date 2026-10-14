package audsync

// PR165b T3 tests: window schedule, the analyzePair verdict (RMS gate
// → cross-correlation → consensus → median → frame snap) and the RMS
// helpers. All float windows carry s16-scale values (the decode
// conversion is the s16 integer value as float64), so the MinRMS
// silence gate compares directly.

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

const testRate = SampleRate // samples per second of the analysis windows

// wobble synthesizes s16-scale "dialogue-like" audio: envelope-gated
// broadband noise (phrase bursts with gaps). Broadband is load-bearing:
// its autocorrelation is a sharp spike, which the peak/mean prominence
// gate requires; s16 scale keeps the RMS gate honest.
func wobble(seconds float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic synthetic signal, not security
	n := int(seconds * testRate)
	x := make([]float64, n)
	inPhrase := false
	for i := range n {
		// Phrase rhythm: 4 s speech, 2 s gap, phase-flipped by i.
		if i%(6*testRate) == 0 {
			inPhrase = true
		}
		if i%(6*testRate) == 4*testRate {
			inPhrase = false
		}
		if !inPhrase {
			continue
		}
		x[i] = rng.NormFloat64() * 6000 // RMS ≈ 6000 ≫ MinRMS
	}
	return x
}

// shiftedCopy renders a donor timeline: the reference shifted by k
// seconds (k > 0 — the donor lags; its head is silence), the owner's
// sign semantics.
func shiftedCopy(k float64, src []float64) []float64 {
	out := make([]float64, len(src))
	shift := int(math.Round(k * testRate))
	if shift >= 0 {
		copy(out[shift:], src[:len(src)-shift])
	} else {
		copy(out, src[-shift:])
	}
	return out
}

// cutWindows applies WindowSchedule to full timelines: ref [t, t+60],
// search [max(0,t−60), +180].
func cutWindows(video, dub []float64, dur float64) ([][]float64, [][]float64) {
	ws := WindowSchedule(dur)
	refs := make([][]float64, 0, len(ws))
	srchs := make([][]float64, 0, len(ws))
	for _, w := range ws {
		r0 := int(w.RefStart * testRate)
		s0 := int(w.SearchStart * testRate)
		refs = append(refs, video[r0:r0+int(ReferenceDur*testRate)])
		srchs = append(srchs, dub[s0:s0+int(SearchDur*testRate)])
	}
	return refs, srchs
}

// mkWindow places ref into a 180-s silent search frame so the
// correlation lag decodes to the wanted offset:
// delay = (ReferenceDur + off)·SampleRate, offset = delay−len(ref).
func mkWindow(ref []float64, off float64) []float64 {
	srch := make([]float64, int(SearchDur*testRate))
	start := int(math.Round((ReferenceDur + off) * testRate))
	copy(srch[start:], ref)
	return srch
}

func TestAnalyzePairFindsShiftedDubExactly(t *testing.T) {
	for _, k := range []float64{17.042, -0.958, 6.25, 0.5} {
		video := wobble(24*60, 3)
		dub := shiftedCopy(k, video)
		refWins, searchWins := cutWindows(video, dub, 1440.0)
		off, err := analyzePair(refWins, searchWins, 24)
		if err != nil {
			t.Fatalf("k=%v: %v", k, err)
		}
		if math.Abs(off.Seconds-k) > 0.05 {
			t.Fatalf("k=%v: got %v", k, off.Seconds)
		}
		if off.WindowsAgree < 2 || off.Confidence < 1 {
			t.Fatalf("k=%v: %+v", k, off)
		}
	}
}

func TestAnalyzePairNoConfidenceOnSilence(t *testing.T) {
	refWins := [][]float64{zeros(60 * testRate), zeros(60 * testRate), zeros(60 * testRate)}
	searchWins := [][]float64{noise(180*testRate, 11), noise(180*testRate, 12), noise(180*testRate, 13)}
	if _, err := analyzePair(refWins, searchWins, 24); !errors.Is(err, ErrNoConfidence) {
		t.Fatalf("want ErrNoConfidence, got %v", err)
	}
}

// TestAnalyzePairConsensusIgnoresDialogueOnlyWindow: windows 1 and 3
// carry the true shift; window 2 is unrelated noise (its dub span sits
// in a gap the video speaks over) — it must not vote, and the two
// agreeing windows still decide.
func TestAnalyzePairConsensusIgnoresDialogueOnlyWindow(t *testing.T) {
	const k = 2.0
	video := wobble(24*60, 3)
	dub := shiftedCopy(k, video)
	ws := WindowSchedule(1440.0)
	refWins := make([][]float64, 0, len(ws))
	searchWins := make([][]float64, 0, len(ws))
	for i, w := range ws {
		refWin := video[int(w.RefStart*testRate) : int(w.RefStart*testRate)+int(ReferenceDur*testRate)]
		srchWin := dub[int(w.SearchStart*testRate) : int(w.SearchStart*testRate)+int(SearchDur*testRate)]
		if i == 1 {
			srchWin = noise(int(SearchDur*testRate), 21) // unrelated noise: must not vote
		}
		refWins = append(refWins, refWin)
		searchWins = append(searchWins, srchWin)
	}
	off, err := analyzePair(refWins, searchWins, 24)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if math.Abs(off.Seconds-k) > 0.05 {
		t.Fatalf("got %v, want ≈%v", off.Seconds, k)
	}
	if off.WindowsAgree != 2 {
		t.Fatalf("WindowsAgree = %d, want 2", off.WindowsAgree)
	}
}

func TestAnalyzePairMedianOfThree(t *testing.T) {
	// Three agreeing windows with per-window measurement noise
	// 1.966/2.000/2.040 (total spread 0.074 ≤ tolerance, so all three
	// vote together) → the median 2.000 wins; 2.000 is on the 24 fps
	// grid, so the snap is a no-op and the assertion isolates the
	// median stage.
	ref := wobble(60, 5)
	refWins := [][]float64{ref, ref, ref}
	searchWins := [][]float64{
		mkWindow(ref, 1.966),
		mkWindow(ref, 2.000),
		mkWindow(ref, 2.040),
	}
	off, err := analyzePair(refWins, searchWins, 24)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if math.Abs(off.Seconds-2.000) > 1e-9 {
		t.Fatalf("median = %v, want 2.000", off.Seconds)
	}
	if off.WindowsAgree != 3 {
		t.Fatalf("WindowsAgree = %d, want 3", off.WindowsAgree)
	}
}

// TestAnalyzePairConsensusRejectsSplit: the viable windows disagree
// (no pair within the tolerance) — no ≥2 agreement, so the verdict is
// no-confidence, never a coin flip.
func TestAnalyzePairConsensusRejectsSplit(t *testing.T) {
	ref := wobble(60, 5)
	refWins := [][]float64{ref, ref}
	searchWins := [][]float64{mkWindow(ref, 2.0), mkWindow(ref, 9.0)}
	if _, err := analyzePair(refWins, searchWins, 24); !errors.Is(err, ErrNoConfidence) {
		t.Fatalf("want ErrNoConfidence, got %v", err)
	}
}

func TestSnapToFrame(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		fps  float64
		want float64
	}{
		{"snaps_within_tolerance", 17.050, 24, 17.041666},
		{"already_on_grid", 6.250, 24, 6.250},
		// 1/48 s off the grid (max residual 0.020833 > SnapTolerance
		// 0.020): the only band the snap must NOT touch. (The plan
		// sketch's 17.100 sits 0.0167 from a frame and snaps under the
		// normative formula — replaced with a genuinely off-grid value.)
		{"keeps_off_grid", 17.020417, 24, 17.020417},
		{"default_fps", 2.000, 0, 2.000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fps := tc.fps
			if fps == 0 {
				fps = DefaultFPS
			}
			if got := snapToFrame(tc.in, fps); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWindowSchedule(t *testing.T) {
	ws := WindowSchedule(1440)
	want := []float64{90, 720, 960}
	if len(ws) != 3 {
		t.Fatalf("len = %d, want 3", len(ws))
	}
	for i, w := range ws {
		if w.RefStart != want[i] {
			t.Fatalf("RefStart[%d] = %v, want %v", i, w.RefStart, want[i])
		}
		if w.SearchStart != want[i]-ReferenceDur {
			t.Fatalf("SearchStart[%d] = %v, want %v", i, w.SearchStart, want[i]-ReferenceDur)
		}
	}
	if got := WindowSchedule(299); got != nil {
		t.Fatalf("D=299 → %+v, want nil", got)
	}
	ws = WindowSchedule(301)
	if len(ws) != 3 {
		t.Fatalf("D=301 → %d windows, want 3", len(ws))
	}
	for i, w := range ws {
		if w.RefStart+ReferenceDur > 300 {
			t.Fatalf("window %d start %v overruns the episode", i, w.RefStart)
		}
	}
}

func TestRMSOfPCM(t *testing.T) {
	pcm := []byte{
		0xe8, 0x03, 0x18, 0xfc, // 1000, -1000 (LE)
		0xe8, 0x03, 0x18, 0xfc, // 1000, -1000
	}
	if got := RMSOfPCM(pcm); math.Abs(got-1000) > 1e-6 {
		t.Fatalf("RMS = %v, want 1000", got)
	}
	if got := RMSOfPCM(nil); got != 0 {
		t.Fatalf("empty RMS = %v, want 0", got)
	}
	if got := rms([]float64{3000, -3000, 3000, -3000}); math.Abs(got-3000) > 1e-9 {
		t.Fatalf("rms = %v, want 3000", got)
	}
}

func zeros(n int) []float64 { return make([]float64, n) }

func noise(n int, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic synthetic signal, not security
	x := make([]float64, n)
	for i := range x {
		x[i] = rng.NormFloat64() * 6000
	}
	return x
}
