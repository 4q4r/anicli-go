package download

// PR174 fix-round pins: the honest duration probe (ffprobe prints to
// STDOUT — the stderr-capturing seam read an empty pipe and every
// real probe came back unparsable) and the cancel-after-success
// contract of the progress runner (a cancellation landing after the
// child exited successfully must never discard a COMPLETED pass).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeProbeOutRunner scripts the stdout-capturing seam for the probe:
// the duration payload arrives on stdout, stderr stays empty (the
// real ffprobe contract). argv is recorded for the shape pin.
type fakeProbeOutRunner struct {
	bin    string
	args   []string
	stdout string
	stderr string
	err    error
	calls  int
}

func (f *fakeProbeOutRunner) runOut(ctx context.Context, bin string, args []string) ([]byte, string, error) {
	f.calls++
	f.bin, f.args = bin, args
	return []byte(f.stdout), f.stderr, f.err
}

// TestRealProbeDurationReadsStdout (fix-round CR-1): ffprobe prints
// the format duration to STDOUT. The probe must read the stdout
// payload through the stdout-capturing seam — argv: -v error,
// -show_entries format=duration, the single -headers pair, then the
// source URL last.
func TestRealProbeDurationReadsStdout(t *testing.T) {
	d := New(Options{})
	fakeOut := &fakeProbeOutRunner{stdout: "62.500000\n"}
	d.runOut = fakeOut.runOut
	// The stderr-capturing seam: the probe must NOT read it (a real
	// ffprobe prints nothing to stderr on success — the payload is on
	// stdout). Scripted empty-and-success so the wrong seam yields
	// the honest unparsable failure instead of a network probe.
	d.run = func(context.Context, string, []string) (string, error) {
		return "", nil
	}
	src := contracts.VideoSource{
		URL:     "https://cdn/v1.m3u8",
		Headers: map[string]string{"Referer": "https://x/"},
	}
	secs, err := d.realProbeDuration(context.Background(), src)
	if err != nil {
		t.Fatalf("realProbeDuration: %v", err)
	}
	if secs != 62.5 {
		t.Fatalf("the stdout payload must parse, got %v", secs)
	}
	joined := strings.Join(fakeOut.args, " ")
	for _, want := range []string{
		"-v error",
		"-show_entries format=duration",
		"-of default=noprint_wrappers=1:nokey=1",
		"-headers",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("probe argv must contain %q, got %v", want, fakeOut.args)
		}
	}
	if fakeOut.args[len(fakeOut.args)-1] != src.URL {
		t.Fatalf("the source URL must close the argv, got %v", fakeOut.args)
	}
}

// TestRealProbeDurationStillTypesFailures: a failed probe run keeps
// the typed wrap; an unparsable stdout payload is the honest error,
// never a zero total masquerading as an unknown duration.
func TestRealProbeDurationStillTypesFailures(t *testing.T) {
	d := New(Options{})
	d.runOut = func(context.Context, string, []string) ([]byte, string, error) {
		return nil, "boom", errors.New("probe refused")
	}
	if _, err := d.realProbeDuration(context.Background(), testVideoSource("v1")); err == nil {
		t.Fatal("a refused probe run must fail the duration measurement")
	}
	d.runOut = func(context.Context, string, []string) ([]byte, string, error) {
		return []byte("\n"), "", nil // stdout empty: unparsable
	}
	if _, err := d.realProbeDuration(context.Background(), testVideoSource("v1")); err == nil {
		t.Fatal("an unparsable stdout payload must fail the duration measurement")
	}
}

// fakeFfmpegScript writes a stand-in ffmpeg executable: it creates the
// output argument (the last one — the .part temp), prints its
// -progress lines, and exits with the given status.
func fakeFfmpegScript(t *testing.T, dir, lines string, exitStatus int) string {
	t.Helper()
	script := filepath.Join(dir, "ffmpeg-fake")
	body := "#!/bin/sh\nfor last do :; done\n: > \"$last\"\nprintf '" + lines + "'\nexit " + itoa(exitStatus) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // the stub ffmpeg must be executable; lives in t.TempDir()
		t.Fatal(err)
	}
	return script
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestRunProgressCancelAfterChildSuccessKeepsFile (fix-round HIGH-2):
// the cancel lands while the scanner is still inside its drain loop —
// the child has already written everything and exited 0. The
// COMPLETED pass must not turn into an error: the file survives and
// DownloadDetailed settles clean (Go's exec injects ctx.Err() into
// Wait for a kill on the unreaped zombie — the runner must read the
// child's real exit status, not the context state).
func TestRunProgressCancelAfterChildSuccessKeepsFile(t *testing.T) {
	dir := t.TempDir()
	bin := fakeFfmpegScript(t, dir, "out_time=00:00:05.000000\\nprogress=end\\n", 0)
	out := filepath.Join(dir, "EP_1_720p.mp4")
	d := New(Options{FFmpeg: bin})
	d.probeDuration = func(context.Context, contracts.VideoSource) (float64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	samples := 0
	err := d.DownloadDetailed(ctx, CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	}, func(Progress) {
		samples++
		// The sample fires from the scanner's drain loop, AFTER the
		// child wrote all of its output: give it time to exit, then
		// cancel on a SUCCESSFULLY completed child.
		time.Sleep(300 * time.Millisecond)
		cancel()
	})
	if err != nil {
		t.Fatalf("a cancel landing after a successful child exit must not fail the download: %v", err)
	}
	if samples != 1 {
		t.Fatalf("the progress sample must flow, got %d", samples)
	}
	if _, serr := os.Stat(out); serr != nil {
		t.Fatalf("the finished file must survive the late cancel: %v", serr)
	}
}

// TestRunProgressCancelDuringRunStillFails: the same runner must keep
// the honest cancellation contract while the child is ALIVE — the
// context kills ffmpeg and the pass is typed as cancelled.
func TestRunProgressCancelDuringRunStillFails(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg-blocked")
	// exec: the sleep REPLACES the shell — the kill lands on the
	// process holding the pipe (a forked grandchild would keep the
	// pipe open past the kill and hang the drain loop).
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil { //nolint:gosec // the stub ffmpeg must be executable; lives in t.TempDir()
		t.Fatal(err)
	}
	out := filepath.Join(dir, "EP_1_720p.mp4")
	d := New(Options{FFmpeg: script})
	d.probeDuration = func(context.Context, contracts.VideoSource) (float64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		res <- d.DownloadDetailed(ctx, CommandInput{
			Video:      testVideoSource("v1"),
			OutputPath: out,
		}, func(Progress) {})
	}()
	time.Sleep(150 * time.Millisecond) // the child is alive inside sleep
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancel while ffmpeg runs must type the pass as cancelled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled download must return")
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("a cancelled pass must leave no final file")
	}
}
