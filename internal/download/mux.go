package download

// PR165: download modes and the multi-audio mux command builder. The
// argv shapes and container rules follow the PR164 research verdict
// (.sdd/pr164-mux-research.md): video + N×audio in ONE ffmpeg pass
// with explicit per-track dispositions and language/title metadata,
// video-only and audio-only (stream copy, never a re-encode). The
// container decision is enforced HERE in code — the ffmpeg muxer
// silently accepts dts/flac in mp4, so the not-AAC ⇒ mkv rule can
// never be delegated to it (research gotcha 4).

import (
	"fmt"
	"os"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// Mode is the download content mode («⬇ Скачать серии» menu).
type Mode string

const (
	// ModeVideoAudio muxes the video with its audio tracks (the
	// default, as today). With a marked audio set the tracks mux
	// onto the video in one pass; without one the legacy
	// python-parity command runs unchanged.
	ModeVideoAudio Mode = "video_audio"
	// ModeVideoOnly writes the video stream alone.
	ModeVideoOnly Mode = "video_only"
	// ModeAudioOnly writes the chosen dub's audio alone,
	// codec-preserving (extension per the codec table).
	ModeAudioOnly Mode = "audio_only"
)

// AudioTrack is one external audio input of the multi-audio mux.
type AudioTrack struct {
	// Source is the resolved audio URL (a dub's stream; ffmpeg
	// extracts its first audio track).
	Source contracts.VideoSource
	// Lang is the ISO 639-2 language for -metadata:s:a:N language
	// ("" = unknown — the flag is omitted).
	Lang string
	// Title is the human-readable track name (the dub name; players
	// show it in their track menus).
	Title string
	// Default marks the default disposition (exactly one per mux).
	Default bool
}

// mp4SafeVideoCodecs are the video codecs MP4 registers (research
// §1.1): everything else muxes into Matroska.
var mp4SafeVideoCodecs = map[string]bool{
	"h264": true,
	"hevc": true,
	"av1":  true,
	"vp9":  true,
}

// audioOnlyExtensions pins the PR164 §1.2 codec → extension table:
// the native container wins where one exists, everything else (ac3,
// eac3, dts, truehd, mlp, unknown) lands in universal Matroska.
var audioOnlyExtensions = map[string]string{
	"aac":    "m4a",
	"alac":   "m4a",
	"flac":   "flac",
	"mp3":    "mp3",
	"opus":   "opus",
	"vorbis": "ogg",
}

// ContainerFor decides the output extension per the research decision
// table. audioCodecs are the probed codec_names of the audio tracks
// that will be mapped (empty = no probe data — the safe fallback is
// Matroska). The rule is total: >1 audio OR any non-AAC audio ⇒ mkv;
// video + single AAC ⇒ mp4; video-only mp4-safe set else mkv;
// audio-only per the codec table.
func ContainerFor(mode Mode, videoCodec string, audioCodecs []string) string {
	switch mode {
	case ModeAudioOnly:
		codec := ""
		if len(audioCodecs) > 0 {
			codec = audioCodecs[0]
		}
		if ext, ok := audioOnlyExtensions[codec]; ok {
			return ext
		}
		return "mka"
	case ModeVideoOnly:
		if mp4SafeVideoCodecs[videoCodec] {
			return "mp4"
		}
		return "mkv"
	default:
		switch {
		case len(audioCodecs) > 1:
			return "mkv"
		case len(audioCodecs) == 1 && audioCodecs[0] == "aac":
			return "mp4"
		default:
			return "mkv"
		}
	}
}

// buildMuxArgs renders the PR164 §2.1 template: one -i per input,
// -map 0:v:0 plus one -map i:a:0 per audio, -c:v copy -c:a copy,
// EXACTLY one explicit -disposition:a:N per output audio track (the
// chosen default, 0 for the rest — research gotcha 1: the last
// -disposition match wins, so per-track flags are the only
// deterministic shape), -metadata:s:a:N language/title when known, no
// -shortest (it trims the output to the shortest stream), and the
// global title. Chapters keep their established slot after every
// media input with -map_metadata pointing at them.
func buildMuxArgs(in CommandInput, tracks []AudioTrack) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}

	args = append(args, headerArgs(in.Video.Headers)...)
	args = append(args, "-i", in.Video.URL)
	for _, tr := range tracks {
		args = append(args, headerArgs(tr.Source.Headers)...)
		args = append(args, "-i", tr.Source.URL)
	}

	chaptersIdx := -1
	if in.ChaptersFile != "" {
		if _, err := os.Stat(in.ChaptersFile); err == nil {
			chaptersIdx = 1 + len(tracks)
			args = append(args, "-i", in.ChaptersFile)
		}
	}

	args = append(args, "-map", "0:v:0")
	for i := range tracks {
		args = append(args, "-map", fmt.Sprintf("%d:a:0", i+1))
	}
	args = append(args, "-c:v", "copy")
	args = append(args, "-c:a", "copy")

	for i, tr := range tracks {
		if tr.Default {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "default")
		} else {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "0")
		}
	}
	for i, tr := range tracks {
		if tr.Lang != "" {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+tr.Lang)
		}
		if tr.Title != "" {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "title="+tr.Title)
		}
	}

	if chaptersIdx >= 0 {
		args = append(args, "-map_metadata", fmt.Sprintf("%d", chaptersIdx))
	}

	title := in.Title
	if title == "" {
		title = "Эпизод"
	}
	args = append(args, "-metadata", "title="+title)

	args = append(args, TempPathFor(in.OutputPath))
	return args
}

// buildVideoOnlyArgs renders the PR164 §2.2 template: the explicit
// -map 0:v:0 excludes audio and subs by itself (autoselection is off
// once -map appears; -an is redundant), stream copy.
func buildVideoOnlyArgs(in CommandInput) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}
	args = append(args, headerArgs(in.Video.Headers)...)
	args = append(args, "-i", in.Video.URL)

	chaptersIdx := -1
	if in.ChaptersFile != "" {
		if _, err := os.Stat(in.ChaptersFile); err == nil {
			chaptersIdx = 1
			args = append(args, "-i", in.ChaptersFile)
		}
	}

	args = append(args, "-map", "0:v:0")
	args = append(args, "-c:v", "copy")
	if chaptersIdx >= 0 {
		args = append(args, "-map_metadata", fmt.Sprintf("%d", chaptersIdx))
	}

	title := in.Title
	if title == "" {
		title = "Эпизод"
	}
	args = append(args, "-metadata", "title="+title)

	args = append(args, TempPathFor(in.OutputPath))
	return args
}

// buildAudioOnlyArgs renders the PR164 §2.3 template: -vn -c:a copy —
// a bit-extract that never re-encodes (TrueHD stays TrueHD). No bsf:
// aac_adtstoasc auto-inserts for the MOV/MP4/M4A family and for
// Matroska alike (research §2.3). The output extension (hence the
// container) was chosen by the caller from the probed codec.
func buildAudioOnlyArgs(in CommandInput) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}
	args = append(args, headerArgs(in.Audio.Headers)...)
	args = append(args, "-i", in.Audio.URL)
	args = append(args, "-vn")
	args = append(args, "-c:a", "copy")

	title := in.Title
	if title == "" {
		title = "Эпизод"
	}
	args = append(args, "-metadata", "title="+title)

	args = append(args, TempPathFor(in.OutputPath))
	return args
}

// LangToISO639_2 maps the providers' two-letter ContentLanguage
// values onto the ISO 639-2 codes Matroska's legacy Language element
// expects (research gotcha 8); unknown codes degrade to "".
func LangToISO639_2(lang string) string {
	if len(lang) == 3 {
		return lang // already ISO 639-2 (or close enough to pass through)
	}
	if iso, ok := iso639_1to639_2[lang]; ok {
		return iso
	}
	return ""
}

var iso639_1to639_2 = map[string]string{
	"ru": "rus",
	"ja": "jpn",
	"en": "eng",
	"uk": "ukr",
	"de": "deu",
	"fr": "fra",
	"es": "spa",
	"it": "ita",
	"ko": "kor",
	"zh": "zho",
	"kk": "kaz",
}
