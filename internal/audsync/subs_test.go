package audsync

// PR173 v2 tests: the ASS subtitle parsing (the script's
// extract_speech_intervals line filter, verbatim) and build_sub_norm
// (the speech gate → active region → mean-centering). The sub_norm
// goldens were generated with the script's own build_sub_norm via
// `uv run --with numpy,scipy python3` (pinned float32 hex values).

import (
	"math"
	"testing"
)

// assFixture covers every skip class the script applies: sign/надп/op
// styles, the \pos/\move/\org/\p1/\fad tags, short text, malformed
// times, out-of-band durations — plus valid dialogues with commas and
// Cyrillic text (the rune-length rule).
const assFixture = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,204
Style: Sign,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,204
Style: надписи,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,204
Style: OP,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,204

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:02.50,Default,,0,0,0,,Первый диалог с, запятой
Dialogue: 0,0:00:03.00,0:00:04.00,Sign,,0,0,0,,Табличка на двери
Dialogue: 0,0:00:04.10,0:00:05.10,надписи,,0,0,0,,Надпись на стене
Dialogue: 0,0:00:05.20,0:00:06.20,OP,,0,0,0,,Опенинг текст
Dialogue: 0,0:00:06.30,0:00:07.30,Default,,0,0,0,,{\pos(1,2)}Позиционированный текст
Dialogue: 0,0:00:07.40,0:00:08.40,Default,,0,0,0,,{\move(1,2,3,4)}Движущийся текст
Dialogue: 0,0:00:08.50,0:00:09.50,Default,,0,0,0,,{\fad(100,100)}Плавный текст
Dialogue: 0,0:00:09.60,0:00:10.60,Default,,0,0,0,,{\org(9,9)}Опорная точка
Dialogue: 0,0:00:10.70,0:00:11.70,Default,,0,0,0,,{\p1}Векторная графика
Dialogue: 0,0:00:11.80,0:00:12.80,Default,,0,0,0,,хи
Dialogue: 0,0:00:12.90,0:00:13.90,Default,,0,0,0,,{\c&HFF&}Цветной{\r} текст тегов
Dialogue: 0,0:00:14.00,0:00:14.05,Default,,0,0,0,,Слишком короткая
Dialogue: 0,0:00:14.20,0:00:27.00,Default,,0,0,0,,Слишком длинная реплика
Dialogue: 0,0:00:28.00,0:00:28.10,Default,,0,0,0,,Мгновенная
Dialogue: 0,0:00:29.00,0:0осью:31.00,Default,,0,0,0,,Сломанное время
Dialogue: 0,0:00:30.00,0:00:32.00,Default,,0,0,0,,Второй валидный
Dialogue: ,0:00:33.00,0:00:34.00,Default,,0,0,0,,Пустой слой
Comment: 0,0:00:35.00,0:00:36.00,Default,,0,0,0,,Комментарий не диалог
`

// TestParseASSIntervals pins the line filter: exactly the four valid
// dialogues survive (comments, signs, tags, shorts, longs and broken
// times all drop; {\r} is only stripped, not a skip tag; an empty
// layer field is still a dialogue).
func TestParseASSIntervals(t *testing.T) {
	ivs := parseASSIntervals(assFixture)
	if len(ivs) != 4 {
		t.Fatalf("intervals = %d (%+v), want 4", len(ivs), ivs)
	}
	want := []assInterval{{1.0, 2.5}, {12.9, 13.9}, {30.0, 32.0}, {33.0, 34.0}}
	for i, w := range want {
		if ivs[i] != w {
			t.Fatalf("interval[%d] = %+v, want %+v", i, ivs[i], w)
		}
	}
}

// TestParseASSIntervalsShortTrackFallsBelowFloor: a real signs-only
// track yields a handful of intervals — below MinSubIntervals.
func TestParseASSIntervalsShortTrackFallsBelowFloor(t *testing.T) {
	ivs := parseASSIntervals("Dialogue: 0,0:00:01.00,0:00:02.00,Sign,,0,0,0,,Не диалог один\n" +
		"Dialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Единственная реплика")
	if len(ivs) != 1 || len(ivs) >= MinSubIntervals {
		t.Fatalf("intervals = %d, want 1 (< MinSubIntervals)", len(ivs))
	}
}

// TestBuildSubNormGolden pins the speech gate → active → mean-center
// math on the golden fixture intervals [(2.0,3.4), (5.0,6.1),
// (10.25,11.0)] (script build_sub_norm replica output).
func TestBuildSubNormGolden(t *testing.T) {
	sn := buildSubNorm([]assInterval{{2.0, 3.4}, {5.0, 6.1}, {10.25, 11.0}})
	if len(sn) != EnvBins {
		t.Fatalf("bins = %d, want %d", len(sn), EnvBins)
	}
	f32 := func(v float64) float64 { return float64(float32(v)) }
	// mean(gate[active]) = 0.27474746108055115 → gate=1 bins hold
	// 1−mean, gate=0 bins inside the active region hold −mean.
	wants := map[int]float64{
		33:  -0.27474746108055115, // first active bin (filter reach-back)
		100: 0.72525253891944885,  // inside the first interval's gate
		162: -0.27474746108055115, // gate ended (3.4−0.18 → bin 161)
		169: -0.27474746108055115, // active tail
	}
	for bin, w := range wants {
		if sn[bin] != f32(w) {
			t.Fatalf("sub_norm[%d] = %v (%x), want %v", bin, sn[bin], math.Float64bits(sn[bin]), f32(w))
		}
	}
	// The active region spans 495 bins starting at 33 (pinned).
	active := 0
	first := -1
	for i, v := range sn {
		if v != 0 {
			active++
			if first < 0 {
				first = i
			}
		}
	}
	if first != 33 || active != 495 {
		t.Fatalf("first active = %d, active count = %d; want 33 / 495", first, active)
	}
	// Outside the active region: exact zeros.
	if sn[0] != 0 || sn[700] != 0 || sn[EnvBins-1] != 0 {
		t.Fatal("sub_norm must be zero outside the active region")
	}
}

// TestBuildSubNormIntervalEdges pins the bin-boundary arithmetic:
// i0 = int(t0·50), i1 = int(max(t0+0.15, t1−0.18)·50) — the 0.15 s
// floor wins for short lines. The POSITIVE sub_norm bins are exactly
// the gate (non-gate active bins hold −mean < 0), so they pin i0/i1
// without exposing the gate. A long interval activates the dilation
// (one 7-bin gate alone is below the 0.05 threshold — 7/150 < 0.05).
func TestBuildSubNormIntervalEdges(t *testing.T) {
	sn := buildSubNorm([]assInterval{{10.0, 20.0}, {20.4, 20.45}})
	// Short line: floor → i1 = int(max(20.55, 20.27)·50) = 1027 →
	// gate [1020..1026]. It sits in the long line's dilation reach
	// (an isolated 7-bin gate never clears the 0.05 threshold).
	for i := 1020; i <= 1026; i++ {
		if sn[i] <= 0 {
			t.Fatalf("sub_norm[%d] = %v, want positive (the 0.15 s floor gate)", i, sn[i])
		}
	}
	if sn[1027] >= 0 {
		t.Fatalf("sub_norm[1027] = %v, want negative (past the floored gate end)", sn[1027])
	}
	// Long line: i1 = int(max(10.15, 19.82)·50) = 991 → gate [500..990].
	if sn[499] >= 0 || sn[990] <= 0 || sn[991] >= 0 {
		t.Fatalf("edges: sn[499]=%v sn[990]=%v sn[991]=%v; want gate [500..990]", sn[499], sn[990], sn[991])
	}
	// An interval past the canvas is dropped; an all-negative one too.
	sn = buildSubNorm([]assInterval{{2000, 2001}, {-5, -4}})
	for i, v := range sn {
		if v != 0 {
			t.Fatalf("sub_norm[%d] = %v, want 0 (out-of-canvas intervals drop)", i, v)
		}
	}
}
