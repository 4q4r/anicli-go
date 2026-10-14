package audsync

// PR165b T2 tests: the pure FFT cross-correlation core (research
// §3.3). Synthetic signals only — a dub is the reference shifted by a
// known lag plus noise, so the exact delay is assertable.

import (
	"math"
	"math/rand"
	"testing"
)

func TestCrossCorrelateFindsExactDelay(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic synthetic signal, not security
	// Broadband reference (noise-dominated with a tonal trace): real
	// dialogue is broadband, and its autocorrelation is a sharp spike.
	// A pure sinusoid would correlate broadly at ANY lag and never
	// pass the peak/mean prominence gate.
	x := make([]float64, 48000) // 6 c @8k
	for i := range x {
		x[i] = rng.NormFloat64() + 0.3*math.Sin(2*math.Pi*61*float64(i)/8000)
	}
	for _, tc := range []struct {
		name  string
		delay int
	}{
		{"small", 12345}, {"large", 98765},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srch := make([]float64, 144000)
			for i := tc.delay; i < len(srch) && i-tc.delay < len(x); i++ {
				srch[i] = x[i-tc.delay] // srch[i] = ref[i−D]
			}
			d, prom, err := CrossCorrelate(x, srch)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if d != tc.delay {
				t.Fatalf("delay = %d, want %d", d, tc.delay)
			}
			if prom < MinProminence {
				t.Fatalf("prominence = %v", prom)
			}
		})
	}
}

func TestCrossCorrelateUncorrelatedLowProminence(t *testing.T) {
	rng := rand.New(rand.NewSource(2)) //nolint:gosec // deterministic synthetic signal, not security
	a := make([]float64, 48000)
	b := make([]float64, 144000)
	for i := range a {
		a[i] = rng.NormFloat64()
	}
	for i := range b {
		b[i] = rng.NormFloat64()
	}
	if _, prom, err := CrossCorrelate(a, b); err != nil || prom >= MinProminence {
		t.Fatalf("prom = %v err = %v, want prom < %v", prom, err, MinProminence)
	}
}

func TestCrossCorrelateRejectsEmpty(t *testing.T) {
	if _, _, err := CrossCorrelate(nil, []float64{1}); err == nil {
		t.Fatal("empty ref must error")
	}
	if _, _, err := CrossCorrelate([]float64{1}, nil); err == nil {
		t.Fatal("empty search must error")
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
