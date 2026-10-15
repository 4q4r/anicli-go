package audsync

// The v2 vocal envelope (the owner's reference script
// .sdd/pr173-sync-v2-reference.py, get_vocal_envelope — ported
// verbatim): a whole-source 8 kHz STEREO s16le decode feeds the
// mid/side separation, a 2nd-order Butterworth bandpass (300–2800 Hz,
// scipy butter constants pinned below), 50 Hz block RMS and the
// log1p(max(0, rms_mid − rms_side)) vocal isolation, a 200-frame
// uniform high-pass, and a fixed 72000-bin canvas (1440 s at 50 Hz).
//
// The filter runs as a direct-form II transposed recurrence — the
// scipy lfilter structure — applied sample-by-sample while streaming
// the PCM, so the whole-source decode never materializes in memory.

import "math"

// The scipy signal.butter(2, [300, 2800], btype="bandpass", fs=8000)
// coefficients, generated once and pinned verbatim (golden test
// TestButterworthCoefficientsGolden cites the command). a[0] is the
// unit normalization lfilter expects.
var (
	butterB = [5]float64{0x1.ac330319bbb1fp-2, 0x0.0p+0, -0x1.ac330319bbb1fp-1, 0x0.0p+0, 0x1.ac330319bbb1fp-2}
	butterA = [5]float64{0x1.0000000000000p+0, -0x1.ea4806927f0b8p-1, -0x1.6280e96be613fp-3, 0x1.bc15c932b3a50p-6, 0x1.ad7f3ed84448cp-3}
)

// biquadStep advances the direct-form II transposed recurrence one
// sample: y[n] = b0·x[n] + z0[n−1], z-row shifted per the scipy
// lfilter structure. z carries the 4 delay elements (a 5-tap filter).
func biquadStep(b, a *[5]float64, z *[4]float64, x float64) float64 {
	y := b[0]*x + z[0]
	z[0] = b[1]*x + z[1] - a[1]*y
	z[1] = b[2]*x + z[2] - a[2]*y
	z[2] = b[3]*x + z[3] - a[3]*y
	z[3] = b[4]*x - a[4]*y
	return y
}

// uniformFilter1d ports scipy.ndimage.uniform_filter1d(x, size,
// mode="reflect"): a centered moving mean whose window spans
// [i−size/2, i+size−1−size/2] and whose out-of-range indices reflect
// off the edges REPEATING the edge value (ndimage "reflect" = numpy
// pad "symmetric"; pinned by TestUniformFilterGolden, including the
// size>n case the short decodes hit).
func uniformFilter1d(x []float64, size int) []float64 {
	n := len(x)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	if size < 1 {
		size = 1
	}
	period := 2 * n
	reflect := func(i int) int {
		m := ((i % period) + period) % period
		if m >= n {
			m = period - 1 - m
		}
		return m
	}
	half := size / 2
	sum := 0.0
	for j := -half; j < size-half; j++ {
		sum += x[reflect(j)]
	}
	out[0] = sum / float64(size)
	for i := 1; i < n; i++ {
		sum += x[reflect(i+size-1-half)] - x[reflect(i-1-half)]
		out[i] = sum / float64(size)
	}
	return out
}

// envelopeFromPCM builds the fixed EnvBins vocal envelope from a
// whole-source s16le STEREO PCM buffer at the analysis rate (the
// script's get_vocal_envelope math on already-decoded bytes). An odd
// trailing sample is ignored; digital silence produces an all-zero
// envelope; content past MAX_SEC truncates onto the fixed canvas.
func envelopeFromPCM(pcm []byte) []float64 {
	var zMid, zSide [4]float64
	var sumMid, sumSide float64
	fill := 0
	vocal := make([]float64, 0, EnvBins)
	block := func() {
		if fill < EnvStep {
			return
		}
		rmsM := math.Sqrt(sumMid / EnvStep)
		rmsS := math.Sqrt(sumSide / EnvStep)
		d := rmsM - rmsS
		if d < 0 {
			d = 0
		}
		if len(vocal) < EnvBins {
			vocal = append(vocal, math.Log1p(d))
		}
		sumMid, sumSide, fill = 0, 0, 0
	}
	for off := 0; off+3 < len(pcm); off += 4 {
		l := float64(int16(uint16(pcm[off]) | uint16(pcm[off+1])<<8))   //nolint:gosec // G115: PCM s16le IS the two's-complement reinterpretation
		r := float64(int16(uint16(pcm[off+2]) | uint16(pcm[off+3])<<8)) //nolint:gosec // G115: same s16 reinterpretation
		mid := biquadStep(&butterB, &butterA, &zMid, 0.5*(l+r))
		side := biquadStep(&butterB, &butterA, &zSide, 0.5*(l-r))
		sumMid += mid * mid
		sumSide += side * side
		fill++
		block()
	}
	filtered := uniformFilter1d(vocal, 4*EnvRate)
	out := make([]float64, EnvBins)
	for i, v := range vocal {
		out[i] = float64(float32(v - filtered[i])) // the script's float32 canvas
	}
	return out
}

// envelopeActivity counts the non-zero bins — the envelope-domain
// silence gate (the honest translation of v1's MinRMS: an envelope
// with no active bins carries no correlatable structure at all).
func envelopeActivity(env []float64) int {
	n := 0
	for _, v := range env {
		if v != 0 {
			n++
		}
	}
	return n
}
