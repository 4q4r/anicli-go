package audsync

// PR173 v2 tests: the DSP primitives of the owner's reference script
// (.sdd/pr173-sync-v2-reference.py), ported verbatim — the Butterworth
// bandpass (scipy butter(2, [300, 2800], btype="bandpass", fs=8000)),
// the lfilter direct-form II transposed recurrence, ndimage's
// uniform_filter1d (mode "reflect") and the full vocal-envelope
// pipeline. The golden numbers were generated ONCE with
//
//	uv run --with numpy,scipy python3 (the pinned commands live in
//	each test's comment)
//
// and are pinned as hex float literals — float32-exact where the
// script rounds to float32.

import (
	"encoding/binary"
	"math"
	"testing"
)

// hexf parses a pinned hex-float golden at compile time (literals are
// used inline; this helper only sharpens test failure messages).
func hexf(t *testing.T, name string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v (%x), want %v (%x)", name, got, math.Float64bits(got), want, math.Float64bits(want))
	}
}

// lcgFloats replicates the golden fixture's input exactly:
// x[i] = (((i*1103515245 + 12345) mod 2^31) / 2^31 − 0.5) · 2
func lcgFloats(n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = ((float64((i*1103515245+12345)%(1<<31)))/math.Pow(2, 31) - 0.5) * 2
	}
	return x
}

// TestButterworthCoefficientsGolden pins the filter constants.
// Generated: `uv run --with numpy,scipy python3 -c "from scipy import
// signal; print([float(v).hex() for v in signal.butter(2, [300, 2800],
// btype='bandpass', fs=8000)[0]]); ..."` — the (B, A) pair the whole
// v2 envelope rides on.
func TestButterworthCoefficientsGolden(t *testing.T) {
	wantB := []float64{
		0x1.ac330319bbb1fp-2, 0x0.0p+0, -0x1.ac330319bbb1fp-1, 0x0.0p+0, 0x1.ac330319bbb1fp-2,
	}
	wantA := []float64{
		0x1.0000000000000p+0, -0x1.ea4806927f0b8p-1, -0x1.6280e96be613fp-3,
		0x1.bc15c932b3a50p-6, 0x1.ad7f3ed84448cp-3,
	}
	if len(butterB) != 5 || len(butterA) != 5 {
		t.Fatalf("coefficient shapes: B %d A %d, want 5/5", len(butterB), len(butterA))
	}
	for i := range wantB {
		hexf(t, "B[i]", butterB[i], wantB[i])
		hexf(t, "A[i]", butterA[i], wantA[i])
	}
}

// TestBiquadStepLfilterGolden pins the DF2T recurrence against scipy
// lfilter. Generated: `uv run --with numpy,scipy python3 -c "from
// scipy import signal; import numpy as np; B, A =
// signal.butter(2, [300, 2800], btype='bandpass', fs=8000); x =
// np.array([(((i*1103515245 + 12345) % 2**31) / 2**31 - 0.5)*2 for i
// in range(24)]); print(', '.join(float(v).hex() for v in
// signal.lfilter(B, A, x)))"`.
func TestBiquadStepLfilterGolden(t *testing.T) {
	wantY := []float64{
		-0x1.ac31c0761befep-2, -0x1.8e27007622139p-2, -0x1.b87aeb55de980p-9,
		-0x1.85eb2ff3c6138p-5, 0x1.aa9507a638030p-5, 0x1.f8dd3d0d9fe92p-4,
		0x1.08450d2fe5464p-3, 0x1.3a4eadfe79bd8p-3, 0x1.3d830207916d0p-3,
		0x1.2a58d9b0c6715p-3, 0x1.14b613070c70cp-3, 0x1.e430ca42e13e2p-4,
		0x1.9a19a30f32254p-4, 0x1.50613c6b612e3p-4, 0x1.07e9be419af4cp-4,
		0x1.8c92c67b41e60p-5, 0x1.18df6bc2ea790p-5, 0x1.6c6b446a91050p-6,
		0x1.96a0235197180p-7, 0x1.30d454d03d4c0p-8, -0x1.3843c18613380p-10,
		-0x1.5dbac2ac9b320p-8, -0x1.079c46bf5ebb0p-7, -0x1.399a4842314c0p-7,
	}
	x := lcgFloats(len(wantY))
	var z [4]float64
	for i, v := range x {
		hexf(t, "y[i]", biquadStep(&butterB, &butterA, &z, v), wantY[i])
	}
}

// TestUniformFilterGolden pins uniform_filter1d(x, size, mode="reflect").
// Generated: `uv run --with numpy,scipy python3 -c "from scipy import
// ndimage; import numpy as np; u = np.arange(1.0, 11.0);
// print([float(v).hex() for v in ndimage.uniform_filter1d(u, size=4)]);
// print([float(v).hex() for v in ndimage.uniform_filter1d(np.array([1.,
// 2., 3.]), size=200)])"`. The size>n case pins the reflect boundary
// (edge-repeating) and the even-size window placement.
func TestUniformFilterGolden(t *testing.T) {
	u := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	want4 := []float64{1.5, 1.75, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.25}
	got := uniformFilter1d(u, 4)
	if len(got) != len(want4) {
		t.Fatalf("len = %d, want %d", len(got), len(want4))
	}
	for i := range want4 {
		if math.Abs(got[i]-want4[i]) > 1e-12 {
			t.Fatalf("f4[%d] = %v, want %v", i, got[i], want4[i])
		}
	}
	wantBig := []float64{
		0x1.0147ae147ae14p+1, 0x1.00a3d70a3d70ap+1, 0x1.feb851eb851ecp+0,
	}
	gotBig := uniformFilter1d([]float64{1, 2, 3}, 200)
	for i := range wantBig {
		if math.Abs(gotBig[i]-wantBig[i]) > 1e-12 {
			t.Fatalf("f200[%d] = %v, want %v", i, gotBig[i], wantBig[i])
		}
	}
}

// envFixture renders the golden fixture's stereo s16le PCM with
// integer-only math (identical values to the python generator: the
// float64 products are exact dyadics, so no rounding can diverge).
// L = LCG noise burst-gated on [0.2,1.2)s and [2.0,3.0)s; R = L·0.5+100.
func envFixture() []byte {
	const rate, total = 8000, 28000 // 3.5 s
	state := int64(987654321)
	out := make([]byte, 0, total*4)
	putS16 := func(dst []byte, v float64) {
		binary.LittleEndian.PutUint16(dst, uint16(int16(clampS16(math.Round(v))))) //nolint:gosec // G115: same s16 reinterpretation
	}
	frame := make([]byte, 4)
	for i := range total {
		state = (state*1103515245 + 12345) % (1 << 31)
		burst := (1600 <= i && i < 9600) || (16000 <= i && i < 24000)
		sl := 0.0
		if burst {
			sl = (float64(state)/math.Pow(2, 31) - 0.5) * 2 * 12000
		}
		sr := sl*0.5 + 100
		putS16(frame[0:2], sl)
		putS16(frame[2:4], sr)
		out = append(out, frame...)
	}
	return out
}

// TestEnvelopeGoldenFixture pins the full pipeline output (mid/side →
// bandpass → 50 Hz block RMS → log1p → high-pass → float32 bins)
// against the script replica. Generated by the same fixture run
// through .sdd/pr173-sync-v2-reference.py's get_vocal_envelope (uv
// run --with numpy,scipy; values pinned from that run).
func TestEnvelopeGoldenFixture(t *testing.T) {
	env := envelopeFromPCM(envFixture())
	if len(env) != EnvBins {
		t.Fatalf("bins = %d, want %d", len(env), EnvBins)
	}
	// The script stores float32 bins; the port reproduces them
	// bit-exact through the same rounding.
	f32 := func(v float64) float64 { return float64(float32(v)) }
	want := map[int]float64{
		0: -0x1.00c24a0000000p+2, 1: -0x1.034e780000000p+2, 2: -0x1.05df5c0000000p+2,
		3: -0x1.0863de0000000p+2, 4: -0x1.0ae4e40000000p+2, 5: -0x1.0d68040000000p+2,
		20: 0x1.8ea88a0000000p+1, 21: 0x1.8430f80000000p+1, 22: 0x1.912a900000000p+1, 23: 0x1.8473700000000p+1,
		88: -0x1.0566860000000p+2, 89: -0x1.02e2da0000000p+2, 90: -0x1.005acc0000000p+2, 91: -0x1.005acc0000000p+2,
		170: -0x1.ffe69c0000000p+1, 171: -0x1.ffe69c0000000p+1, 172: -0x1.ffe69c0000000p+1,
		173: -0x1.ffe69c0000000p+1, 174: -0x1.ffe69c0000000p+1,
	}
	for bin, w := range want {
		if env[bin] != f32(w) {
			t.Fatalf("env[%d] = %v (%x), want %v", bin, env[bin], math.Float64bits(env[bin]), f32(w))
		}
	}
	// The tail past the decoded content is zero-padding.
	for i := range 25 {
		if env[175+i] != 0 {
			t.Fatalf("env[%d] = %v, want 0 (padding)", 175+i, env[175+i])
		}
	}
}

// TestEnvelopeSilenceIsAllZero: a silent source carries no vocal
// energy — every bin zero (the log1p(0) law).
func TestEnvelopeSilenceIsAllZero(t *testing.T) {
	env := envelopeFromPCM(make([]byte, 8000*4)) // 1 s of digital silence
	for i, v := range env[:EnvRate] {
		if v != 0 {
			t.Fatalf("env[%d] = %v, want 0 for silence", i, v)
		}
	}
	if envelopeActivity(env) != 0 {
		t.Fatalf("activity = %d, want 0", envelopeActivity(env))
	}
}

// TestEnvelopeTruncatesToMaxBins: content past MAX_SEC falls off the
// fixed 72000-bin canvas (the script's out[:n_frames] with
// n_frames = min(..., N_BINS)).
func TestEnvelopeTruncatesToMaxBins(t *testing.T) {
	// 1441 s of silence with one audible dual-mono burst at the head.
	pcm := make([]byte, (1441*8000+160)*4)
	for i := range 1600 { // 0.2 s burst at 1 kHz-ish block level
		v := float64(12000)
		if (i/40)%2 == 1 {
			v = -12000
		}
		b := s16Bytes([]float64{v, v}) // dual mono: mid = v, side = 0
		copy(pcm[i*4:], b)
	}
	env := envelopeFromPCM(pcm)
	if len(env) != EnvBins {
		t.Fatalf("bins = %d, want %d (truncation)", len(env), EnvBins)
	}
	if envelopeActivity(env) < 10 {
		t.Fatalf("activity = %d, want a real burst to register", envelopeActivity(env))
	}
}

// TestEnvelopeOddTrailingBytes: a truncated final frame is ignored,
// never a panic (the decode conversion's convention).
func TestEnvelopeOddTrailingBytes(t *testing.T) {
	full := envFixture()
	for _, cut := range []int{1, 2, 3} {
		env := envelopeFromPCM(full[:len(full)-cut])
		if len(env) != EnvBins {
			t.Fatalf("cut %d: bins = %d", cut, len(env))
		}
	}
}
