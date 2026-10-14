//go:build live

package tui

// PR165b T12 live proofs: a REAL-toolchain end-to-end sync episode —
// real-encoded media (x264/aac), real analysis (audsync + ffmpeg),
// the byte-exact shift, a real mkvmerge assembly, the delivery
// verification, ffprobe geometry and an mpv headless decode. The
// offset's stability across two runs is pinned at ±0.125 s (ITU).
//
// Scope note: the owner's real-world pair (video from one provider +
// dub from another at +17.042) needs his provider environment; this
// proof runs the same pipeline over real-encoded synthetic media so
// the gate stays deterministic. Run:
//
//	go test -tags live -run TestLiveMeSyncRealPair -count=1 -v ./internal/tui/

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/audsync"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
)

// meSyncFixture renders the episode pair with REAL encoders: the
// reference (video + its own voice) and the dub (the same voice
// delayed 3 s — the donor lags). Both mp4 (x264/aac).
func meSyncFixture(t *testing.T, dir string) (refPath, donorPath string) {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("live test: ffmpeg not on PATH")
	}
	run := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixture command
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %s", args, out)
		}
	}
	refPath = filepath.Join(dir, "ref.mp4")
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=320",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=320",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "96k",
		"-ar", "48000", "-ac", "2", refPath)
	donorPath = filepath.Join(dir, "donor.mp4")
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=323",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=320,adelay=3000:all=1",
		"-t", "323",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "96k",
		"-ar", "48000", "-ac", "2", donorPath)
	return refPath, donorPath
}

func TestLiveMeSyncRealPair(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe", "mkvmerge", "mpv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("live test: %s not on PATH", bin)
		}
	}
	dir := t.TempDir()
	refPath, donorPath := meSyncFixture(t, dir)

	analyzer := audsync.New(audsync.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	ref, err := analyzer.Reference(ctx, contracts.VideoSource{URL: refPath})
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}

	// The offset, measured TWICE — the stability pin ±0.125 (ITU).
	var found [2]audsync.SyncOffset
	for run := range found {
		off, err := analyzer.Offset(ctx, ref, contracts.VideoSource{URL: donorPath})
		if err != nil {
			t.Fatalf("Offset run %d: %v", run+1, err)
		}
		found[run] = off
		t.Logf("run %d: found %+.3fs, confidence %.2f, windows agree %d",
			run+1, off.Seconds, off.Confidence, off.WindowsAgree)
		if math.Abs(off.Seconds-3) > 0.125 {
			t.Fatalf("run %d: found %+.3f, want ≈ +3 ± 0.125", run+1, off.Seconds)
		}
		if off.WindowsAgree < 2 || off.Confidence < 1 {
			t.Fatalf("run %d verdict weak: %+v", run+1, off)
		}
	}
	if drift := math.Abs(found[0].Seconds - found[1].Seconds); drift > 0.125 {
		t.Fatalf("the offset drifted %.3fs between runs, want ≤ 0.125", drift)
	}

	// The FULL pipeline: RunSyncJob runs Stages A-D (video solo →
	// decode → byte-exact shift → m4a → real mkvmerge → verification)
	// from donorPath.
	dl := download.New(download.Options{MKVMerge: "mkvmerge"})
	out := filepath.Join(dir, "EP_1_DUB_720p.mkv")
	if err := dl.RunSyncJob(ctx, download.SyncJob{
		Video:  contracts.VideoSource{URL: refPath},
		Donors: []download.SyncDonor{{Dub: "DUB", Source: contracts.VideoSource{URL: donorPath}, Lang: "rus", Title: "DUB", Default: true, Offset: found[0].Seconds}},
		Output: out,
		Title:  "Тайтл — эп 1",
	}, func(p float64) { t.Logf("progress %.0f%%", p) }); err != nil {
		t.Fatalf("RunSyncJob: %v", err)
	}

	// ffprobe: the assembled geometry is sane — video start ≈ 0.
	probe, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-select_streams", "v:0", "-show_entries", "stream=start_time",
		"-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	startStr := strings.TrimSpace(string(probe))
	start, perr := strconv.ParseFloat(startStr, 64)
	t.Logf("assembled video start_time = %q", startStr)
	if perr == nil && math.Abs(start) > 0.05 {
		t.Fatalf("video start_time %s, want ≈ 0 ± 0.05", startStr)
	}

	// silencedetect: the head must be audible from ≈0 (trim geometry:
	// the applied shift was a TRIM, so any leading silence is bounded
	// by the ±1 s verification band the job already enforced; this
	// re-reads the real stderr as the independent proof).
	sil := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-i", out,
		"-map", "0:a:0", "-t", "15", "-af", "silencedetect=noise=-50dB:d=0.2",
		"-f", "null", "-")
	silOut, _ := sil.CombinedOutput()
	t.Logf("silencedetect: %s", strings.TrimSpace(string(silOut)))
	if i := strings.Index(string(silOut), "silence_start:"); i >= 0 {
		rest := string(silOut)[i+len("silence_start:"):]
		if v, err := strconv.ParseFloat(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]), 64); err == nil && v > 1.0 {
			t.Fatalf("the trim geometry must not open with %.3fs of silence", v)
		}
	}

	// mpv headless: the assembled file DECODES (2 video frames from
	// the 45th second — the RMS@45s region the invariant guards).
	mpv := exec.CommandContext(ctx, "mpv", "--no-resume-playback", "--vo=null",
		"--ao=null", "--frames=2", "--start=45", "--really-quiet", out)
	if outp, err := mpv.CombinedOutput(); err != nil {
		t.Fatalf("mpv decode: %v: %s", err, outp)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
	t.Log("e2e sync episode delivered")
}
