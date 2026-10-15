package audsync

// PR173 v2 correlation tests: the full-length band-limited lag search
// in the envelope domain (the script's measure_lag). Synthetic spike
// envelopes make the exact lag assertable — a whole-bin shift is
// reproduced exactly (50 Hz bins; 150 bins = +3.000 s).

import (
	"testing"
)

// spikeEnv builds a deterministic APERIODIC envelope-like signal:
// sharp 2–5-bin bursts with irregular gaps (an LCG picks the rhythm),
// so the autocorrelation has one unique spike — the exact lag is
// assertable.
func spikeEnv() []float64 {
	env := make([]float64, EnvBins)
	state := int64(424242)
	next := func() int64 {
		state = (state*1103515245 + 12345) % (1 << 31)
		return state
	}
	b := 0
	for b < EnvBins {
		width := 2 + int(next()%4)
		for k := 0; k < width && b+k < EnvBins; k++ {
			env[b+k] = 1 + float64(next()%17)*0.25
		}
		b += width + 11 + int(next()%30)
	}
	return env
}

// delayEnv shifts src by bins (positive — delayed; the donor-lag
// semantics), zero-filling the vacated head/tail.
func delayEnv(src []float64, bins int) []float64 {
	out := make([]float64, len(src))
	if bins >= 0 {
		copy(out[bins:], src[:len(src)-bins])
	} else {
		copy(out, src[-bins:])
	}
	return out
}

// TestMeasureLagExactPositive: a donor delayed by exactly 150 bins
// (+3.000 s — the owner's canonical synthetic) measures +3.000.
func TestMeasureLagExactPositive(t *testing.T) {
	ref := spikeEnv()
	aud := delayEnv(ref, 150)
	lag, err := measureLag(aud, ref, SubLagMin, SubLagMax)
	if err != nil {
		t.Fatalf("measureLag: %v", err)
	}
	if lag != 3.0 {
		t.Fatalf("lag = %v, want exactly 3.0", lag)
	}
}

// TestMeasureLagNegative: a donor EARLY by 25 bins (its content sits
// ahead — the pad geometry) measures −0.5.
func TestMeasureLagNegative(t *testing.T) {
	ref := spikeEnv()
	aud := delayEnv(ref, -25)
	lag, err := measureLag(aud, ref, AudioLagMin, AudioLagMax)
	if err != nil {
		t.Fatalf("measureLag: %v", err)
	}
	if lag != -0.5 {
		t.Fatalf("lag = %v, want exactly -0.5", lag)
	}
}

// TestMeasureLagZero: identical envelopes are in sync.
func TestMeasureLagZero(t *testing.T) {
	ref := spikeEnv()
	lag, err := measureLag(ref, ref, SubLagMin, SubLagMax)
	if err != nil {
		t.Fatalf("measureLag: %v", err)
	}
	if lag != 0 {
		t.Fatalf("lag = %v, want 0", lag)
	}
}

// TestMeasureLagBandClamp pins the band semantics: a +30 s donor lies
// outside the subtitle band (−3…+20) — the in-band argmax is BOUNDED
// by the band (it cannot report +30); the audio band (−60…+60) finds
// it exactly. The lag SEMANTICS are identical from the caller's view
// for both reference kinds (positive = donor later = trim).
func TestMeasureLagBandClamp(t *testing.T) {
	ref := spikeEnv()
	aud := delayEnv(ref, 30*EnvRate)
	subLag, err := measureLag(aud, ref, SubLagMin, SubLagMax)
	if err != nil {
		t.Fatalf("measureLag sub band: %v", err)
	}
	if subLag < SubLagMin || subLag > SubLagMax || subLag == 30.0 {
		t.Fatalf("sub-band lag = %v, want an in-band value (the true +30 is out of band)", subLag)
	}
	audioLag, err := measureLag(aud, ref, AudioLagMin, AudioLagMax)
	if err != nil {
		t.Fatalf("measureLag audio band: %v", err)
	}
	if audioLag != 30.0 {
		t.Fatalf("audio-band lag = %v, want exactly 30.0", audioLag)
	}
}

// TestMeasureLagRejectsEmpty: an empty envelope is a caller bug — the
// primitive refuses, never silently returns 0.
func TestMeasureLagRejectsEmpty(t *testing.T) {
	if _, err := measureLag(nil, spikeEnv(), -1, 1); err == nil {
		t.Fatal("empty aud must error")
	}
	if _, err := measureLag(spikeEnv(), nil, -1, 1); err == nil {
		t.Fatal("empty ref must error")
	}
}

func TestNextPow2(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{1, 1}, {2, 2}, {3, 4}, {191999, 262144}, {262144, 262144},
	} {
		if got := nextPow2(tc.in); got != tc.want {
			t.Fatalf("nextPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
