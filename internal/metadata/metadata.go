// Package metadata provides the anime title-alias enrichment clients
// (AniList, Kitsu, AniSearch, AniDB) and the ordered manager that merges
// their results for search query expansion. Ported from anicli-py
// anicli/core/anime_metadata.py (Jellyfin plugin parity, per the feature
// inventory section C).
package metadata

import (
	"strings"
	"unicode"
)

// Limits ported verbatim from the Python original.
const (
	// maxTitleLength trims over-long titles for dedupe/search.
	maxTitleLength = 220
	// maxScrapedAliases caps aliases collected from one HTML scrape
	// before stopping early.
	maxScrapedAliases = 30
)

// normalizeTitle normalizes title text for dedupe and search usage
// (python _normalize_title): collapse whitespace runs to single spaces,
// strip, and cap at maxTitleLength runes.
func normalizeTitle(value string) string {
	var b strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return ""
	}
	if runes := []rune(cleaned); len(runes) > maxTitleLength {
		return strings.TrimSpace(string(runes[:maxTitleLength]))
	}
	return cleaned
}
