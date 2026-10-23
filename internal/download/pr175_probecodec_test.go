package download

// PR175 pins: ProbeCodec reads the ffprobe JSON payload from STDOUT
// (the stderr-seam class PR27 found in realProbeDuration — ffprobe
// prints its -of json output to stdout; the stderr-capturing run seam
// read an empty pipe and every real probe came back "", silently
// degrading the container decision to the safe-Matroska fallback).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// errProbeRefused is the test sentinel pinning the wrap contract:
// ProbeCodec must wrap the runOut failure with %w so errors.Is reaches
// the underlying cause. A regression to %v (or an unrelated error)
// fails the assertion.
var errProbeRefused = errors.New("probe refused")

// TestProbeCodecReadsStdout: ffprobe prints the codec JSON to STDOUT.
// The probe must read the payload through the stdout-capturing seam —
// argv: -v error, -select_streams <kind>:0, -show_entries
// stream=codec_name, -of json, the single -headers pair, then the
// source URL last.
func TestProbeCodecReadsStdout(t *testing.T) {
	d := New(Options{})
	fakeOut := &fakeProbeOutRunner{stdout: `{"streams":[{"codec_name":"hevc"}]}`}
	d.runOut = fakeOut.runOut
	// The stderr-capturing seam: the probe must NOT read it (a real
	// ffprobe prints nothing to stderr on success — the payload is on
	// stdout). Scripted empty-and-success so the wrong seam yields the
	// silent "" degradation instead of a network probe.
	d.run = func(context.Context, string, []string) (string, error) {
		return "", nil
	}
	src := contracts.VideoSource{
		URL:     "https://cdn/v1.m3u8",
		Headers: map[string]string{"Referer": "https://x/"},
	}
	codec, err := d.ProbeCodec(context.Background(), "v", src)
	if err != nil {
		t.Fatalf("ProbeCodec: %v", err)
	}
	if codec != "hevc" {
		t.Fatalf("the stdout payload must parse, got codec %q", codec)
	}
	joined := strings.Join(fakeOut.args, " ")
	for _, want := range []string{
		"-v error",
		"-select_streams v:0",
		"-show_entries stream=codec_name",
		"-of json",
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

// TestProbeCodecStillTypesFailures: a failed probe run keeps the typed
// %w wrap (errors.Is reaches the sentinel — a %v wrap or an unrelated
// error fails); an empty stdout payload degrades to "" WITHOUT an error
// — the callers' fallback contract (ContainerFor's safe Matroska) must
// not change.
func TestProbeCodecStillTypesFailures(t *testing.T) {
	d := New(Options{})
	d.runOut = func(context.Context, string, []string) ([]byte, string, error) {
		return nil, "boom", errProbeRefused
	}
	if _, err := d.ProbeCodec(context.Background(), "a", testVideoSource("v1")); !errors.Is(err, errProbeRefused) {
		t.Fatalf("the failed probe must wrap the run error with %%w (errors.Is sentinel), got: %v", err)
	}
	d.runOut = func(context.Context, string, []string) ([]byte, string, error) {
		return []byte(""), "", nil // stdout empty: the degradation contract
	}
	codec, err := d.ProbeCodec(context.Background(), "a", testVideoSource("v1"))
	if err != nil {
		t.Fatalf("an empty stdout payload must degrade, not error: %v", err)
	}
	if codec != "" {
		t.Fatalf("an empty stdout payload degrades to the empty codec, got %q", codec)
	}
}

// TestProbeCodecWarnsOnEmptyPayload pins the PR28 diagnostics seam: a
// successful probe run with an empty or unparsable stdout payload WARNs
// through the Downloader's sink (the probeDuration warn pattern,
// downloader.go:335) naming the probed source — the failure typing
// stays "" without an error.
func TestProbeCodecWarnsOnEmptyPayload(t *testing.T) {
	d := New(Options{})
	d.runOut = func(context.Context, string, []string) ([]byte, string, error) {
		return []byte(""), "", nil
	}
	var buf bytes.Buffer
	d.log = slog.New(slog.NewTextHandler(&buf, nil))
	src := testVideoSource("v1")
	codec, err := d.ProbeCodec(context.Background(), "v", src)
	if err != nil {
		t.Fatalf("an empty payload must degrade, not error: %v", err)
	}
	if codec != "" {
		t.Fatalf("an empty payload degrades to the empty codec, got %q", codec)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") {
		t.Fatalf("an empty/unparsable probe payload must WARN through the diagnostics sink, got: %q", out)
	}
	if !strings.Contains(out, src.URL) {
		t.Fatalf("the WARN must name the probed source %q, got: %q", src.URL, out)
	}
}
