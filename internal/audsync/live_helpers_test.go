//go:build live

package audsync

// PR165b live fixtures: real ffmpeg-synthesized media (white-noise
// voices — broadband, so the prominence gate fires honestly). Shared
// by the closed-loop sign proof and the discontinuous-HLS decode
// verification.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// srcURL wraps a local media path as a VideoSource (the analysis legs
// are URL-driven; local files work through the same argv).
func srcURL(path string) contracts.VideoSource {
	return contracts.VideoSource{URL: path}
}

// liveBin skips the test when ffmpeg is missing.
func liveBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("live test: ffmpeg not on PATH")
	}
	return bin
}

// runFF runs one fixture-building command (60 s cap).
func runFF(t *testing.T, bin string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixture command, bin from LookPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %s", args, out)
	}
}

// liveRefFixture builds a continuous "episode" (tiny video + one
// white-noise voice, seed 42, `dur` seconds) and a donor lagging by
// exactly 3 s (3 s silence + the same voice). Both are mp4 (video
// mpeg4, audio aac) — the codec round-trip the analysis must survive.
func liveRefFixture(t *testing.T, bin, dir string, dur int) (refPath, donorPath string) {
	t.Helper()
	refPath = filepath.Join(dir, "ref.mp4")
	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=1:duration="+strconv.Itoa(dur),
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d="+strconv.Itoa(dur),
		"-c:v", "mpeg4", "-q:v", "8", "-c:a", "aac", "-b:a", "96k",
		"-ar", "48000", "-ac", "2",
		refPath)

	donorPath = filepath.Join(dir, "donor3s.mp4")
	runFF(t, bin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=1:duration="+strconv.Itoa(dur+3),
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d="+strconv.Itoa(dur)+",adelay=3000:all=1",
		"-t", strconv.Itoa(dur+3),
		"-c:v", "mpeg4", "-q:v", "8", "-c:a", "aac", "-b:a", "96k",
		"-ar", "48000", "-ac", "2",
		donorPath)
	return refPath, donorPath
}
