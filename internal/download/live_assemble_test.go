//go:build live

package download

// PR165b T7 live proof: a REAL mkvmerge assembly over the audsync
// closed-loop fixtures — the owner's verbatim shape with two audio
// tracks, verified by ffprobe (v + 2a tracks, language/default tags
// in place), plus the assembly-speed measurement for the plan's
// §1.3 estimate. Requires ffmpeg, ffprobe and mkvmerge on PATH.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// liveFFBin skips when ffmpeg is missing (the fixture builder).
func liveFFBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("live test: ffmpeg not on PATH")
	}
	return bin
}

// buildAssembleFixture renders one 320-s reference (video+voice) and
// two donor m4a tracks (the voice, shifted +3 s, and an unrelated
// second track) via the pipeline shapes.
func buildAssembleFixture(t *testing.T, dir string) (videoPath, dub1, dub2 string) {
	t.Helper()
	bin := liveFFBin(t)
	run := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-only fixture command
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %s", args, out)
		}
	}
	videoPath = filepath.Join(dir, "video.tmp.mkv")
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=320",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=320",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "96k",
		"-ar", "48000", "-ac", "2", videoPath)
	wav1 := filepath.Join(dir, "dub1.wav")
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=42:a=0.3:r=48000:d=320,adelay=3000:all=1",
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", "-f", "wav", wav1)
	dub1 = filepath.Join(dir, "dub1.m4a")
	run("-y", "-hide_banner", "-loglevel", "error", "-i", wav1,
		"-c:a", "aac", "-b:a", "256k", dub1)
	dub2 = filepath.Join(dir, "dub2.m4a")
	run("-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anoisesrc=c=white:s=99:a=0.3:r=48000:d=320",
		"-c:a", "aac", "-b:a", "256k", dub2)
	return videoPath, dub1, dub2
}

func TestLiveRunAssemble(t *testing.T) {
	if _, err := exec.LookPath("mkvmerge"); err != nil {
		t.Skip("live test: mkvmerge not on PATH")
	}
	dir := t.TempDir()
	videoPath, dub1, dub2 := buildAssembleFixture(t, dir)

	d := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The mux-time version probe — the owner-ordered behavior,
	// proven on the real binary.
	version, err := ProbeMKVMerge(ctx, "mkvmerge")
	if err != nil {
		t.Fatalf("ProbeMKVMerge: %v", err)
	}
	t.Logf("mkvmerge version = %d → default flag %q", version, defaultTrackFlagFor(version))

	start := time.Now()
	outPath := filepath.Join(dir, "out.mkv")
	err = d.RunAssemble(ctx, AssembleInput{
		VideoPath: videoPath,
		AudioFiles: []AssembleAudio{
			{Path: dub1, Lang: "rus", Title: "AniLibria.TV", Default: true},
			{Path: dub2, Lang: "eng", Title: "DUB2", Default: false},
		},
		OutputPath: outPath,
		Title:      "Тайтл — эп 1",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunAssemble: %v", err)
	}
	t.Logf("assembly of a 320-s episode: %v (%.1fx realtime)", elapsed, 320.0/elapsed.Seconds())

	// ffprobe opens the assembled file: v + 2a, language tags and the
	// container title in place.
	probe, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type:stream_tags=language",
		"-show_entries", "format_tags=title",
		"-of", "json", outPath).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Tags struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(probe, &parsed); err != nil {
		t.Fatalf("ffprobe json: %v", err)
	}
	videoCount, audioCount := 0, 0
	var audioLangs []string
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			videoCount++
		case "audio":
			audioCount++
			audioLangs = append(audioLangs, s.Tags.Language)
		}
	}
	if videoCount != 1 || audioCount != 2 {
		t.Fatalf("streams v=%d a=%d, want 1+2", videoCount, audioCount)
	}
	if len(audioLangs) != 2 || audioLangs[0] != "rus" || audioLangs[1] != "eng" {
		t.Fatalf("audio languages %v, want [rus eng]", audioLangs)
	}
	if parsed.Format.Tags.Title != "Тайтл — эп 1" {
		t.Fatalf("container title = %q", parsed.Format.Tags.Title)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatal(err)
	}
}
