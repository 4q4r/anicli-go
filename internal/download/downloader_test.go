package download

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestBuildCommandVideoOnly pins the minimal ffmpeg argv: single input,
// audio from stream 0, title metadata, .part temp output.
func TestBuildCommandVideoOnly(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Video:      contracts.VideoSource{URL: "https://cdn/v.m3u8"},
		OutputPath: "/dl/ep.mp4",
		Title:      "Эпизод",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", "https://cdn/v.m3u8",
		"-map", "0:v",
		"-map", "0:a",
		"-c", "copy",
		"-metadata", "title=Эпизод",
		"/dl/ep.part.mp4",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	if cmd.OutputPath != "/dl/ep.mp4" {
		t.Errorf("OutputPath = %q, want final path (temp applied at run)", cmd.OutputPath)
	}
	if cmd.TempPath != "/dl/ep.part.mp4" {
		t.Errorf("TempPath = %q, want extension-preserving temp", cmd.TempPath)
	}
}

// TestBuildCommandAudioAndChapters pins the multi-input layout: video,
// external audio and chapters inputs are numbered in order and
// -map_metadata points at the chapters index (python downloader.py
// input ordering).
func TestBuildCommandAudioAndChapters(t *testing.T) {
	t.Parallel()

	// The chapters file must exist: BuildCommand mirrors python's
	// Path(chapters_file).exists() guard.
	chapters := filepath.Join(t.TempDir(), "ch.ffmetadata")
	if err := os.WriteFile(chapters, []byte(";FFMETADATA1\n"), 0o600); err != nil {
		t.Fatalf("seed chapters: %v", err)
	}

	audio := contracts.VideoSource{URL: "https://cdn/a.m3u8"}
	cmd := BuildCommand(CommandInput{
		Video:        contracts.VideoSource{URL: "https://cdn/v.m3u8", Headers: map[string]string{"Referer": "https://r/"}},
		Audio:        &audio,
		ChaptersFile: chapters,
		OutputPath:   "/dl/ep.mp4",
		Title:        "T",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-headers", "Referer: https://r/\r\n",
		"-i", "https://cdn/v.m3u8",
		"-i", "https://cdn/a.m3u8",
		"-i", chapters,
		"-map_metadata", "2",
		"-map", "0:v",
		"-map", "1:a",
		"-c", "copy",
		"-metadata", "title=T",
		"/dl/ep.part.mp4",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestBuildCommandAudioSameURL pins: an audio source equal to the video
// URL is not added as a second input.
func TestBuildCommandAudioSameURL(t *testing.T) {
	t.Parallel()

	audio := contracts.VideoSource{URL: "https://cdn/v.m3u8"}
	cmd := BuildCommand(CommandInput{
		Video: contracts.VideoSource{URL: "https://cdn/v.m3u8"},
		Audio: &audio,
	})
	inputs := 0
	for i, arg := range cmd.Args {
		if arg == "-i" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "https://cdn/v.m3u8" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Errorf("video input count = %d, want 1 (equal audio skipped)", inputs)
	}
	// The audio map must reference stream 0 when no separate input exists.
	maps := 0
	for i, arg := range cmd.Args {
		if arg == "-map" && i+1 < len(cmd.Args) {
			maps++
			if maps == 2 && cmd.Args[i+1] != "0:a" {
				t.Errorf("audio map = %s, want 0:a when no separate input", cmd.Args[i+1])
			}
		}
	}
}

// TestDownloadMissingFFmpegFailsLoud pins the LookPath typed error.
func TestDownloadMissingFFmpegFailsLoud(t *testing.T) {
	t.Parallel()

	d := New(Options{FFmpeg: "/nonexistent/ffmpeg"})
	err := d.Download(context.Background(), CommandInput{
		Video:      contracts.VideoSource{URL: "x"},
		OutputPath: filepath.Join(t.TempDir(), "out.mp4"),
	})
	if err == nil {
		t.Fatal("Download with missing ffmpeg returned nil, want error")
	}
	var notFound *ErrBinaryNotFound
	if !errors.As(err, &notFound) || notFound.Bin != "/nonexistent/ffmpeg" {
		t.Fatalf("Download err = %v, want ErrBinaryNotFound(/nonexistent/ffmpeg)", err)
	}
}

// blockingRunner stubs ffmpeg execution for cancel/atomicity tests.
type blockingRunner struct {
	started  chan struct{}
	unblock  chan struct{}
	err      error
	lastArgs []string
}

func (b *blockingRunner) run(ctx context.Context, bin string, args []string) (string, error) {
	b.lastArgs = args
	close(b.started)
	select {
	case <-b.unblock:
		return "", b.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestDownloadCancelRemovesTemp pins: cancellation removes the .part
// file and leaves no final output.
func TestDownloadCancelRemovesTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	out := filepath.Join(dir, "ep.mp4")
	runner := &blockingRunner{started: make(chan struct{}), unblock: make(chan struct{})}
	d := New(Options{FFmpeg: "ffmpeg"})
	d.run = runner.run

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Download(ctx, CommandInput{Video: contracts.VideoSource{URL: "u"}, OutputPath: out})
	}()
	<-runner.started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Download err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download did not return on cancel")
	}

	if _, err := os.Stat(TempPathFor(out)); !os.IsNotExist(err) {
		t.Errorf("temp survived cancel: stat err = %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("final output appeared despite cancel: stat err = %v", err)
	}
}

// TestDownloadFailureRemovesTemp pins: a failing ffmpeg run removes the
// .part file and surfaces stderr.
func TestDownloadFailureRemovesTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	out := filepath.Join(dir, "ep.mp4")
	runner := &blockingRunner{started: make(chan struct{}), unblock: make(chan struct{}), err: errors.New("boom: bad stream")}
	d := New(Options{FFmpeg: "ffmpeg"})
	d.run = runner.run
	close(runner.unblock)

	err := d.Download(context.Background(), CommandInput{Video: contracts.VideoSource{URL: "u"}, OutputPath: out})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Download err = %v, want boom-wrapped error", err)
	}
	if _, statErr := os.Stat(TempPathFor(out)); !os.IsNotExist(statErr) {
		t.Errorf("temp survived failure: stat err = %v", statErr)
	}
}

// stubRenamer intercepts os.Rename for the success-path unit test.
func TestDownloadSuccessAtomicRename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	out := filepath.Join(dir, "ep.mp4")
	runner := &blockingRunner{started: make(chan struct{}), unblock: make(chan struct{})}
	d := New(Options{FFmpeg: "ffmpeg"})
	d.run = runner.run
	// Make the fake run "produce" the temp file before unblocking.
	go func() {
		<-runner.started
		if err := os.WriteFile(TempPathFor(out), []byte("data"), 0o600); err != nil {
			t.Errorf("seed part: %v", err)
		}
		close(runner.unblock)
	}()

	if err := d.Download(context.Background(), CommandInput{Video: contracts.VideoSource{URL: "u"}, OutputPath: out}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	data, err := os.ReadFile(out) //nolint:gosec // test reads its own temp file
	if err != nil || string(data) != "data" {
		t.Errorf("final output = %q err=%v, want renamed temp", data, err)
	}
	if _, err := os.Stat(TempPathFor(out)); !os.IsNotExist(err) {
		t.Errorf("temp survived success: stat err = %v", err)
	}
	// The temp path must have been the command's output argument.
	joined := strings.Join(runner.lastArgs, " ")
	if !strings.Contains(joined, TempPathFor(out)) {
		t.Errorf("ffmpeg wrote to %q, want the .part temp", joined)
	}
}

// TestDownloadRealFFmpeg is the optional end-to-end integration check
// (skipped under -short / without ffmpeg): a real copy with embedded
// chapters must land as the atomically renamed final file.
func TestDownloadRealFFmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	src := dir + "/src.mp4"
	out := dir + "/final.mp4"

	// 1s input with video+audio.
	gen := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10",
		"-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono",
		"-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", src}
	if outb, err := runCmd(context.Background(), "ffmpeg", gen); err != nil {
		t.Fatalf("generate source: %v: %s", err, outb)
	}

	chapters := dir + "/ch.ffmetadata"
	if err := os.WriteFile(chapters, []byte(";FFMETADATA1\n\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=500\ntitle=Opening\n\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=500\nEND=1000\ntitle=Ending\n\n"), 0o600); err != nil {
		t.Fatalf("chapters: %v", err)
	}

	d := New(Options{FFmpeg: "ffmpeg"})
	if err := d.Download(context.Background(), CommandInput{
		Video:        contracts.VideoSource{URL: src},
		ChaptersFile: chapters,
		OutputPath:   out,
		Title:        "Integration",
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if _, err := os.Stat(out); err != nil {
		t.Fatalf("final output missing: %v", err)
	}
	if _, err := os.Stat(TempPathFor(out)); !os.IsNotExist(err) {
		t.Errorf("temp survived: %v", err)
	}

	// Chapters must be embedded (NO skip lookup at playback, FEATURE E).
	probe, err := runCmd(context.Background(), "ffprobe",
		[]string{"-v", "error", "-show_chapters", "-print_format", "json", out})
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	if !strings.Contains(probe, `"Opening"`) {
		t.Errorf("chapters not embedded; probe=%s", probe)
	}
}

// runCmd runs a real binary with a timeout, returning combined output.
func runCmd(ctx context.Context, bin string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixed binary
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// countArgPairs counts "-flag value" occurrences in an argv.
func countArgPairs(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// TestHeaderArgsSingleArgument (169-A): ffmpeg honors only the LAST
// -headers argument — a multi-header map rendered as one pair per
// header silently dropped every header but the final one (live-verified
// against ffmpeg n9.0.2 on a local HTTP server: only Header2 arrived).
// The map must render ONE -headers pair carrying every header
// CRLF-joined inside a single value.
func TestHeaderArgsSingleArgument(t *testing.T) {
	t.Parallel()

	args := headerArgs(map[string]string{
		"Referer":    "https://ref.example/",
		"User-Agent": "anicli/1",
	})
	if got := countArgPairs(args, "-headers"); got != 1 {
		t.Fatalf("-headers pairs = %d (argv %v), want exactly one", got, args)
	}
	value := args[1]
	for _, want := range []string{"Referer: https://ref.example/\r\n", "User-Agent: anicli/1\r\n"} {
		if !strings.Contains(value, want) {
			t.Errorf("joined value %q missing %q", value, want)
		}
	}
}

// TestHeaderArgsBuildCommandSinglePair (169-A integration): the full
// download argv carries one -headers pair per input, however many
// headers the provider attached.
func TestHeaderArgsBuildCommandSinglePair(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Video: contracts.VideoSource{
			URL: "https://cdn/v.m3u8",
			Headers: map[string]string{
				"Referer":    "https://r/",
				"User-Agent": "ua",
				"Origin":     "https://o",
			},
		},
		OutputPath: "/t/out.mp4",
	})
	if got := countArgPairs(cmd.Args, "-headers"); got != 1 {
		t.Fatalf("video-input -headers pairs = %d (argv %v), want 1", got, cmd.Args)
	}
}

// TestHeaderArgsStripsCRLF (169-J): CR/LF inside a header key or value
// must never reach the ffmpeg argv — -headers lines are joined
// verbatim, so an embedded CRLF smuggles extra header lines (and, with
// a path-shaped payload, whole extra requests) into the HTTP exchange.
func TestHeaderArgsStripsCRLF(t *testing.T) {
	t.Parallel()

	args := headerArgs(map[string]string{
		"X-Injected":        "yes\r\nGET /smuggled HTTP/1.1\r\nHost: evil",
		"X-Key\r\nX-Forged": "v",
	})
	if len(args) != 2 {
		t.Fatalf("argv = %v, want one -headers pair", args)
	}
	value := args[1]
	// Every remaining CRLF is a pair terminator: splitting the joined
	// blob on CRLF yields exactly one line per header (never more) and
	// no line carries an interior break.
	lines := strings.Split(value, "\r\n")
	if len(lines) != 3 { // two header lines + the trailing-terminator empty
		t.Fatalf("rendered -headers value splits into %d lines (%q), want 2 header lines", len(lines), value)
	}
	for i, line := range lines[:len(lines)-1] {
		if line == "" {
			t.Fatalf("header line %d vanished (an interior break survived): %q", i, value)
		}
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("line %d carries an interior break: %q", i, line)
		}
	}
	if strings.Contains(value, "GET /smuggled\r\n") || strings.Contains(value, "X-Forged\r\n") {
		t.Fatalf("an injected line survived as its own header: %q", value)
	}
}
