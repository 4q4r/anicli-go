// Dub stream exclusion (PR23): the [providers] exclude_streams regex
// list drops trash streams (trailers, ads) from every episode listing
// a provider emits. The patterns are compiled once when the registry
// is built and applied per request; invalid patterns are rejected
// fail-loud at settings load (config.Settings.Validate) and again
// here so a hand-built configuration cannot smuggle one in.

package providers

import (
	"context"
	"fmt"
	"regexp"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// StreamFilter drops dub streams whose name matches any of the
// configured exclude_streams regular expressions. A nil or empty
// filter passes everything through untouched.
type StreamFilter struct {
	patterns []*regexp.Regexp
}

// NewStreamFilter compiles the exclude patterns, failing loud on the
// first invalid one.
func NewStreamFilter(patterns []string) (*StreamFilter, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("exclude_streams %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}
	return &StreamFilter{patterns: compiled}, nil
}

// FilterEmbeds returns embeds without the excluded dub names. The
// input map is never mutated; when nothing is excluded it is returned
// as-is (no per-request copying on the hot path).
func (f *StreamFilter) FilterEmbeds(embeds map[string][]string) map[string][]string {
	if f == nil || len(f.patterns) == 0 || len(embeds) == 0 {
		return embeds
	}
	out := make(map[string][]string, len(embeds))
	for dub, links := range embeds {
		if f.Excluded(dub) {
			continue
		}
		out[dub] = links
	}
	return out
}

// Excluded reports whether a dub name matches any exclude pattern.
func (f *StreamFilter) Excluded(dub string) bool {
	if f == nil {
		return false
	}
	for _, re := range f.patterns {
		if re.MatchString(dub) {
			return true
		}
	}
	return false
}

// dubFilteredProvider wraps a provider, dropping excluded dub streams
// from its episode listings — the place every consumer (TUI session,
// HTTP API, parity) reads the dub list from. ResolveStream is left
// untouched: an excluded stream can no longer be selected because it
// never appears in a listing. Providers hydrating their dubs lazily
// via FetchDubs inside GetEpisodes (gogoanime) are covered too: the
// filter sees the final embeds.
type dubFilteredProvider struct {
	contracts.Provider

	filter *StreamFilter
}

// GetEpisodes lists the wrapped provider's episodes with excluded dub
// streams removed from every episode's RawEmbeds.
func (p dubFilteredProvider) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	episodes, err := p.Provider.GetEpisodes(ctx, animeURL)
	if err != nil {
		return nil, err
	}
	if p.filter == nil {
		return episodes, nil
	}
	for i := range episodes {
		episodes[i].RawEmbeds = p.filter.FilterEmbeds(episodes[i].RawEmbeds)
	}
	return episodes, nil
}

// FetchDubs forwards the lazy-dub capability (contracts.DubsHydrator)
// of the wrapped provider — without this forwarding the registry
// wrapper HIDES the capability from every consumer, which is how the
// PR43 «Ист: 0» bug survived: the hydration could never be reached.
// Hydrated embeds pass through the same exclusion filter as the eager
// listings. Providers without the capability resolve to a no-op.
func (p dubFilteredProvider) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	hydrator, ok := p.Provider.(contracts.DubsHydrator)
	if !ok {
		return episode, nil
	}
	out, err := hydrator.FetchDubs(ctx, episode)
	if err != nil {
		return out, err
	}
	if p.filter != nil && out != nil {
		out.RawEmbeds = p.filter.FilterEmbeds(out.RawEmbeds)
	}
	return out, nil
}
