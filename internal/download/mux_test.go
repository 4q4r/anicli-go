package download

// PR165: download modes + multi-audio muxing. The argv and container
// pins follow the PR164 research verdict (.sdd/pr164-mux-research.md):
// the decision table (mode × codec → container), the exact ffmpeg
// argv templates (video + N×audio with per-track -disposition and
// -metadata:s:a:N), and the gotchas (disposition last-match-wins →
// explicit per-track flags; no -shortest; the CODE enforces the
// not-AAC ⇒ mkv rule because the muxer silently accepts dts/flac).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestContainerForAudioOnly pins the PR164 §1.2 codec → extension
// table: aac/alac → m4a, flac → flac, mp3 → mp3, opus → opus,
// vorbis → ogg, everything else (ac3/eac3/dts/truehd/mlp/unknown)
// → mka — the universal Matroska fallback.
func TestContainerForAudioOnly(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"aac":     "m4a",
		"alac":    "m4a",
		"flac":    "flac",
		"mp3":     "mp3",
		"opus":    "opus",
		"vorbis":  "ogg",
		"ac3":     "mka",
		"eac3":    "mka",
		"dts":     "mka",
		"truehd":  "mka",
		"mlp":     "mka",
		"unknown": "mka",
		"":        "mka",
	}
	for codec, want := range cases {
		if got := ContainerFor(ModeAudioOnly, "", []string{codec}); got != want {
			t.Errorf("ContainerFor(audio_only, %q) = %q, want %q", codec, got, want)
		}
	}
}

// TestContainerForVideoOnly pins the video-only rule: the MP4-safe
// set {h264, hevc, av1, vp9} → mp4; exotic and unknown codecs → mkv.
func TestContainerForVideoOnly(t *testing.T) {
	t.Parallel()

	for _, codec := range []string{"h264", "hevc", "av1", "vp9"} {
		if got := ContainerFor(ModeVideoOnly, codec, nil); got != "mp4" {
			t.Errorf("ContainerFor(video_only, %q) = %q, want mp4", codec, got)
		}
	}
	for _, codec := range []string{"mpeg2video", "unknown", ""} {
		if got := ContainerFor(ModeVideoOnly, codec, nil); got != "mkv" {
			t.Errorf("ContainerFor(video_only, %q) = %q, want mkv", codec, got)
		}
	}
}

// TestContainerForVideoAudio pins the anchor rule: >1 audio OR any
// non-AAC audio ⇒ mkv, without exceptions (the muxer silently accepts
// dts/flac in mp4 — the code is the only guard); video + single AAC
// audio ⇒ mp4; unknown single audio ⇒ mkv (the safe fallback).
func TestContainerForVideoAudio(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		codecs []string
		want   string
	}{
		{"two aac", []string{"aac", "aac"}, "mkv"},
		{"three mixed", []string{"aac", "dts", "flac"}, "mkv"},
		{"single aac", []string{"aac"}, "mp4"},
		{"single dts", []string{"dts"}, "mkv"},
		{"single flac", []string{"flac"}, "mkv"},
		{"single truehd", []string{"truehd"}, "mkv"},
		{"single unknown", []string{"eac3"}, "mkv"},
		{"no probe data", nil, "mkv"},
	}
	for _, tc := range cases {
		if got := ContainerFor(ModeVideoAudio, "h264", tc.codecs); got != tc.want {
			t.Errorf("%s: ContainerFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBuildMuxCommandVideoMultiAudio pins the PR164 §2.1 template:
// one -i per input (video + each marked audio), -map 0:v:0 plus one
// -map i:a:0 per audio, -c:v copy -c:a copy, EXACTLY one explicit
// -disposition:a:N per output audio track (default on the chosen,
// 0 on the rest), -metadata:s:a:N language/title when known, NO
// -shortest, and the extension-preserving .part temp path.
func TestBuildMuxCommandVideoMultiAudio(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Mode: ModeVideoAudio,
		Video: contracts.VideoSource{
			URL:     "https://cdn/v.m3u8",
			Headers: map[string]string{"Referer": "https://r/"},
		},
		Tracks: []AudioTrack{
			{
				Source:  contracts.VideoSource{URL: "https://cdn/ru.m3u8", Headers: map[string]string{"Referer": "https://r/"}},
				Lang:    "rus",
				Title:   "Русская",
				Default: true,
			},
			{
				Source: contracts.VideoSource{URL: "https://cdn/jp.m3u8"},
				Lang:   "jpn",
				Title:  "Японская",
			},
		},
		OutputPath: "/dl/ep.mkv",
		Title:      "Тайтл — Эп 1",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-headers", "Referer: https://r/\r\n",
		"-i", "https://cdn/v.m3u8",
		"-headers", "Referer: https://r/\r\n",
		"-i", "https://cdn/ru.m3u8",
		"-i", "https://cdn/jp.m3u8",
		"-map", "0:v:0",
		"-map", "1:a:0",
		"-map", "2:a:0",
		"-c:v", "copy",
		"-c:a", "copy",
		"-disposition:a:0", "default",
		"-disposition:a:1", "0",
		"-metadata:s:a:0", "language=rus",
		"-metadata:s:a:0", "title=Русская",
		"-metadata:s:a:1", "language=jpn",
		"-metadata:s:a:1", "title=Японская",
		"-metadata", "title=Тайтл — Эп 1",
		"/dl/ep.part.mkv",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	for _, arg := range cmd.Args {
		if arg == "-shortest" {
			t.Error("-shortest must never appear (trims the output to the shortest stream)")
		}
	}
	if cmd.TempPath != "/dl/ep.part.mkv" {
		t.Errorf("TempPath = %q, want extension-preserving temp", cmd.TempPath)
	}
}

// TestBuildMuxCommandDefaultOnMiddleTrack pins the disposition
// override: the DEFAULT track is the one the user chose with `d`, not
// the first — every other track gets an explicit `0` (research gotcha
// 1: one -disposition per track, deterministic, warning-free).
func TestBuildMuxCommandDefaultOnMiddleTrack(t *testing.T) {
	t.Parallel()

	tracks := []AudioTrack{
		{Source: contracts.VideoSource{URL: "https://cdn/a1"}},
		{Source: contracts.VideoSource{URL: "https://cdn/a2"}, Default: true},
		{Source: contracts.VideoSource{URL: "https://cdn/a3"}},
	}
	cmd := BuildCommand(CommandInput{
		Mode:       ModeVideoAudio,
		Video:      contracts.VideoSource{URL: "https://cdn/v"},
		Tracks:     tracks,
		OutputPath: "/dl/ep.mkv",
		Title:      "T",
	})

	var dispositions []string
	for i, arg := range cmd.Args {
		if arg == "-disposition:a:0" || arg == "-disposition:a:1" || arg == "-disposition:a:2" {
			dispositions = append(dispositions, arg+" "+cmd.Args[i+1])
		}
	}
	want := []string{"-disposition:a:0 0", "-disposition:a:1 default", "-disposition:a:2 0"}
	if !reflect.DeepEqual(dispositions, want) {
		t.Errorf("dispositions = %v, want %v", dispositions, want)
	}
}

// TestBuildMuxCommandUnknownLangOmitsLanguage pins the metadata
// guard: a track with an unknown language carries the title only —
// no empty language= flag (Matroska language is ISO 639-2; an empty
// value would write garbage).
func TestBuildMuxCommandUnknownLangOmitsLanguage(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Mode:       ModeVideoAudio,
		Video:      contracts.VideoSource{URL: "https://cdn/v"},
		Tracks:     []AudioTrack{{Source: contracts.VideoSource{URL: "https://cdn/a"}, Title: "Дубляж"}},
		OutputPath: "/dl/ep.mkv",
		Title:      "T",
	})

	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "language=") {
		t.Errorf("unknown language must omit the language flag: %v", cmd.Args)
	}
	if !strings.Contains(joined, "title=Дубляж") {
		t.Errorf("the title must still be written: %v", cmd.Args)
	}
}

// TestBuildMuxCommandChapters pins the chapters input position: the
// FFMETADATA file is the input AFTER video + all audio tracks and
// -map_metadata points at it (the established download bake, PR74).
func TestBuildMuxCommandChapters(t *testing.T) {
	t.Parallel()

	chapters := filepath.Join(t.TempDir(), "ch.ffmetadata")
	if err := os.WriteFile(chapters, []byte(";FFMETADATA1\n"), 0o600); err != nil {
		t.Fatalf("seed chapters: %v", err)
	}

	cmd := BuildCommand(CommandInput{
		Mode:         ModeVideoAudio,
		Video:        contracts.VideoSource{URL: "https://cdn/v"},
		Tracks:       []AudioTrack{{Source: contracts.VideoSource{URL: "https://cdn/a"}}},
		ChaptersFile: chapters,
		OutputPath:   "/dl/ep.mkv",
		Title:        "T",
	})

	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-map_metadata 2") {
		t.Errorf("chapters input is index 2 (video+1 audio), args = %v", cmd.Args)
	}
}

// TestBuildCommandVideoOnlyMode pins the PR164 §2.2 template: an
// explicit -map 0:v:0 excludes audio and subs by itself (autoselection
// is off once -map appears; -an is redundant), stream copy.
func TestBuildCommandVideoOnlyMode(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Mode:       ModeVideoOnly,
		Video:      contracts.VideoSource{URL: "https://cdn/v.m3u8"},
		OutputPath: "/dl/ep.mp4",
		Title:      "T",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", "https://cdn/v.m3u8",
		"-map", "0:v:0",
		"-c:v", "copy",
		"-metadata", "title=T",
		"/dl/ep.part.mp4",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestBuildCommandAudioOnlyMode pins the PR164 §2.3 template: -vn
// -c:a copy — a bit-extract, never a re-encode (TrueHD stays TrueHD).
func TestBuildCommandAudioOnlyMode(t *testing.T) {
	t.Parallel()

	audio := contracts.VideoSource{URL: "https://cdn/a.m3u8"}
	cmd := BuildCommand(CommandInput{
		Mode:       ModeAudioOnly,
		Audio:      &audio,
		OutputPath: "/dl/ep.mka",
		Title:      "T",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", "https://cdn/a.m3u8",
		"-vn",
		"-c:a", "copy",
		"-metadata", "title=T",
		"/dl/ep.part.mka",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestLegacyCommandUnchanged pins the compat contract: the default
// path (no Mode, no Tracks) keeps the python-parity argv byte-for-byte
// (PR165 must not churn the established video+аудио shape).
func TestLegacyCommandUnchanged(t *testing.T) {
	t.Parallel()

	cmd := BuildCommand(CommandInput{
		Video:      contracts.VideoSource{URL: "https://cdn/v.m3u8"},
		OutputPath: "/dl/ep.mp4",
		Title:      "Эпизод",
	})

	want := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", "https://cdn/v.m3u8",
		"-map", "0:v",
		"-map", "0:a",
		"-c", "copy",
		"-metadata", "title=Эпизод",
		"/dl/ep.part.mp4",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestFFprobeCodecArgs pins the mandate's codec-probe argv:
// -v error -select_streams <s>:0 -show_entries stream=codec_name
// -of json, with the resolved source's HTTP headers ahead of the URL
// (the same per-input option placement the ffmpeg builder uses).
func TestFFprobeCodecArgs(t *testing.T) {
	t.Parallel()

	args := ffprobeCodecArgs("a", contracts.VideoSource{
		URL:     "https://cdn/a.m3u8",
		Headers: map[string]string{"Referer": "https://r/"},
	})

	want := []string{
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "json",
		"-headers", "Referer: https://r/\r\n",
		"https://cdn/a.m3u8",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}

	video := ffprobeCodecArgs("v", contracts.VideoSource{URL: "https://cdn/v"})
	if !reflect.DeepEqual(video, []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "json",
		"https://cdn/v",
	}) {
		t.Errorf("video args = %v", video)
	}
}

// TestParseCodecName pins the ffprobe JSON parse: the codec_name of
// the selected stream; empty on a missing stream or malformed output.
func TestParseCodecName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"streams":[{"codec_name":"aac"}]}`:             "aac",
		`{"streams":[{"codec_name":"truehd"}]}`:          "truehd",
		`{"streams":[]}`:                                 "",
		`{"streams":[{"index":0,"codec_type":"video"}]}`: "",
		`not json at all`:                                "",
	}
	for in, want := range cases {
		if got := parseCodecName([]byte(in)); got != want {
			t.Errorf("parseCodecName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProbeCodecRunsFFprobe pins the prober plumbing: the ffprobe
// binary runs with the codec argv and the raw stdout reaches the
// parser; a runner failure surfaces as the typed error.
func TestProbeCodecRunsFFprobe(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	var gotBin string
	var gotArgs []string
	d.run = func(_ context.Context, bin string, args []string) (string, error) {
		gotBin = bin
		gotArgs = args
		return `{"streams":[{"codec_name":"dts"}]}`, nil
	}
	codec, err := d.ProbeCodec(context.Background(), "a", contracts.VideoSource{URL: "https://cdn/a"})
	if err != nil {
		t.Fatalf("ProbeCodec: %v", err)
	}
	if codec != "dts" {
		t.Errorf("codec = %q, want dts", codec)
	}
	if gotBin != "ffprobe" {
		t.Errorf("bin = %q, want ffprobe", gotBin)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "-v" {
		t.Errorf("args = %v, want the codec probe argv", gotArgs)
	}

	d.run = func(_ context.Context, _ string, _ []string) (string, error) {
		return "", errors.New("boom")
	}
	if _, err := d.ProbeCodec(context.Background(), "v", contracts.VideoSource{URL: "https://cdn/v"}); err == nil {
		t.Error("a runner failure must surface, not degrade silently")
	}
}

// TestLangToISO639_2 pins the language mapping for the Matroska
// language metadata: the providers' two-letter ContentLanguage values
// map to ISO 639-2 (research gotcha 8); unknown codes map to "".
func TestLangToISO639_2(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"ru":      "rus",
		"ja":      "jpn",
		"en":      "eng",
		"uk":      "ukr",
		"de":      "deu",
		"fr":      "fra",
		"es":      "spa",
		"it":      "ita",
		"ko":      "kor",
		"zh":      "zho",
		"kk":      "kaz",
		"":        "",
		"klingon": "",
	}
	for in, want := range cases {
		if got := LangToISO639_2(in); got != want {
			t.Errorf("LangToISO639_2(%q) = %q, want %q", in, got, want)
		}
	}
}
