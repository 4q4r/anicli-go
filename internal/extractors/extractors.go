// Package extractors ports the video extractor factory of the frozen
// Python original (anicli-py anicli/core/extractors.py): each extractor
// turns one embed-player URL into quality-keyed VideoSources, and the
// factory walks them in the Python registration order until one yields
// sources.
//
// Divergences from the Python original are deliberate and documented at
// each site:
//
//   - quality keys are Go strings ("720"), matching the contracts DTO
//     used across the Go port, where Python used int keys;
//   - the Python extractors swallow every exception into {}, while the
//     Go port returns typed errors (extractor name +
//     contracts.ErrExtractFailed for page-shape mismatches; transport
//     errors pass through with the extractor name attached) — the
//     no-silent-failure policy of this repo;
//   - three factory extractors unreachable from the 11 registered
//     providers stay unported and surface a typed "unreachable" error
//     instead (see skippedExtractor).
//
// The animekai-style arithmetic obfuscation (n*7+3 family) is NOT ported:
// no extractor reachable from the registered providers evaluates
// arithmetic expressions (kwik's obfuscation is the base-conversion
// routine already in internal/crypto), so the restricted arithmetic
// evaluator the task sketched has no consumer and is intentionally
// absent.
package extractors

import (
	"context"
	"fmt"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Extractor resolves one embed-player URL into quality-keyed sources.
// Matches is the Python extract() URL gate; the factory only invokes
// Extract on URLs Matches accepted.
type Extractor interface {
	// Name identifies the extractor in typed errors and factory order.
	Name() string
	// Matches ports the Python URL substring gate.
	Matches(u string) bool
	// Extract fetches the embed page and assembles the media sources.
	Extract(ctx context.Context, url string) (map[string]contracts.VideoSource, error)
}

// Factory ports ExtractorFactory (extractors.py:653-689): an ordered
// extractor list walked per embed URL.
type Factory struct {
	extractors []Extractor
}

// NewFactory builds the extractor list in the Python registration order
// (extractors.py:658-671) with one task-mandated addition: the kwik
// extractor, disabled upstream, is appended in full so animepahe embeds
// resolve.
func NewFactory(http *netclient.Client) *Factory {
	return &Factory{extractors: []Extractor{
		&kodikExtractor{http: http},
		&aniboomExtractor{http: http},
		&cdnVideoHubExtractor{http: http, apiBase: cdnVideoHubAPIBase},
		&allohaExtractor{http: http},
		&sibnetExtractor{http: http},
		&skippedExtractor{name: "askor", matches: containsAny("aksor.yani.tv")},
		&skippedExtractor{name: "csst", matches: containsAny("csst.online")},
		&skippedExtractor{name: "sovetromantica_embed", matches: containsAny("sovetromantica.com/embed")},
		&gogoPlayExtractor{http: http},
		&streamTapeExtractor{http: http},
		&doodExtractor{http: http},
		&kwikExtractor{http: http},
	}}
}

// GetSources ports ExtractorFactory.get_sources (extractors.py:673-689):
// walk the factory-ordered extractors; the first one that yields sources
// wins; a failed extractor contributes nothing and iteration continues
// (the Python except: continue). The first failure is remembered so a
// resolve that yielded nothing anywhere surfaces it loudly. After the
// loop, a bare media URL resolves through the direct fallback
// (extractors.py:686-687).
func (f *Factory) GetSources(ctx context.Context, embedURL string) (map[string]contracts.VideoSource, error) {
	var firstErr error
	for _, ex := range f.extractors {
		if !ex.Matches(embedURL) {
			continue
		}
		sources, err := ex.Extract(ctx, embedURL)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(sources) > 0 {
			return sources, nil
		}
	}

	if strings.HasSuffix(embedURL, ".mp4") || strings.HasSuffix(embedURL, ".m3u8") {
		return map[string]contracts.VideoSource{
			"720": {URL: embedURL, Quality: "720"},
		}, nil
	}

	if firstErr != nil {
		return nil, firstErr
	}
	return map[string]contracts.VideoSource{}, nil
}

// skippedExtractor marks a Python factory extractor deliberately not
// ported because no registered provider can produce its URLs. Evidence
// (grep of anicli-py anicli/providers/, 2026-09-13):
//
//   - askor (aksor.yani.tv, extractors.py:394-406): no provider emits
//     aksor URLs; the only site embedding the player is yummyanime,
//     which is not among the registered providers (and even
//     yummyanime.py never places aksor URLs into raw_embeds);
//   - csst (csst.online, extractors.py:409-423): no provider in the
//     Python tree emits csst.online embed URLs at all;
//   - sovetromantica_embed (sovetromantica.com/embed,
//     extractors.py:426-438): the frozen Python sovetromantica
//     provider stashes episode-page URLs and resolves them inline
//     (sovetromantica.py:84-98), never /embed URLs — and the Go
//     sovetromantica provider was removed in PR22 (site dead: domain
//     hijacked off the anime project, frozen 2025), so nothing can
//     produce these URLs here either.
//
// mp4upload needs no entry: the Python factory omits it entirely
// (extractors.py:670 comment — disabled), so its URLs match nothing and
// resolve to {} exactly like upstream.
type skippedExtractor struct {
	name    string
	matches func(u string) bool
}

// Name identifies the skipped extractor.
func (e *skippedExtractor) Name() string { return e.name }

// Matches keeps the Python URL gate so factory ordering stays verbatim.
func (e *skippedExtractor) Matches(u string) bool { return e.matches(u) }

// Extract explains the skip with the typed taxonomy instead of the
// Python silent {}.
func (e *skippedExtractor) Extract(_ context.Context, _ string) (map[string]contracts.VideoSource, error) {
	return nil, fmt.Errorf("extractor:%s: %w: not ported: unreachable from the registered providers",
		e.name, contracts.ErrExtractFailed)
}

// containsAny builds a Python-style substring gate over the alternatives.
func containsAny(needles ...string) func(string) bool {
	return func(u string) bool {
		for _, n := range needles {
			if strings.Contains(u, n) {
				return true
			}
		}
		return false
	}
}
