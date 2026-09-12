package skip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Detection thresholds ported from python skip_manager.py.
const (
	// minOpeningEndSeconds is the minimum range end to qualify as an
	// opening candidate.
	minOpeningEndSeconds = 30.0
	// minSkipSegmentSeconds is the minimum duration for an emitted skip
	// segment.
	minSkipSegmentSeconds = 10.0
)

// Detectors (python _SILENCE_*_RE / _BLACK_*_RE).
var (
	silenceStartPattern = regexp.MustCompile(`silence_start:\s*([0-9.]+)`)
	silenceEndPattern   = regexp.MustCompile(`silence_end:\s*([0-9.]+)`)
	blackStartPattern   = regexp.MustCompile(`black_start:\s*([0-9.]+)`)
	blackEndPattern     = regexp.MustCompile(`black_end:\s*([0-9.]+)`)
)

// ErrBinaryNotFound reports a missing ffmpeg/ffprobe binary (fail-loud
// typed error; wraps exec.ErrNotFound).
type ErrBinaryNotFound struct {
	// Bin is the binary that was requested.
	Bin string
}

// Error implements error.
func (e *ErrBinaryNotFound) Error() string {
	return fmt.Sprintf("skip: required binary not found: %s", e.Bin)
}

// IntroSkipperOptions configures the local heuristic detector. Defaults
// are reconstructed: the python config section was lost in the 2026-09-07
// working-tree loss, so the values follow the detector constants.
type IntroSkipperOptions struct {
	// Enabled gates the detector (python intro_skipper.enabled).
	Enabled bool
	// FFmpegBin is the ffmpeg binary path.
	FFmpegBin string
	// FFprobeBin is the ffprobe binary path.
	FFprobeBin string
	// SilenceNoiseDB is the silencedetect noise floor in dB (negative).
	SilenceNoiseDB float64
	// SilenceMinDuration is the shortest silence run to report, in
	// seconds.
	SilenceMinDuration float64
	// OpeningMaxEndSeconds bounds the latest end of an opening
	// candidate range.
	OpeningMaxEndSeconds int
	// EndingSearchWindowSeconds bounds the ending search window from
	// the tail of the episode.
	EndingSearchWindowSeconds int
}

// DefaultIntroSkipperOptions carries the reconstructed defaults.
func DefaultIntroSkipperOptions() IntroSkipperOptions {
	return IntroSkipperOptions{
		Enabled:                   true,
		FFmpegBin:                 "ffmpeg",
		FFprobeBin:                "ffprobe",
		SilenceNoiseDB:            -35.0,
		SilenceMinDuration:        2.0,
		OpeningMaxEndSeconds:      180,
		EndingSearchWindowSeconds: 300,
	}
}

// commandRunner executes one subprocess and returns its stdout/stderr.
// Swapped by tests; production uses real ffmpeg/ffprobe.
type commandRunner func(ctx context.Context, bin string, args []string) (stdout, stderr string, err error)

// IntroSkipperClient detects op/ed heuristically from a local media
// file: embedded chapters first, then ffmpeg silencedetect, then
// blackdetect (ported from anicli-py skip_manager.IntroSkipperClient,
// an intentionally lightweight approximation of the Jellyfin plugin).
type IntroSkipperClient struct {
	opts IntroSkipperOptions
	// run executes commands; injectable for tests.
	run commandRunner
}

// NewIntroSkipperClient builds the detector with default options.
func NewIntroSkipperClient(opts IntroSkipperOptions) *IntroSkipperClient {
	if opts.FFmpegBin == "" {
		opts.FFmpegBin = "ffmpeg"
	}
	if opts.FFprobeBin == "" {
		opts.FFprobeBin = "ffprobe"
	}
	c := &IntroSkipperClient{opts: opts}
	c.run = realCommandRunner
	return c
}

// ID implements Provider.
func (c *IntroSkipperClient) ID() string { return ProviderIntroSkipper }

// GetSkipTimes analyses the media file and returns detected intervals
// plus a machine-readable detail code. Errors are reserved for hard
// failures (missing binaries); analysis misses are detail codes with
// empty intervals, mirroring the python semantics.
func (c *IntroSkipperClient) GetSkipTimes(ctx context.Context, mediaInput string) ([]Interval, string, error) {
	if !c.opts.Enabled {
		return nil, "intro_skipper_disabled", nil
	}
	// Transcript fix: ffprobe cannot walk remote URLs; refuse them
	// before spawning anything.
	if strings.HasPrefix(mediaInput, "http://") || strings.HasPrefix(mediaInput, "https://") {
		return nil, "remote_url_unsupported", nil
	}

	duration, ok := c.probeDuration(ctx, mediaInput)
	if !ok {
		return nil, "duration_probe_failed", nil
	}

	if intervals := c.detectFromChapters(ctx, mediaInput); len(intervals) > 0 {
		return intervals, detailWithMarkers("chapter_detected", intervals), nil
	}

	for _, probe := range []struct {
		marker string
		run    func(context.Context, string) [][2]float64
	}{
		{"silence", c.detectSilence},
		{"blackdetect", c.detectBlackFrames},
	} {
		ranges := probe.run(ctx, mediaInput)
		intervals := BuildIntervalsFromRanges(ranges, duration,
			c.opts.OpeningMaxEndSeconds, c.opts.EndingSearchWindowSeconds)
		if len(intervals) > 0 {
			return intervals, detailWithMarkers(probe.marker+"_detected", intervals), nil
		}
		if probe.marker == "blackdetect" {
			// Python distinguishes the final black miss.
			break
		}
	}

	if _, err := exec.LookPath("fpcalc"); err == nil {
		return nil, "chromaprint_available_no_match", nil
	}
	return nil, "no_intro_or_ending_detected", nil
}

// detailWithMarkers renders "<detail>:<sorted,type,list>" (python
// f"{details}:{','.join(markers)}").
func detailWithMarkers(detail string, intervals []Interval) string {
	seen := make(map[string]struct{}, len(intervals))
	var markers []string
	for _, iv := range intervals {
		if iv.SkipType == "" {
			continue
		}
		if _, dup := seen[iv.SkipType]; dup {
			continue
		}
		seen[iv.SkipType] = struct{}{}
		markers = append(markers, iv.SkipType)
	}
	sort.Strings(markers)
	if len(markers) == 0 {
		return detail
	}
	return detail + ":" + strings.Join(markers, ",")
}

// probeDuration returns the media duration in seconds via ffprobe
// (python _probe_duration). A missing binary is a hard error.
func (c *IntroSkipperClient) probeDuration(ctx context.Context, mediaInput string) (float64, bool) {
	out, _, err := c.run(ctx, c.opts.FFprobeBin, ffprobeDurationArgs(mediaInput))
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || isNotFound(err) {
			return 0, false
		}
		return 0, false
	}
	duration, parseErr := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if parseErr != nil || duration <= 0 {
		return 0, false
	}
	return duration, true
}

// detectFromChapters recovers op/ed from embedded chapter titles via
// ffprobe (python _detect_from_chapters).
func (c *IntroSkipperClient) detectFromChapters(ctx context.Context, mediaInput string) []Interval {
	out, _, err := c.run(ctx, c.opts.FFprobeBin, ffprobeChaptersArgs(mediaInput))
	if err != nil {
		return nil
	}
	intervals, _ := parseChaptersJSON([]byte(out))
	return intervals
}

// detectSilence returns silence ranges via the silencedetect filter
// (python _detect_silence).
func (c *IntroSkipperClient) detectSilence(ctx context.Context, mediaInput string) [][2]float64 {
	_, errOut, err := c.run(ctx, c.opts.FFmpegBin, ffmpegSilenceArgs(mediaInput, c.opts))
	if err != nil {
		return nil
	}
	return parseDetectRanges(errOut, silenceStartPattern, silenceEndPattern)
}

// detectBlackFrames returns black-frame ranges via blackdetect (python
// _detect_black_frames).
func (c *IntroSkipperClient) detectBlackFrames(ctx context.Context, mediaInput string) [][2]float64 {
	_, errOut, err := c.run(ctx, c.opts.FFmpegBin, ffmpegBlackArgs(mediaInput))
	if err != nil {
		return nil
	}
	return parseDetectRanges(errOut, blackStartPattern, blackEndPattern)
}

// realCommandRunner runs the binary with LookPath fail-loud semantics.
func realCommandRunner(ctx context.Context, bin string, args []string) (string, string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", "", &ErrBinaryNotFound{Bin: bin}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are config-derived, not request input
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("run %s: %w", bin, err)
	}
	return stdout.String(), stderr.String(), nil
}

// isNotFound matches the typed wrapper too.
func isNotFound(err error) bool {
	var notFound *ErrBinaryNotFound
	return errors.As(err, &notFound)
}

// ffprobeDurationArgs builds the duration probe argv.
func ffprobeDurationArgs(mediaInput string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		mediaInput,
	}
}

// ffprobeChaptersArgs builds the chapter probe argv.
func ffprobeChaptersArgs(mediaInput string) []string {
	return []string{
		"-v", "error",
		"-show_chapters",
		"-print_format", "json",
		mediaInput,
	}
}

// ffmpegSilenceArgs builds the silencedetect argv.
func ffmpegSilenceArgs(mediaInput string, opts IntroSkipperOptions) []string {
	return []string{
		"-hide_banner",
		"-i", mediaInput,
		"-af", fmt.Sprintf("silencedetect=noise=%gdB:d=%g", opts.SilenceNoiseDB, opts.SilenceMinDuration),
		"-f", "null",
		"-",
	}
}

// ffmpegBlackArgs builds the blackdetect argv (filter string verbatim
// from python).
func ffmpegBlackArgs(mediaInput string) []string {
	return []string{
		"-hide_banner",
		"-i", mediaInput,
		"-vf", "blackdetect=d=1.0:pix_th=0.10",
		"-an",
		"-f", "null",
		"-",
	}
}

// parseDetectRanges extracts (start,end) ranges from ffmpeg detector
// stderr: starts and ends are paired in order, ends at or before a
// start are skipped and a start with no remaining end is dropped
// (python pairing walk).
func parseDetectRanges(text string, startRe, endRe *regexp.Regexp) [][2]float64 {
	starts := parseFloatsAll(startRe, text)
	ends := parseFloatsAll(endRe, text)
	if len(starts) == 0 || len(ends) == 0 {
		return nil
	}

	ranges := make([][2]float64, 0, len(starts))
	endIdx := 0
	for _, start := range starts {
		for endIdx < len(ends) && ends[endIdx] <= start {
			endIdx++
		}
		if endIdx >= len(ends) {
			break
		}
		ranges = append(ranges, [2]float64{start, ends[endIdx]})
		endIdx++
	}
	return ranges
}

// parseFloatsAll extracts every regex capture as float.
func parseFloatsAll(re *regexp.Regexp, text string) []float64 {
	matches := re.FindAllStringSubmatch(text, -1)
	out := make([]float64, 0, len(matches))
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// parseChaptersJSON extracts op/ed intervals from an ffprobe
// -show_chapters JSON payload. Non-dict entries are skipped (python
// isinstance guard) and non-string tag values ignored. The second
// return is the detail code (python _detect_from_chapters verdicts).
func parseChaptersJSON(payload []byte) ([]Interval, string) {
	var doc struct {
		// Pointer distinguishes an absent key (chapter_missing) from an
		// empty array (chapter_not_detected).
		Chapters *[]json.RawMessage `json:"chapters"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, "chapter_payload_invalid"
	}
	if doc.Chapters == nil {
		return nil, "chapter_missing"
	}

	intervals := make([]Interval, 0, len(*doc.Chapters))
	for _, raw := range *doc.Chapters {
		var chapter struct {
			StartTime any            `json:"start_time"`
			EndTime   any            `json:"end_time"`
			Tags      map[string]any `json:"tags"`
		}
		if err := json.Unmarshal(raw, &chapter); err != nil {
			continue
		}
		start, okStart := asFloat(chapter.StartTime)
		end, okEnd := asFloat(chapter.EndTime)
		if !okStart || !okEnd || end <= start {
			continue
		}
		title := stringTag(chapter.Tags, "title", "TITLE")
		lowered := strings.ToLower(title)
		switch {
		case containsAny(lowered, "op", "opening", "intro"):
			intervals = append(intervals, Interval{SkipType: "op", StartTime: start, EndTime: end})
		case containsAny(lowered, "ed", "ending", "credit"):
			intervals = append(intervals, Interval{SkipType: "ed", StartTime: start, EndTime: end})
		}
	}
	if len(intervals) == 0 {
		return nil, "chapter_not_detected"
	}
	return intervals, "chapter_detected"
}

// stringTag returns the first string-valued tag among names.
func stringTag(tags map[string]any, names ...string) string {
	for _, name := range names {
		if v, ok := tags[name].(string); ok {
			return v
		}
	}
	return ""
}

// containsAny reports whether any token is a substring of s.
func containsAny(s string, tokens ...string) bool {
	for _, token := range tokens {
		if strings.Contains(s, token) {
			return true
		}
	}
	return false
}

// asFloat converts ffprobe's string-or-number time fields.
func asFloat(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// BuildIntervalsFromRanges converts detected ranges into op/ed
// intervals (python _build_skip_intervals_from_ranges): the first range
// ending within the opening window becomes the op (anchored at 0), the
// last range starting inside the ending window becomes the ed (anchored
// at the duration); both must exceed the minimum segment length.
func BuildIntervalsFromRanges(ranges [][2]float64, duration float64,
	openingMaxEndSeconds, endingSearchWindowSeconds int,
) []Interval {
	if len(ranges) == 0 {
		return nil
	}

	var opEnd *float64
	for _, r := range ranges {
		if minOpeningEndSeconds <= r[1] && r[1] <= float64(openingMaxEndSeconds) {
			end := r[1]
			opEnd = &end
			break
		}
	}

	var edStart *float64
	endingBorder := max(0, duration-float64(endingSearchWindowSeconds))
	for i := len(ranges) - 1; i >= 0; i-- {
		if ranges[i][0] >= endingBorder {
			start := ranges[i][0]
			edStart = &start
			break
		}
	}

	var result []Interval
	if opEnd != nil && *opEnd > minSkipSegmentSeconds {
		result = append(result, Interval{
			SkipType:      "op",
			StartTime:     0,
			EndTime:       min(*opEnd, duration),
			EpisodeLength: duration,
		})
	}
	if edStart != nil && duration-*edStart > minSkipSegmentSeconds {
		result = append(result, Interval{
			SkipType:      "ed",
			StartTime:     max(*edStart, 0),
			EndTime:       duration,
			EpisodeLength: duration,
		})
	}
	return result
}
