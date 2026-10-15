package audsync

// The v2 lag search (the script's measure_lag): the FULL-length
// linear cross-correlation of two envelopes via zero-padded FFT, the
// lag axis scaled to seconds, and the argmax restricted to the
// reference kind's band. Sign convention (pinned by the synthetic
// tests): lag > 0 — the donor's audio events are LATER than the
// reference's events → trim the donor's head; lag < 0 — pad.

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

// correlateFull computes the linear full cross-correlation of a and b:
// result[j] = Σ a[m]·b[m−ℓ] with the lag ℓ = j − (len(b)−1) — the
// numpy/scipy "full" convention. a and b may differ in length; both
// are zero-padded to the next power of two.
func correlateFull(a, b []float64) ([]float64, error) {
	if len(a) == 0 || len(b) == 0 {
		return nil, errors.New("audsync: empty signal")
	}
	na, nb := len(a), len(b)
	n := nextPow2(na + nb - 1)
	pa := make([]float64, n)
	pb := make([]float64, n)
	copy(pa, a)
	copy(pb, b)
	fft := fourier.NewFFT(n)
	fa := fft.Coefficients(nil, pa)
	fb := fft.Coefficients(nil, pb)
	for i := range fa {
		fa[i] *= cmplx.Conj(fb[i])
	}
	seq := fft.Sequence(nil, fa)
	// The circular result maps onto the linear full correlation with
	// lag ℓ = j−(nb−1) read at circular index ℓ (≥0) or n+ℓ (<0):
	// corr[j] = seq[(j−(nb−1)) mod n].
	corr := make([]float64, na+nb-1)
	for j := range corr {
		idx := (j - (nb - 1)) % n
		if idx < 0 {
			idx += n
		}
		corr[j] = seq[idx]
	}
	return corr, nil
}

// measureLag returns the band-limited peak lag of aud against ref in
// SECONDS: the script's measure_lag(aud_env, sub_norm, min_lag,
// max_lag), verbatim — the full correlation, the lag axis ÷ EnvRate,
// argmax over the band (first max on ties, numpy's argmax
// convention). lag > 0 — aud's events are later than ref's (trim the
// donor head); lag < 0 — pad.
func measureLag(aud, ref []float64, minLag, maxLag float64) (float64, error) {
	corr, err := correlateFull(aud, ref)
	if err != nil {
		return 0, err
	}
	bestBin := -1
	bestVal := math.Inf(-1)
	for j, v := range corr {
		lag := float64(j-(len(ref)-1)) / EnvRate
		if lag < minLag || lag > maxLag {
			continue
		}
		if v > bestVal {
			bestVal = v
			bestBin = j
		}
	}
	if bestBin < 0 {
		return 0, errors.New("audsync: empty lag band")
	}
	return float64(bestBin-(len(ref)-1)) / EnvRate, nil
}
