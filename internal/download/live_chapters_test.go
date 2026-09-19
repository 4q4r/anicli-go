//go:build live

package download

// LIVE probe for the PR74 owner question «при скачивании скипы же
// вшиваются в файлы?»: the REAL Downloader + real ffmpeg bake a real
// skip.GenerateFFMetadata payload into the output, verified with
// ffprobe. Excluded from the hermetic suite by the `live` build tag.
// Run:
//
//	go test -tags live -run TestLiveChaptersBakedIntoDownload -count=1 -v ./internal/download/
//
// Requires ffmpeg+ffprobe on PATH (the downloader's existing
// dependency).

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/skip"
)

// TestLiveChaptersBakedIntoDownload: the python-parity bake — the
// CommandInput.ChaptersFile input lands as real chapter atoms in the
// finished mp4 (RU chapter names included).
func TestLiveChaptersBakedIntoDownload(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()

	// A tiny real stream with video+audio (the downloader's -map 0:v
	// -map 0:a shape).
	src := dir + "/src.mp4"
	mk := exec.CommandContext(ctx, "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=10:size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("fixture stream: %v\n%s", err, out)
	}

	// The REAL aniskip payload path: intervals → FFMETADATA → temp
	// file (the same calls downloadOne makes before the merge).
	intervals := []skip.Interval{
		{SkipType: "op", StartTime: 0, EndTime: 4, EpisodeLength: 10},
		{SkipType: "ed", StartTime: 8, EndTime: 10, EpisodeLength: 10},
	}
	bundle := skip.Bundle{
		FFMetadata:   skip.GenerateFFMetadata(intervals),
		ChapterTypes: []string{"ed", "op"},
		ProviderID:   skip.ProviderAniSkip,
	}
	chapters, err := bundle.WriteChaptersFile(dir)
	if err != nil {
		t.Fatalf("chapters file: %v", err)
	}
	defer func() { _ = os.Remove(chapters) }()

	out := dir + "/EP_1_live_720p.mp4"
	d := New(Options{})
	err = d.Download(ctx, CommandInput{
		Video:        contracts.VideoSource{URL: src},
		ChaptersFile: chapters,
		OutputPath:   out,
		Title:        "LIVE PR74 — серия 1",
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_chapters", "-of", "json", out)
	raw, perr := probe.CombinedOutput()
	if perr != nil {
		t.Fatalf("ffprobe: %v\n%s", perr, raw)
	}
	body := string(raw)
	t.Logf("ffprobe chapters:\n%s", body)
	for _, want := range []string{`"title": "Опенинг"`, `"title": "Эндинг"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("chapters not baked: %q missing from ffprobe output", want)
		}
	}
}
