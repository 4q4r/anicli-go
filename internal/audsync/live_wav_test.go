package audsync

// PR165b T5 live proof: the REAL ffmpeg WAV header (LIST/INFO before
// data — the shape the hand-built fixtures approximate) parses, and a
// ±1 s shift round-trips with exact sizes. Requires ffmpeg on PATH.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// liveFFmpeg skips the test when ffmpeg is unavailable.
func liveFFmpeg(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("live test: ffmpeg not on PATH")
	}
	return bin
}

func TestLiveWAVLayoutRealFFmpegAndShift(t *testing.T) {
	bin := liveFFmpeg(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "real.wav")

	// 10 s of 48 kHz stereo s16le with the metadata tag — the header
	// carries LIST/INFO exactly like the pipeline decode produces.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, //nolint:gosec // G204: the test-only fixture command, bin from LookPath
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=10",
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le",
		"-metadata", "encoder=pr165b-live", "-f", "wav", srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture: %v: %s", err, out)
	}

	srcFile, err := os.Open(srcPath) //nolint:gosec // G304: the test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	srcLayout, err := readWAVLayout(srcFile)
	_ = srcFile.Close()
	if err != nil {
		t.Fatalf("real ffmpeg wav did not parse: %v", err)
	}
	if srcLayout.BlockAlign != 4 || srcLayout.Frames != 480000 {
		t.Fatalf("layout = %+v, want blockAlign 4, 480000 frames", srcLayout)
	}
	t.Logf("real ffmpeg DataOff = %d (44 would mean no LIST/INFO)", srcLayout.DataOff)

	// −1 s pad: 48000 zero frames on the head.
	padPath := filepath.Join(dir, "pad.wav")
	if err := ShiftWAVFile(srcPath, padPath, -48000); err != nil {
		t.Fatalf("pad shift: %v", err)
	}
	padFile, err := os.Open(padPath) //nolint:gosec // G304: the test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	padLayout, err := readWAVLayout(padFile)
	_ = padFile.Close()
	if err != nil {
		t.Fatalf("shifted wav did not parse: %v", err)
	}
	if padLayout.Frames != srcLayout.Frames+48000 || padLayout.DataSize != srcLayout.DataSize+48000*4 {
		t.Fatalf("pad layout = %+v (src %+v)", padLayout, srcLayout)
	}

	// +1 s trim: the payload loses exactly 48000 frames.
	trimPath := filepath.Join(dir, "trim.wav")
	if err := ShiftWAVFile(srcPath, trimPath, 48000); err != nil {
		t.Fatalf("trim shift: %v", err)
	}
	trimFile, err := os.Open(trimPath) //nolint:gosec // G304: the test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	trimLayout, err := readWAVLayout(trimFile)
	_ = trimFile.Close()
	if err != nil {
		t.Fatalf("trimmed wav did not parse: %v", err)
	}
	if trimLayout.Frames != srcLayout.Frames-48000 {
		t.Fatalf("trim layout = %+v (src %+v)", trimLayout, srcLayout)
	}
	// Over-trim is a typed error, never a silent empty file.
	if err := ShiftWAVFile(srcPath, filepath.Join(dir, "over.wav"), srcLayout.Frames*4); !errors.Is(err, errOutOfRange) {
		t.Fatalf("over-trim err = %v, want errOutOfRange", err)
	}
}
