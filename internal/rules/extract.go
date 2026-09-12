package rules

import (
	"fmt"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// episodeRecord is the internal extraction target for episodes.
type episodeRecord struct {
	num string
	url string
}

// ExtractSearch applies a search rule set and maps results into
// contracts.SearchResult. Records missing title or URL are dropped
// (python: `if title and link_val`); sourceID fills SourceID.
func ExtractSearch(data any, rules []Rule, sourceID string) ([]contracts.SearchResult, error) {
	results, err := Extract(data, rules)
	if err != nil {
		return nil, fmt.Errorf("search extract: %w", err)
	}
	out := make([]contracts.SearchResult, 0, len(results))
	for _, r := range results {
		if r.Title == "" || r.URL == "" {
			continue
		}
		out = append(out, contracts.SearchResult{
			Title:    r.Title,
			URL:      r.URL,
			SourceID: sourceID,
		})
	}
	return out, nil
}

// ExtractEpisodes applies an episode rule set and maps results into
// contracts.Episode. Records missing num or URL are dropped (python:
// `if num and url`); the extracted URL/slug lands in RawID (python's
// Episode.url), and numeric episode numbers sort ascending when every Num
// parses.
func ExtractEpisodes(data any, rules []Rule) ([]contracts.Episode, error) {
	results, err := Extract(data, rules)
	if err != nil {
		return nil, fmt.Errorf("episode extract: %w", err)
	}
	records := make([]episodeRecord, 0, len(results))
	for _, r := range results {
		if r.Num == "" || r.URL == "" {
			continue
		}
		records = append(records, episodeRecord{num: r.Num, url: r.URL})
	}
	sortByNumericEpisode(records)

	out := make([]contracts.Episode, 0, len(records))
	for _, r := range records {
		out = append(out, contracts.Episode{Num: r.num, RawID: r.url})
	}
	return out, nil
}
