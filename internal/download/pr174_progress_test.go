package download

// PR174 S4/S5: the finalize tolerance (a rename ENOENT with the
// final file already delivered by another writer is a SUCCESS — the
// file IS the deliverable; the single-flight guard makes the branch
// unreachable in-product, this is defense-in-depth) and the honest
// progress plumbing of the detailed pass: ffmpeg -progress pipe:1
// reporting out_time against the probed input duration.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeProgressRunner fabricates ffmpeg's -progress key=value lines
// and writes the temp file (the runner plays the child process).
type fakeProgressRunner struct {
	lines []string
}

func (f *fakeProgressRunner) run(ctx context.Context, bin string, args []string, onLine func(string)) error {
	// The output argument is the last one: prove the progress args
	// precede it and the output itself is the .part temp (TempPathFor
	// hides the suffix inside the base name).
	last := args[len(args)-1]
	if !strings.Contains(filepath.Base(last), TempSuffix) {
		return nil
	}
	if err := os.WriteFile(last, []byte("the merged output"), 0o640); err != nil {
		return err
	}
	for _, l := range f.lines {
		onLine(l)
	}
	return nil
}

// TestDownloadDetailedReportsProgress: the detailed pass pipes
// ffmpeg's progress and reports the output time against the probed
// total; the fraction and the raw time both ride the sample.
func TestDownloadDetailedReportsProgress(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_1_720p.mp4")
	d := New(Options{})
	fp := &fakeProgressRunner{lines: []string{
		"frame=10",
		"out_time_ms=60000000", // ffmpeg reports MICROseconds here
		"out_time=00:01:00.000000",
		"progress=continue",
		"out_time=00:02:00.000000",
		"progress=end",
	}}
	d.runProgress = fp.run
	d.probeDuration = func(context.Context, contracts.VideoSource) (float64, error) {
		return 240, nil // 4 minutes total
	}
	var samples []Progress
	err := d.DownloadDetailed(context.Background(), CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	}, func(p Progress) {
		samples = append(samples, p)
	})
	if err != nil {
		t.Fatalf("DownloadDetailed: %v", err)
	}
	if len(samples) < 2 {
		t.Fatalf("the progress samples must flow, got %d: %+v", len(samples), samples)
	}
	if samples[0].TotalSec != 240 {
		t.Fatalf("the probed total must ride the sample, got %+v", samples[0])
	}
	// out_time=00:01:00 → 60s of 240 → 0.25.
	if samples[0].TimeSec != 60 {
		t.Fatalf("the first sample must read 60s (out_time), got %+v", samples[0])
	}
	if samples[0].Fraction() < 0.24 || samples[0].Fraction() > 0.26 {
		t.Fatalf("the fraction must be 0.25, got %+v", samples[0])
	}
	// The probe cost rides the sample: the caller can report it.
	if samples[len(samples)-1].TimeSec != 120 {
		t.Fatalf("the last sample must read 120s, got %+v", samples[len(samples)-1])
	}
}

// TestDownloadDetailedUnknownDurationStaysIndeterminate: a failed
// duration probe keeps the total at 0 — the bar stays honest
// (indeterminate), never a fabricated fraction.
func TestDownloadDetailedUnknownDurationStaysIndeterminate(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_1_720p.mp4")
	d := New(Options{})
	fp := &fakeProgressRunner{lines: []string{"out_time=00:01:00.000000", "progress=end"}}
	d.runProgress = fp.run
	d.probeDuration = func(context.Context, contracts.VideoSource) (float64, error) {
		return 0, errors.New("probe refused")
	}
	var samples []Progress
	if err := d.DownloadDetailed(context.Background(), CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	}, func(p Progress) { samples = append(samples, p) }); err != nil {
		t.Fatalf("DownloadDetailed: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("the samples must flow even without a total")
	}
	for _, s := range samples {
		if s.TotalSec != 0 || s.Fraction() != 0 {
			t.Fatalf("an unknown total must keep the fraction at 0 (indeterminate), got %+v", s)
		}
		if s.TimeSec != 60 {
			t.Fatalf("the raw output time must still ride the sample, got %+v", s)
		}
	}
}

// TestDownloadFinalizeTolerantWhenFinalExists (PR174 S4): the rename
// hits ENOENT while the final file already exists with a nonzero
// size — another writer delivered the same episode first. The file
// IS the deliverable: the episode settles as a success with a
// warning, never a spurious «download: finalize» on a healthy file.
func TestDownloadFinalizeTolerantWhenFinalExists(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_14_720p.mp4")
	if err := os.WriteFile(out, []byte("a full healthy episode file, really"), 0o640); err != nil {
		t.Fatal(err)
	}
	d := New(Options{})
	// The runner "succeeds" but never creates the temp file — the
	// rename finds no .part (the S4 live signature).
	d.run = func(ctx context.Context, bin string, args []string) (string, error) {
		return "", nil
	}
	if err := d.Download(context.Background(), CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	}); err != nil {
		t.Fatalf("a delivered final must settle as success, got %v", err)
	}
}

// TestDownloadFinalizeStillFailsWithoutFinal: the ENOENT tolerance
// never masks a real failure — no final file, no success.
func TestDownloadFinalizeStillFailsWithoutFinal(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_14_720p.mp4")
	d := New(Options{})
	d.run = func(ctx context.Context, bin string, args []string) (string, error) {
		return "", nil // no temp written either
	}
	err := d.Download(context.Background(), CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	})
	if err == nil {
		t.Fatal("a missing final must still fail the download")
	}
}

// TestDownloadFinalizeFailsOnEmptyFinal: a zero-size final is not a
// deliverable — the tolerance must not bless it.
func TestDownloadFinalizeFailsOnEmptyFinal(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_14_720p.mp4")
	if err := os.WriteFile(out, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	d := New(Options{})
	d.run = func(ctx context.Context, bin string, args []string) (string, error) {
		return "", nil
	}
	if err := d.Download(context.Background(), CommandInput{
		Video:      testVideoSource("v1"),
		OutputPath: out,
	}); err == nil {
		t.Fatal("a zero-size final must not settle as success")
	}
}

// testVideoSource builds a plain source for the fixture tests.
func testVideoSource(url string) contracts.VideoSource {
	return contracts.VideoSource{URL: "https://cdn/" + url + ".m3u8"}
}
