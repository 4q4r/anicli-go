package audsync

// The pure cross-correlation core (research §3.3):
// c = IFFT( FFT(x) · conj(FFT(y)) ), N = next_pow2(len(x)+len(y)−1);
// k = argmax(seq); D = (N−k) mod N — the delay of srch relative to
// ref (srch[i] = ref[i−D]). Prominence is the peak against the mean
// absolute correlation — uncorrelated signals stay far below
// MinProminence.

import (
	"errors"
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// nextPow2 rounds n up to the next power of two.
func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// CrossCorrelate locates srch inside ref's timeline: it returns the
// delay D in samples (srch[i] = ref[i−D]) and the peak prominence —
// seq[k] over the mean absolute correlation. Empty inputs are an
// error; there is no other failure mode (the caller gates
// prominence/RMS itself).
func CrossCorrelate(ref, srch []float64) (delay int, prominence float64, err error) {
	if len(ref) == 0 || len(srch) == 0 {
		return 0, 0, errors.New("audsync: empty signal")
	}
	n := nextPow2(len(ref) + len(srch) - 1)
	px := make([]float64, n)
	copy(px, ref)
	py := make([]float64, n)
	copy(py, srch)
	fft := fourier.NewFFT(n)
	x := fft.Coefficients(nil, px)
	y := fft.Coefficients(nil, py)
	for i := range x {
		x[i] *= cmplx.Conj(y[i])
	}
	seq := fft.Sequence(nil, x)
	k := 0
	sum := 0.0
	for i, v := range seq {
		av := math.Abs(v)
		sum += av
		if v > seq[k] {
			k = i
		}
	}
	return (n - k) % n, seq[k] / (sum / float64(n)), nil
}
