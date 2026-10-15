package audsync

// The ASS subtitle leg of the v2 reference hierarchy (the script's
// extract_speech_intervals line filter and build_sub_norm — ported
// verbatim): Dialogue lines are filtered by style, inline tags, text
// length and duration; the survivors become a 50 Hz speech gate that
// dilates into an active region and mean-centers into the sub
// envelope the lag search correlates against.

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// assInterval is one usable speech interval in seconds.
type assInterval struct {
	start, end float64
}

// The script's skip lists, verbatim: styles that carry signs/typesets
// (case-insensitive substring match on the lowercased style) and the
// inline tags that mark positioned/moved/drawn events (matched on the
// RAW text, no case folding).
var (
	skipStyles = []string{"sign", "надп", "title", "op", "ed", "song", "logo", "caption", "preview", "next", "песн"}
	skipTags   = []string{`\pos`, `\move`, `\org`, `\p1`, `\fad`}
	assTagRe   = regexp.MustCompile(`\{.*?\}`)
)

// parseASSTime parses "H:MM:SS.cc" (the ASS time format; stray
// whitespace tolerated the way the script's strip() does).
func parseASSTime(s string) (float64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0, strconv.ErrSyntax
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err != nil {
		return 0, err
	}
	return float64(h)*3600 + float64(m)*60 + sec, nil
}

// parseASSIntervals extracts the usable speech intervals from one ASS
// track's text (the per-track body of the script's
// extract_speech_intervals). Every filter is the script's, verbatim:
// ≥10 comma fields, style/tag skips, {...} stripping, \N → space,
// rune length ≥ 3, duration 0.15–12.0 s.
func parseASSIntervals(ass string) []assInterval {
	var ivs []assInterval
	for _, line := range strings.Split(ass, "\n") {
		if !strings.HasPrefix(line, "Dialogue:") {
			continue
		}
		parts := strings.SplitN(line, ",", 10)
		if len(parts) < 10 {
			continue
		}
		style := strings.ToLower(parts[3])
		skip := false
		for _, k := range skipStyles {
			if strings.Contains(style, k) {
				skip = true
				break
			}
		}
		if !skip {
			for _, tag := range skipTags {
				if strings.Contains(parts[9], tag) {
					skip = true
					break
				}
			}
		}
		if skip {
			continue
		}
		clean := strings.TrimSpace(strings.ReplaceAll(assTagRe.ReplaceAllString(parts[9], ""), `\N`, " "))
		if utf8.RuneCountInString(clean) < 3 {
			continue
		}
		t0, err0 := parseASSTime(parts[1])
		t1, err1 := parseASSTime(parts[2])
		if err0 != nil || err1 != nil {
			continue
		}
		if d := t1 - t0; d < 0.15 || d > 12.0 {
			continue
		}
		ivs = append(ivs, assInterval{start: t0, end: t1})
	}
	return ivs
}

// buildSubNorm renders the subtitle reference envelope (the script's
// build_sub_norm): the intervals mark 1.0 into a 50 Hz gate over
// [t0, max(t0+0.15, t1−0.18)) bins; a 150-bin uniform dilation marks
// the active region; inside it the gate mean-centers (speech bins
// above the region mean, silence below) — the correlation shape.
func buildSubNorm(intervals []assInterval) []float64 {
	gate := make([]float64, EnvBins)
	for _, iv := range intervals {
		i0 := int(iv.start * EnvRate)
		end := math.Max(iv.start+0.15, iv.end-0.18)
		i1 := int(end * EnvRate)
		if 0 <= i0 && i0 < i1 && i1 < EnvBins {
			for i := i0; i < i1; i++ {
				gate[i] = 1.0
			}
		}
	}
	active := uniformFilter1d(gate, 3*EnvRate)
	mean := 0.0
	n := 0
	for i, v := range gate {
		if active[i] > 0.05 {
			mean += v
			n++
		}
	}
	if n > 0 {
		mean /= float64(n)
	}
	out := make([]float64, EnvBins)
	for i := range out {
		if active[i] > 0.05 {
			out[i] = float64(float32(gate[i] - mean))
		}
	}
	return out
}
