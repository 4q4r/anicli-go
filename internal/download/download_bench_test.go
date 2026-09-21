package download

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR81 benchmarks for the download command assembly (the per-task
// ffmpeg argv build) and the offline index snapshot round trip.

var (
	benchSinkCommand Command
)

// BenchmarkBuildCommandHLS assembles the ffmpeg command for a
// separate-audio HLS download with chapters — the per-episode
// download-start cost.
func BenchmarkBuildCommandHLS(b *testing.B) {
	b.ReportAllocs()
	in := CommandInput{
		Video: contracts.VideoSource{
			URL:     "https://cdn.example/hls/1080/index.m3u8",
			Quality: "1080",
			Type:    "m3u8",
			Headers: map[string]string{"Referer": "https://prov.example/", "User-Agent": "anicli-bench"},
		},
		Audio: &contracts.VideoSource{
			URL:     "https://cdn.example/audio/ru/index.m3u8",
			Quality: "ru",
			Type:    "m3u8",
		},
		ChaptersFile: "/tmp/anicli-bench/chapters.ffmeta",
		OutputPath:   "/tmp/anicli-bench/one-piece-1100.mkv",
		Title:        "One Piece — 1100",
	}
	for b.Loop() {
		benchSinkCommand = BuildCommand(in)
		if len(benchSinkCommand.Args) == 0 {
			b.Fatal("empty command args")
		}
	}
}

// BenchmarkBuildCommandProgressive — the single-file (mp4) shape.
func BenchmarkBuildCommandProgressive(b *testing.B) {
	b.ReportAllocs()
	in := CommandInput{
		Video:      contracts.VideoSource{URL: "https://cdn.example/media/1080.mp4", Quality: "1080", Type: "mp4"},
		OutputPath: "/tmp/anicli-bench/episode.mp4",
		Title:      "Episode",
	}
	for b.Loop() {
		benchSinkCommand = BuildCommand(in)
	}
}
