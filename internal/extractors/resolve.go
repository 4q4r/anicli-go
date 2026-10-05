package extractors

import (
	"context"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Resolve ports the provider-facing surface of
// ExtractorFactory.get_sources (anicli-py anicli/core/extractors.py:
// 673-689) as consumed by the provider resolve loops:
//
//   - URLs ending in .mp4/.m3u8 resolve directly to a quality-720
//     VideoSource (extractors.py:686-687). The suffix is checked FIRST:
//     Python tries extractors and only reaches the fallback when they
//     all yield empty — which is exactly what happens for a raw media
//     URL that happens to match an extractor substring (e.g. "all.mp4"
//     matching Alloha's "all."): the extractor extracts nothing from a
//     bare media file. Checking the suffix first reproduces the
//     observable Python outcome while skipping the wasted embed fetch;
//   - other URLs run through the real extractor factory in Python
//     registration order (kwik appended where Python disabled it);
//   - results merge across links with Python dict.update semantics —
//     later links overwrite earlier keys (gogoanime.py:128-132,
//     animego.py:134-137, kodik.py:178-183, anilib.py:162);
//   - a link whose extraction fails contributes nothing but is
//     remembered: only when NOTHING resolved does the first failure
//     surface (the Go no-silent-failure replacement for Python's
//     swallowed exceptions), never shadowing links that do resolve.
//
// Shared by the compiled providers (providers.resolveEmbeds delegates
// here) and the Lua SDK's anicli.extract (PR116) — one implementation
// for both surfaces.
func Resolve(ctx context.Context, http *netclient.Client, links []string) (map[string]contracts.VideoSource, error) {
	factory := NewFactory(http)
	sources := map[string]contracts.VideoSource{}
	var firstErr error

	for _, link := range links {
		if strings.HasSuffix(link, ".mp4") || strings.HasSuffix(link, ".m3u8") {
			sources["720"] = contracts.VideoSource{URL: link, Quality: "720"}
			continue
		}
		res, err := factory.GetSources(ctx, link)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for quality, src := range res {
			sources[quality] = src // dict.update: later links overwrite
		}
	}

	if len(sources) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return sources, nil
}
