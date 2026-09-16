// Package torrent wraps the anacrolix/torrent client (PR35): realtime
// streaming playback of magnet links, .torrent URLs and raw infohashes
// through a local loopback HTTP server — no external programs, pure Go.
//
// The engine is lazy: constructing it starts nothing; the torrent
// client (DHT/announce machinery included) starts on the first added
// link and the loopback stream server starts with it. Release quality
// parsing is plain regex over release names — deterministic, fail-soft,
// no ML (user ruling).
package torrent

import (
	"regexp"
	"strconv"
	"strings"
)

// Quality is the metadata parsed out of a release or file name.
// Every field is optional: parsing is fail-soft, a missing label is
// simply absent.
type Quality struct {
	// Group is the release group (leading [brackets] preferred, else a
	// trailing tag that contains neither digits nor spaces).
	Group string
	// Resolution is the quality key: "2160", "1440", "1080", "720",
	// "480" or "360" (also derived from WxH forms like 1920x1080).
	Resolution string
	// Source is the release source: BDRip, BluRay, WEB-DL, WEBRip,
	// HDTV or DVDRip.
	Source string
	// VideoCodec is x264, x265, HEVC or AV1 (h264/h.264 count as
	// x264, h265/h.265 as x265).
	VideoCodec string
	// Audio is FLAC, AAC, AC3, Opus or DTS.
	Audio string
	// Dub is the Russian voice-over marker: Левша, AniLibria, JAM,
	// DUB or RUS — the first one found wins.
	Dub string
	// Episodes are the episode numbers this name maps to: a single
	// number for a one-episode file, an ascending range for a batch.
	Episodes []int
}

// Badge renders the quality label used in TUI columns ("1080p"),
// "?" when the resolution is unknown.
func (q Quality) Badge() string {
	if q.Resolution == "" {
		return "?"
	}
	return q.Resolution + "p"
}

// videoExtensions are the container suffixes stripped before parsing
// (a trailing ".mkv" must not confuse the tag patterns).
var videoExtensions = []string{
	".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".m2t", ".mov", ".wmv",
	".flv", ".ogm", ".ogv", ".mpg", ".mpeg", ".iso",
}

var (
	// Group: a leading bracket tag wins; a trailing bracket tag counts
	// only when it looks like a group name (no digits, no spaces —
	// otherwise "[BDRip 1920x1080 x265]" would parse as one).
	leadingGroupRe  = regexp.MustCompile(`^\[([^][]+)\]`)
	trailingGroupRe = regexp.MustCompile(`\[([^][]+)\]\s*$`)

	resolutionPRe  = regexp.MustCompile(`(?i)\b(2160|1440|1080|720|480|360)p\b`)
	resolutionWHRe = regexp.MustCompile(`\b\d{3,4}\s*[x×]\s*(2160|1440|1080|720|480|360)\b`)

	sourceRes = []struct {
		re     *regexp.Regexp
		canons string
	}{
		{regexp.MustCompile(`(?i)\bBD\s?Rip\b`), "BDRip"},
		{regexp.MustCompile(`(?i)\bBlu[- ]?Ray\b|\bBD\s?Remux\b`), "BluRay"},
		{regexp.MustCompile(`(?i)\bWEB[- ]?DL\b`), "WEB-DL"},
		{regexp.MustCompile(`(?i)\bWEB[- ]?Rip\b`), "WEBRip"},
		{regexp.MustCompile(`(?i)\bHDTV\b`), "HDTV"},
		{regexp.MustCompile(`(?i)\bDVD\s?Rip\b`), "DVDRip"},
	}

	codecRes = []struct {
		re     *regexp.Regexp
		canons string
	}{
		{regexp.MustCompile(`(?i)\bx[.-]?264\b|\bh[.-]?264\b|\bAVC\b`), "x264"},
		{regexp.MustCompile(`(?i)\bx[.-]?265\b|\bh[.-]?265\b`), "x265"},
		{regexp.MustCompile(`(?i)\bHEVC\b`), "HEVC"},
		{regexp.MustCompile(`(?i)\bAV1\b`), "AV1"},
	}

	audioRes = []struct {
		re     *regexp.Regexp
		canons string
	}{
		{regexp.MustCompile(`(?i)\bFLAC\b`), "FLAC"},
		{regexp.MustCompile(`(?i)\bAAC\b`), "AAC"},
		{regexp.MustCompile(`(?i)\bAC[- ]?3\b`), "AC3"},
		{regexp.MustCompile(`(?i)\bOpus\b`), "Opus"},
		{regexp.MustCompile(`(?i)\bDTS\b`), "DTS"},
	}

	// Dub markers, checked in canonical order; first match wins.
	dubRes = []struct {
		re     *regexp.Regexp
		canons string
	}{
		{regexp.MustCompile(`(?i)\b(?:Левша|Levsha)\b`), "Левша"},
		{regexp.MustCompile(`(?i)\bAniLibria\b`), "AniLibria"},
		{regexp.MustCompile(`\bJAM\b`), "JAM"},
		{regexp.MustCompile(`(?i)\bDUB\b`), "DUB"},
		{regexp.MustCompile(`\bRUS\b`), "RUS"},
	}

	// Episode patterns, checked in priority order: SxxEyy (the E
	// number is the episode), a batch range NN-MM (at most 3 digits a
	// side so years like 2023-2024 never match), "NN из MM" (one
	// episode of a MM-episode batch), "- NN" and "[NN]".
	seasonEpisodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)
	episodeRangeRe  = regexp.MustCompile(`\b(\d{1,3})\s*[-–]\s*(\d{1,3})\b`)
	episodeOfRe     = regexp.MustCompile(`(?i)\b(\d{1,3})\s*из\s*\d{1,3}\b`)
	episodeDashRe   = regexp.MustCompile(`-\s(\d{1,3})\b`)
	episodeBraceRe  = regexp.MustCompile(`\[(\d{1,3})\]`)
)

// ParseQuality extracts the quality metadata from a release or file
// name. Regex-only and deterministic; nothing is ever guessed.
func ParseQuality(name string) Quality {
	s := strings.TrimSpace(name)
	if s == "" {
		return Quality{}
	}
	s = stripVideoExtension(s)

	q := Quality{}
	if m := leadingGroupRe.FindStringSubmatch(s); m != nil {
		q.Group = m[1]
	} else if m := trailingGroupRe.FindStringSubmatch(s); m != nil && !strings.ContainsAny(m[1], "0123456789 ") {
		q.Group = m[1]
	}

	if m := resolutionPRe.FindStringSubmatch(s); m != nil {
		q.Resolution = m[1]
	} else if m := resolutionWHRe.FindStringSubmatch(s); m != nil {
		q.Resolution = m[1]
	}

	q.Source = firstMatch(s, sourceRes)
	q.VideoCodec = firstMatch(s, codecRes)
	q.Audio = firstMatch(s, audioRes)
	for _, d := range dubRes {
		if d.re.MatchString(s) {
			q.Dub = d.canons
			break
		}
	}
	q.Episodes = parseEpisodes(s)
	return q
}

// parseEpisodes resolves the episode numbers with the documented
// priority: SxxEyy > NN-MM range > "NN из MM" > "- NN" > "[NN]".
func parseEpisodes(s string) []int {
	if m := seasonEpisodeRe.FindStringSubmatch(s); m != nil {
		return []int{atoiSmall(m[2])}
	}
	if m := episodeRangeRe.FindStringSubmatch(s); m != nil {
		lo, hi := atoiSmall(m[1]), atoiSmall(m[2])
		if lo <= hi && hi-lo <= 999 {
			out := make([]int, 0, hi-lo+1)
			for n := lo; n <= hi; n++ {
				out = append(out, n)
			}
			return out
		}
	}
	if m := episodeOfRe.FindStringSubmatch(s); m != nil {
		return []int{atoiSmall(m[1])}
	}
	if m := episodeDashRe.FindStringSubmatch(s); m != nil {
		return []int{atoiSmall(m[1])}
	}
	if m := episodeBraceRe.FindStringSubmatch(s); m != nil {
		return []int{atoiSmall(m[1])}
	}
	return nil
}

// stripVideoExtension removes one trailing video container suffix
// (case-insensitive), mirroring how release tools label files.
func stripVideoExtension(s string) string {
	lower := strings.ToLower(s)
	for _, ext := range videoExtensions {
		if strings.HasSuffix(lower, ext) {
			return strings.TrimSpace(s[:len(s)-len(ext)])
		}
	}
	return s
}

// firstMatch returns the canonical form of the first matching pattern
// family ("" when none).
func firstMatch(s string, patterns []struct {
	re     *regexp.Regexp
	canons string
}) string {
	for _, p := range patterns {
		if p.re.MatchString(s) {
			return p.canons
		}
	}
	return ""
}

// atoiSmall parses small decimal numbers; a malformed match yields 0
// (the regexes above guarantee digits, the guard is belt-and-braces).
func atoiSmall(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
