package skip

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FFMETADATA rendering rules ported from anicli-py
// AniSkipClient._generate_ffmetadata_content.
const (
	// ffmetaPrologueMinStart is the leading gap (seconds) above which a
	// prologue chapter is inserted.
	ffmetaPrologueMinStart = 5.0
	// ffmetaGapMinSeconds is the inter-skip gap (seconds) above which a
	// main-content chapter fills the hole.
	ffmetaGapMinSeconds = 1.0
	// ffmetaTailMinSeconds is the trailing gap (seconds) above which a
	// tail chapter closes the episode.
	ffmetaTailMinSeconds = 5.0
	// ffmetaFallbackEpisodeLen substitutes a missing episode length so
	// tail chapters always have an end.
	ffmetaFallbackEpisodeLen = 1440.0
)

// ffmetaLabel maps a skip type to its RU chapter title.
func ffmetaLabel(skipType string) string {
	switch skipType {
	case "op", "mixed-op":
		return "Опенинг"
	case "ed", "mixed-ed":
		return "Эндинг"
	case "recap":
		return "Пересказ"
	case "preview":
		return "Превью"
	case "eyecatch":
		return "Айкэтч"
	case "sponsor":
		return "Спонсор"
	default:
		return "Скип"
	}
}

// ffmetaChapter is one rendered chapter.
type ffmetaChapter struct {
	start float64
	end   float64
	title string
}

// GenerateFFMetadata renders the FFMETADATA1 chapter document for the
// given intervals: skips are sorted by start, gaps become main-content
// chapters, a leading gap becomes a prologue and a trailing gap becomes
// a typed tail (epilogue after an ending, credits after a preview).
// Empty input renders as empty content.
func GenerateFFMetadata(intervals []Interval) string {
	if len(intervals) == 0 {
		return ""
	}

	episodeLen := 0.0
	sorted := make([]Interval, len(intervals))
	copy(sorted, intervals)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].StartTime < sorted[j].StartTime
	})
	for _, iv := range sorted {
		episodeLen = max(episodeLen, iv.EpisodeLength)
	}
	if episodeLen == 0 {
		episodeLen = ffmetaFallbackEpisodeLen
	}

	chapters := make([]ffmetaChapter, 0, len(sorted)*2)
	if sorted[0].StartTime > ffmetaPrologueMinStart {
		chapters = append(chapters, ffmetaChapter{
			start: 0,
			end:   sorted[0].StartTime,
			title: "Пролог",
		})
		// Ported quirk (python aniskip.py): current_time is NOT advanced
		// after the prologue, so a leading gap also emits a duplicate
		// main-content chapter over the prologue span. Replicated
		// verbatim for parity.
	}

	current := 0.0
	lastType := ""
	for _, iv := range sorted {
		if iv.StartTime > current+ffmetaGapMinSeconds {
			chapters = append(chapters, ffmetaChapter{
				start: current,
				end:   iv.StartTime,
				title: "Основной контент",
			})
		}
		chapters = append(chapters, ffmetaChapter{
			start: iv.StartTime,
			end:   iv.EndTime,
			title: ffmetaLabel(iv.SkipType),
		})
		current = iv.EndTime
		lastType = iv.SkipType
	}

	if episodeLen > current+ffmetaTailMinSeconds {
		title := "Основной контент"
		switch lastType {
		case "ed", "mixed-ed":
			title = "Эпилог/Превью"
		case "preview":
			title = "Титры"
		}
		chapters = append(chapters, ffmetaChapter{
			start: current,
			end:   episodeLen,
			title: title,
		})
	}

	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for _, ch := range chapters {
		b.WriteString("\n[CHAPTER]\n")
		b.WriteString("TIMEBASE=1/1000\n")
		fmt.Fprintf(&b, "START=%d\n", int64(ch.start*1000))
		fmt.Fprintf(&b, "END=%d\n", int64(ch.end*1000))
		fmt.Fprintf(&b, "title=%s\n", ch.title)
	}
	return b.String()
}

// WriteChaptersFile materializes FFMETADATA content as a temp file with
// the given name prefix and the .ffmetadata suffix, returning its path.
// Callers own the file (mpv launcher and downloader delete it when
// done).
func WriteChaptersFile(dir, prefix, content string) (string, error) {
	f, err := os.CreateTemp(dir, prefix+"*.ffmetadata")
	if err != nil {
		return "", fmt.Errorf("write chapters file: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write chapters file %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close chapters file %s: %w", f.Name(), err)
	}
	return filepath.ToSlash(f.Name()), nil
}
