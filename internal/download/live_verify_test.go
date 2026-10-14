//go:build live

package download

// PR165b T8 live proof: VerifyAssembled over REAL assembled mkv
// files — a 3-second silent head FAILS the lead check, a file
// sounding from 0 passes, and a padded 2-second head passes when the
// expected lead is 2 s. Requires ffmpeg and mkvmerge on PATH.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// buildVerifyFixture renders a 60-s mkv whose audio starts with the
// given leadSilence (0 = sound from the first sample).
func buildVerifyFixture(t *testing.T, dir, name string, leadSilence float64) string {
	t.Helper()
	bin := liveFFBin(t)
	wav := filepath.Join(dir, name+".wav")
	mkv := filepath.Join(dir, name+".mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	run := func(args ...string) {
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixture command
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %s", args, out)
		}
	}
	// Loud broadband voice after the optional silent head.
	filter := "anoisesrc=c=white:s=7:a=0.4:r=48000:d=60"
	if leadSilence > 0 {
		filter += ",adelay=" + strconv.Itoa(int(leadSilence*1000)) + ":all=1"
	}
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", filter,
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", "-f", "wav", wav)
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=60",
		"-i", wav, "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "96k", mkv)
	return mkv
}

func TestLiveVerifyAssembled(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("live test: mkvmerge not on PATH")
	}
	dir := t.TempDir()
	d := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. A 3-s silent head with expected lead 0 (the trim geometry):
	// the RMS floor passes, the lead geometry FAILS.
	padded := buildVerifyFixture(t, dir, "padded3s", 3)
	err := d.VerifyAssembled(ctx, padded, 0)
	var silent *ErrSilentOutput
	if !err_is_silent(err, &silent) || silent.Check != "lead" {
		t.Fatalf("padded-3s/expected-0: %v, want ErrSilentOutput{lead}", err)
	}
	t.Logf("padded3s vs expected 0 → %v", silent.Details)

	// 2. Sound from the first sample, expected 0: PASS.
	clean := buildVerifyFixture(t, dir, "clean", 0)
	if err := d.VerifyAssembled(ctx, clean, 0); err != nil {
		t.Fatalf("clean/expected-0: %v", err)
	}

	// 3. A 2-s padded head, expected 2 s (the pad geometry): PASS.
	padded2 := buildVerifyFixture(t, dir, "padded2s", 2)
	if err := d.VerifyAssembled(ctx, padded2, 2*time.Second); err != nil {
		t.Fatalf("padded-2s/expected-2s: %v", err)
	}

	// 4. A silent file fails the RMS floor.
	silentFile := buildSilentFixture(t, dir)
	err = d.VerifyAssembled(ctx, silentFile, 0)
	if !err_is_silent(err, &silent) || silent.Check != "rms@45s" {
		t.Fatalf("silent file: %v, want ErrSilentOutput{rms@45s}", err)
	}
	// The caller removes the file on failure — prove removal works on
	// this path's contract.
	if err := os.Remove(silentFile); err != nil {
		t.Fatal(err)
	}
}

// err_is_silent is errors.As specialized for ErrSilentOutput.
func err_is_silent(err error, target **ErrSilentOutput) bool {
	if e, ok := err.(*ErrSilentOutput); ok {
		*target = e
		return true
	}
	return false
}

// buildSilentFixture renders a video with a DIGITALLY SILENT audio
// track (anullsrc) — the «половина без звука» catastrophe.
func buildSilentFixture(t *testing.T, dir string) string {
	t.Helper()
	bin := liveFFBin(t)
	mkv := filepath.Join(dir, "silent.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=50",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo:d=50",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "96k", mkv)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %s: %v", out, err)
	}
	return mkv
}
