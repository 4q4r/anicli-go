package torrent

import (
	"regexp"
	"testing"
)

// TestParseQualityMatchesReference is the PR82 P2#6 behavior-identity
// proof: the combined family scanners must return EXACTLY what the
// original ordered per-pattern loops returned, over the bench corpus
// plus adversarial multi-tag names (conflicting sources, multiple dubs,
// case variants of the case-sensitive JAM/RUS markers, mixed families).
//
// The reference below is the original implementation, verbatim: the
// first pattern (in list order) that matches ANYWHERE in the string
// wins — position never influences the choice.
func TestParseQualityMatchesReference(t *testing.T) {
	type pat struct {
		re     *regexp.Regexp
		canons string
	}
	sourceRes := []pat{
		{regexp.MustCompile(`(?i)\bBD\s?Rip\b`), "BDRip"},
		{regexp.MustCompile(`(?i)\bBlu[- ]?Ray\b|\bBD\s?Remux\b`), "BluRay"},
		{regexp.MustCompile(`(?i)\bWEB[- ]?DL\b`), "WEB-DL"},
		{regexp.MustCompile(`(?i)\bWEB[- ]?Rip\b`), "WEBRip"},
		{regexp.MustCompile(`(?i)\bHDTV\b`), "HDTV"},
		{regexp.MustCompile(`(?i)\bDVD\s?Rip\b`), "DVDRip"},
	}
	codecRes := []pat{
		{regexp.MustCompile(`(?i)\bx[.-]?264\b|\bh[.-]?264\b|\bAVC\b`), "x264"},
		{regexp.MustCompile(`(?i)\bx[.-]?265\b|\bh[.-]?265\b`), "x265"},
		{regexp.MustCompile(`(?i)\bHEVC\b`), "HEVC"},
		{regexp.MustCompile(`(?i)\bAV1\b`), "AV1"},
	}
	audioRes := []pat{
		{regexp.MustCompile(`(?i)\bFLAC\b`), "FLAC"},
		{regexp.MustCompile(`(?i)\bAAC\b`), "AAC"},
		{regexp.MustCompile(`(?i)\bAC[- ]?3\b`), "AC3"},
		{regexp.MustCompile(`(?i)\bOpus\b`), "Opus"},
		{regexp.MustCompile(`(?i)\bDTS\b`), "DTS"},
	}
	dubRes := []pat{
		{regexp.MustCompile(`(?i)\b(?:Левша|Levsha)\b`), "Левша"},
		{regexp.MustCompile(`(?i)\bAniLibria\b`), "AniLibria"},
		{regexp.MustCompile(`\bJAM\b`), "JAM"},
		{regexp.MustCompile(`(?i)\bDUB\b`), "DUB"},
		{regexp.MustCompile(`\bRUS\b`), "RUS"},
	}
	first := func(s string, pats []pat) string {
		for _, p := range pats {
			if p.re.MatchString(s) {
				return p.canons
			}
		}
		return ""
	}
	reference := func(name string) Quality {
		s := stripVideoExtension(trimSpace(name))
		if s == "" {
			return Quality{}
		}
		q := Quality{}
		if m := leadingGroupRe.FindStringSubmatch(s); m != nil {
			q.Group = m[1]
		} else if m := trailingGroupRe.FindStringSubmatch(s); m != nil && !containsDigitsOrSpace(m[1]) {
			q.Group = m[1]
		}
		if m := resolutionPRe.FindStringSubmatch(s); m != nil {
			q.Resolution = m[1]
		} else if m := resolutionWHRe.FindStringSubmatch(s); m != nil {
			q.Resolution = m[1]
		}
		q.Source = first(s, sourceRes)
		q.VideoCodec = first(s, codecRes)
		q.Audio = first(s, audioRes)
		q.Dub = first(s, dubRes)
		q.Episodes = parseEpisodes(s)
		return q
	}

	// The bench corpus (1000 realistic names across every label
	// family)…
	corpus := append([]string(nil), benchReleaseCorpus...)
	// …plus adversarial multi-tag names: conflicting sources in both
	// priority orders, case variants of the case-sensitive markers,
	// overlapping families, and empty/group-only edges.
	corpus = append(corpus,
		"WEB-DL BDRip", "BDRip WEB-DL", "BD Remux BDRip", "DVDRip HDTV BluRay WEBRip",
		"[LEASE] Anime (2020) [720p] [x264] [AAC] (DUB) (RUS) (jam)",
		"anime 1080p JAM x265 FLAC Levsha",
		"anime 1080p jam x265 FLAC levsha", // lowercase jam/levsha: JAM is case-sensitive, Levsha is not
		"RUS rus Rus DUB dub",              // only case-insensitive markers may match the variants
		"HDTV x264 AAC AC3 Opus DTS",
		"WEBRip WEB-DL", "WEBDL WEBRip",
		"One Piece - 0987 [1080p][BDRemux][FLAC]",
		"1920x1080 BDRemux AVC DTS RUS",
		"[AniLibria] TV show S01E01 [WEB-DL 1080p]",
		"[GROUP] Name Episode 5 [X264]",
		"Название 12 из 24 (2021) [HDTV] [RUS]",
	)

	for i, name := range corpus {
		got := ParseQuality(name)
		want := reference(name)
		if got.Group != want.Group || got.Resolution != want.Resolution ||
			got.Source != want.Source || got.VideoCodec != want.VideoCodec ||
			got.Audio != want.Audio || got.Dub != want.Dub ||
			!intSlicesEqual(got.Episodes, want.Episodes) {
			t.Fatalf("corpus[%d] %q:\n got %+v\nwant %+v", i, name, got, want)
		}
	}
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func containsDigitsOrSpace(s string) bool {
	for _, r := range s {
		if r == ' ' || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}
