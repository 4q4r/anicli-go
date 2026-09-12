package skip

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// goldenSilenceStderr models real ffmpeg silencedetect stderr output.
const goldenSilenceStderr = `[silencedetect @ 0x55f] silence_start: 0.523
[silencedetect @ 0x55f] silence_end: 90.212 | silence_duration: 89.689
[silencedetect @ 0x55f] silence_start: 1299.8
[silencedetect @ 0x55f] silence_end: 1400.01 | silence_duration: 100.21
`

// goldenBlackStderr models real ffmpeg blackdetect stderr output.
const goldenBlackStderr = `[blackdetect @ 0x55e] black_start:0 black_end:88.5 black_pixcnt:...'
[blackdetect @ 0x55e] black_start:1295 black_end:1399.9 black_pixcnt:...
`

func TestParseDetectRangesSilence(t *testing.T) {
	t.Parallel()

	got := parseDetectRanges(goldenSilenceStderr, silenceStartPattern, silenceEndPattern)
	want := [][2]float64{{0.523, 90.212}, {1299.8, 1400.01}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}

func TestParseDetectRangesBlack(t *testing.T) {
	t.Parallel()

	got := parseDetectRanges(goldenBlackStderr, blackStartPattern, blackEndPattern)
	want := [][2]float64{{0, 88.5}, {1295, 1399.9}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}

// TestParseDetectRangesTrailingStartWithoutEnd pins the pairing walk
// (python semantics): ends at or before a start are skipped, and a
// start with no remaining end is dropped.
func TestParseDetectRangesTrailingStartWithoutEnd(t *testing.T) {
	t.Parallel()

	// ends[0]=5 consumed by start 1; start 10 has no end left.
	got := parseDetectRanges("silence_start: 1\nsilence_start: 10\nsilence_end: 5\n", silenceStartPattern, silenceEndPattern)
	want := [][2]float64{{1, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}

	// The end at/before the start is skipped, the later one pairs.
	got = parseDetectRanges("silence_start: 10\nsilence_end: 5\nsilence_end: 25\n", silenceStartPattern, silenceEndPattern)
	want = [][2]float64{{10, 25}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}

func TestParseDetectRangesEmpty(t *testing.T) {
	t.Parallel()

	if got := parseDetectRanges("no markers here", silenceStartPattern, silenceEndPattern); len(got) != 0 {
		t.Errorf("ranges = %v, want empty", got)
	}
}

// TestParseChaptersJSON pins ffprobe chapter extraction: title
// classification, numeric-string tolerance and invalid-row dropping.
func TestParseChaptersJSON(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"chapters":[
		{"start_time":"0.000","end_time":"90.000","tags":{"title":"Opening"}},
		{"start_time":1300.5,"end_time":1400.0,"tags":{"title":"End Credits"}},
		{"start_time":10,"end_time":10,"tags":{"title":"Opening"}},
		{"start_time":0,"end_time":5},
		"garbage"
	]}`)

	intervals, detail := parseChaptersJSON(payload)
	if detail != "chapter_detected" {
		t.Fatalf("detail = %q, want chapter_detected", detail)
	}
	want := []Interval{
		{SkipType: "op", StartTime: 0, EndTime: 90},
		{SkipType: "ed", StartTime: 1300.5, EndTime: 1400},
	}
	if !reflect.DeepEqual(intervals, want) {
		t.Errorf("intervals = %+v, want %+v", intervals, want)
	}
}

func TestParseChaptersJSONFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		detail  string
	}{
		{"malformed json", `{`, "chapter_payload_invalid"},
		{"missing chapters key", `{"format":{}}`, "chapter_missing"},
		{"no titled chapters", `{"chapters":[{"start_time":0,"end_time":5,"tags":{"title":"Part 1"}}]}`, "chapter_not_detected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			intervals, detail := parseChaptersJSON([]byte(tc.payload))
			if detail != tc.detail {
				t.Errorf("detail = %q, want %q", detail, tc.detail)
			}
			if len(intervals) != 0 {
				t.Errorf("intervals = %+v, want empty", intervals)
			}
		})
	}
}

// TestBuildIntervalsFromRanges pins the op/ed inference table.
func TestBuildIntervalsFromRanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		ranges   [][2]float64
		duration float64
		opMaxEnd int
		edWindow int
		want     []Interval
	}{
		{
			name:     "op and ed from silence",
			ranges:   [][2]float64{{0.5, 90}, {1299, 1400}},
			duration: 1440,
			opMaxEnd: 180,
			edWindow: 300,
			want: []Interval{
				{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440},
				{SkipType: "ed", StartTime: 1299, EndTime: 1440, EpisodeLength: 1440},
			},
		},
		{
			name:     "op beyond window ignored",
			ranges:   [][2]float64{{0, 200}},
			duration: 1440,
			opMaxEnd: 180,
			edWindow: 300,
			want:     nil,
		},
		{
			name:     "op shorter than minimum segment ignored",
			ranges:   [][2]float64{{0, 25}},
			duration: 1440,
			opMaxEnd: 180,
			edWindow: 300,
			want:     nil,
		},
		{
			name:     "ed shorter than minimum segment ignored",
			ranges:   [][2]float64{{1435, 1439}},
			duration: 1440,
			opMaxEnd: 180,
			edWindow: 300,
			want:     nil,
		},
		{
			name:     "empty ranges",
			ranges:   nil,
			duration: 1440,
			opMaxEnd: 180,
			edWindow: 300,
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BuildIntervalsFromRanges(tc.ranges, tc.duration, tc.opMaxEnd, tc.edWindow)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("intervals = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestIntroSkipperCommandArgs pins the exact ffmpeg/ffprobe argv shapes.
func TestIntroSkipperCommandArgs(t *testing.T) {
	t.Parallel()

	opts := IntroSkipperOptions{
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe",
		SilenceNoiseDB: -35, SilenceMinDuration: 2.5,
	}

	if got := ffprobeDurationArgs("media.mp4"); !reflect.DeepEqual(got, []string{
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", "media.mp4",
	}) {
		t.Errorf("duration args = %v", got)
	}

	if got := ffprobeChaptersArgs("media.mp4"); !reflect.DeepEqual(got, []string{
		"-v", "error", "-show_chapters", "-print_format", "json", "media.mp4",
	}) {
		t.Errorf("chapters args = %v", got)
	}

	if got := ffmpegSilenceArgs("media.mp4", opts); !reflect.DeepEqual(got, []string{
		"-hide_banner", "-i", "media.mp4",
		"-af", "silencedetect=noise=-35dB:d=2.5",
		"-f", "null", "-",
	}) {
		t.Errorf("silence args = %v", got)
	}

	if got := ffmpegBlackArgs("media.mp4"); !reflect.DeepEqual(got, []string{
		"-hide_banner", "-i", "media.mp4",
		"-vf", "blackdetect=d=1.0:pix_th=0.10",
		"-an", "-f", "null", "-",
	}) {
		t.Errorf("black args = %v", got)
	}
}

// fakeRunner stubs command execution for flow tests.
type fakeRunner struct {
	calls []string
	// respond maps a marker substring of the args to (stdout, stderr,
	// err); unmatched calls return an error.
	respond map[string]string
}

func (f *fakeRunner) run(_ context.Context, bin string, args []string) (string, string, error) {
	joined := bin + " " + strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for marker, out := range f.respond {
		if strings.Contains(joined, marker) {
			// Payload on both streams: probes read stdout, detectors
			// read stderr.
			return out, out, nil
		}
	}
	return "", "", errFakeRunnerNoResponse
}

var errFakeRunnerNoResponse = &fakeRunnerError{}

type fakeRunnerError struct{}

func (*fakeRunnerError) Error() string { return "fake runner: no response configured" }

// newFakeIntroSkipper builds a client whose commands are stubbed.
func newFakeIntroSkipper(respond map[string]string) (*IntroSkipperClient, *fakeRunner) {
	runner := &fakeRunner{respond: respond}
	opts := DefaultIntroSkipperOptions()
	c := NewIntroSkipperClient(opts)
	c.run = runner.run
	return c, runner
}

// TestIntroSkipperRemoteURLGuard pins the transcript fix: ffprobe needs
// a local file, remote URLs are refused before any subprocess runs.
func TestIntroSkipperRemoteURLGuard(t *testing.T) {
	t.Parallel()

	c, runner := newFakeIntroSkipper(nil)

	_, details, err := c.GetSkipTimes(context.Background(), "https://cdn.example/ep.m3u8")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if details != "remote_url_unsupported" {
		t.Errorf("details = %q, want remote_url_unsupported", details)
	}
	if len(runner.calls) != 0 {
		t.Errorf("subprocess calls = %v, want none", runner.calls)
	}
}

// TestIntroSkipperDisabled pins the enabled guard short-circuits before
// any probe.
func TestIntroSkipperDisabled(t *testing.T) {
	t.Parallel()

	c := NewIntroSkipperClient(IntroSkipperOptions{Enabled: false})
	c.run = func(context.Context, string, []string) (string, string, error) {
		t.Error("disabled client must not run subprocesses")
		return "", "", nil
	}

	_, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if details != "intro_skipper_disabled" {
		t.Errorf("details = %q, want intro_skipper_disabled", details)
	}
}

// TestIntroSkipperFlowChaptersFirst pins the detection ladder: embedded
// chapters win over signal analysis.
func TestIntroSkipperFlowChaptersFirst(t *testing.T) {
	t.Parallel()

	c, runner := newFakeIntroSkipper(map[string]string{
		"format=duration": "1440.0\n",
		"-show_chapters":  `{"chapters":[{"start_time":0,"end_time":90,"tags":{"title":"Opening"}}]}`,
	})

	intervals, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 1 || intervals[0].SkipType != "op" {
		t.Fatalf("intervals = %+v, want one op", intervals)
	}
	if details != "chapter_detected:op" {
		t.Errorf("details = %q, want chapter_detected:op", details)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "silencedetect") {
			t.Errorf("silence probe ran despite chapter hit: %s", call)
		}
	}
}

// TestIntroSkipperFlowSilenceFallback pins: no usable chapters ->
// silencedetect ranges build op+ed.
func TestIntroSkipperFlowSilenceFallback(t *testing.T) {
	t.Parallel()

	c, _ := newFakeIntroSkipper(map[string]string{
		"format=duration": "1440.0\n",
		"-show_chapters":  `{"chapters":[]}`,
		"silencedetect":   goldenSilenceStderr,
	})

	intervals, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 2 {
		t.Fatalf("intervals = %+v, want op+ed", intervals)
	}
	if intervals[0].SkipType != "op" || intervals[1].SkipType != "ed" {
		t.Errorf("types = %s/%s, want op/ed", intervals[0].SkipType, intervals[1].SkipType)
	}
	if details != "silence_detected:ed,op" {
		t.Errorf("details = %q, want silence_detected:ed,op", details)
	}
}

// TestIntroSkipperFlowBlackdetectFallback pins: no chapters, no usable
// silence -> blackdetect.
func TestIntroSkipperFlowBlackdetectFallback(t *testing.T) {
	t.Parallel()

	c, runner := newFakeIntroSkipper(map[string]string{
		"format=duration": "1440.0\n",
		"-show_chapters":  `{"chapters":[]}`,
		"silencedetect":   "nothing detected",
		"blackdetect":     goldenBlackStderr,
	})

	intervals, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 2 {
		t.Fatalf("intervals = %+v, want op+ed", intervals)
	}
	if details != "blackdetect_detected:ed,op" {
		t.Errorf("details = %q, want blackdetect_detected:ed,op", details)
	}
	// All three probes ran.
	for _, marker := range []string{"format=duration", "-show_chapters", "silencedetect", "blackdetect"} {
		found := false
		for _, call := range runner.calls {
			if strings.Contains(call, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("probe %q never ran; calls = %v", marker, runner.calls)
		}
	}
}

// TestIntroSkipperFlowNothingDetected pins the terminal miss detail.
func TestIntroSkipperFlowNothingDetected(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("fpcalc"); err == nil {
		t.Skip("fpcalc present: chromaprint branch applies")
	}

	c, _ := newFakeIntroSkipper(map[string]string{
		"format=duration": "1440.0\n",
		"-show_chapters":  `{"chapters":[]}`,
		"silencedetect":   "nothing",
		"blackdetect":     "nothing",
	})

	intervals, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 0 {
		t.Errorf("intervals = %+v, want empty", intervals)
	}
	if details != "no_intro_or_ending_detected" {
		t.Errorf("details = %q, want no_intro_or_ending_detected", details)
	}
}

// TestIntroSkipperFlowDurationProbeFailed pins the missing-duration
// bail-out.
func TestIntroSkipperFlowDurationProbeFailed(t *testing.T) {
	t.Parallel()

	c, runner := newFakeIntroSkipper(map[string]string{
		"format=duration": "",
	})

	_, details, err := c.GetSkipTimes(context.Background(), "ep.mp4")
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if details != "duration_probe_failed" {
		t.Errorf("details = %q, want duration_probe_failed", details)
	}
	if len(runner.calls) != 1 {
		t.Errorf("calls = %v, want only the duration probe", runner.calls)
	}
}

// TestIntroSkipperRealFFmpegChapters is the optional integration check
// (skipped under -short and when the binaries are absent): it muxes a
// real FFMETADATA file into a real media file and requires the chapter
// detector to recover the Opening/Ending marks end to end.
func TestIntroSkipperRealFFmpegChapters(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	wav := dir + "/audio.wav"
	out := dir + "/episode.mp4"

	// 2s of silence.
	gen := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "2", wav}
	if outb, err := runExternal(context.Background(), ffmpeg, gen); err != nil {
		t.Fatalf("generate wav: %v: %s", err, outb)
	}

	// Chapters with latin titles: the detector's token set is
	// op/opening/intro + ed/ending/credit (ported verbatim). Note the
	// round-trip gap replicated from python: our own FFMETADATA writer
	// emits RU titles («Опенинг»), which this detector cannot read back;
	// reported as a follow-up, not fixed here (parity first).
	content := strings.Join([]string{
		";FFMETADATA1", "",
		"[CHAPTER]", "TIMEBASE=1/1000", "START=0", "END=1000", "title=Opening", "",
		"[CHAPTER]", "TIMEBASE=1/1000", "START=1000", "END=2000", "title=Ending", "",
	}, "\n")
	metaPath := dir + "/meta.ffmetadata"
	if err := os.WriteFile(metaPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write chapters: %v", err)
	}

	mux := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-i", wav, "-i", metaPath, "-map_metadata", "1", "-c", "copy", out}
	if outb, err := runExternal(context.Background(), ffmpeg, mux); err != nil {
		t.Fatalf("mux chapters: %v: %s", err, outb)
	}

	c := NewIntroSkipperClient(IntroSkipperOptions{
		Enabled:                   true,
		OpeningMaxEndSeconds:      5,
		EndingSearchWindowSeconds: 5,
	})
	intervals, details, err := c.GetSkipTimes(context.Background(), out)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if details != "chapter_detected:ed,op" {
		t.Fatalf("details = %q, want chapter_detected:ed,op", details)
	}
	if len(intervals) != 2 || intervals[0].SkipType != "op" || intervals[1].SkipType != "ed" {
		t.Fatalf("intervals = %+v, want op+ed", intervals)
	}
	if intervals[0].EndTime != 1 || intervals[1].StartTime != 1 {
		t.Errorf("chapter times drifted: %+v", intervals)
	}
}

// runExternal runs a real binary, returning combined output.
func runExternal(ctx context.Context, bin string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixed binary
	outb, err := cmd.CombinedOutput()
	return string(outb), err
}
