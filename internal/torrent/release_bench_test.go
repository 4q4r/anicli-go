package torrent

import (
	"fmt"
	"testing"
)

// Benchmarks for the release-name parser (PR81). ParseQuality is the
// hot path behind every torrent roster row (23-provider fan-outs render
// one Quality per result) and every file pick inside a torrent. Inputs
// are prebuilt BEFORE b.Loop (auto-excluded from timing); results sink
// into a package-level var so the compiler cannot dead-code the loop
// body.

// releaseNameCorpus is 1000 realistic release names mixing every label
// family ParseQuality recognises: groups, resolutions (p and WxH
// forms), sources, codecs, audio, Russian dub markers and episode
// ranges/batches. One benchmark op parses the whole corpus.
func releaseNameCorpus() []string {
	groups := []string{"LEASE", "AniLibria", "JAM", "Levsha", "SubsPlease", "EVR", "HorribleSubs", ""}
	sources := []string{"BDRip", "BluRay", "WEB-DL", "WEBRip", "HDTV", "DVDRip", ""}
	codecs := []string{"x264", "x265", "HEVC", "AV1", ""}
	audio := []string{"FLAC", "AAC", "AC3", "Opus", "DTS", ""}
	dubs := []string{"Левша", "AniLibria", "JAM", "DUB", "RUS", ""}
	resolutions := []string{"2160p", "1440p", "1080p", "720p", "480p", "1920x1080", ""}
	titles := []string{
		"One Piece", "Naruto Shippuuden", "Bleach Sennen Kessen-hen", "Shingeki no Kyojin",
		"Jujutsu Kaisen", "Chainsaw Man", "Vinland Saga S2", "Bocchi the Rock",
	}
	names := make([]string, 0, 1000)
	for i := 0; len(names) < 1000; i++ {
		ep := 1 + i%1200
		var epPart string
		switch i % 4 {
		case 0:
			epPart = fmt.Sprintf(" - %03d", ep)
		case 1:
			epPart = fmt.Sprintf(" S01E%02d", 1+i%12)
		case 2:
			epPart = fmt.Sprintf(" [%03d]", ep)
		default:
			epPart = fmt.Sprintf(" %02d из %d", 1+i%12, 12)
		}
		name := fmt.Sprintf("[%s] %s%s (%d) [%s] [%s] [%s] [%s].mkv",
			groups[i%len(groups)],
			titles[(i/4)%len(titles)],
			epPart,
			2000+i%26,
			resolutions[i%len(resolutions)],
			sources[i%len(sources)],
			codecs[i%len(codecs)],
			audio[i%len(audio)],
		)
		if dubs[i%len(dubs)] != "" {
			name = name[:len(name)-4] + " (" + dubs[i%len(dubs)] + ").mkv"
		}
		names = append(names, name)
	}
	return names
}

// benchReleaseCorpus is built once per test binary (not per
// iteration): 1000 names ≈ the big-torrent file-roster scale.
var benchReleaseCorpus = releaseNameCorpus()

// benchSinkRelease keeps ParseQuality results alive across iterations.
var benchSinkRelease Quality

// BenchmarkParseQualityCorpus1000 parses 1000 release names per op —
// the roster-scale cost of the quality columns.
func BenchmarkParseQualityCorpus1000(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, name := range benchReleaseCorpus {
			benchSinkRelease = ParseQuality(name)
		}
	}
}

// BenchmarkParseQualitySingle parses one heavy release name per op —
// the per-row cost in a live roster render.
func BenchmarkParseQualitySingle(b *testing.B) {
	b.ReportAllocs()
	name := "[AniLibria] One Piece Wan Pisu - 1100 (2024) [1080p] [BDRip] [x265] [FLAC] (Левша).mkv"
	for b.Loop() {
		benchSinkRelease = ParseQuality(name)
	}
}

// BenchmarkParseQualityLight parses a minimal name (no optional tags)
// — the lower bound of the parser.
func BenchmarkParseQualityLight(b *testing.B) {
	b.ReportAllocs()
	name := "One Piece 1100.mkv"
	for b.Loop() {
		benchSinkRelease = ParseQuality(name)
	}
}

// BenchmarkParseEpisodes benchmarks the episode-range extraction on a
// batch name (the dedupe/dub grouping input).
func BenchmarkParseEpisodes(b *testing.B) {
	b.ReportAllocs()
	name := "[SubsPlease] Some Anime (01-24) [1080p] [x265].mkv"
	for b.Loop() {
		benchSinkEpisodes = parseEpisodes(name)
	}
}

var benchSinkEpisodes []int
