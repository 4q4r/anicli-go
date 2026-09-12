package providers

import (
	"fmt"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// embedExtractors ports the URL matching table of the Python
// ExtractorFactory (anicli-py anicli/core/extractors.py:658-671) in its
// original order. The extractors themselves land in a later PR
// (internal/extractors); until then a matched URL surfaces as an
// ErrExtractFailed "pending" error instead of the Python original's
// silently empty result (task ruling: no silent success paths).
var embedExtractors = []struct {
	name    string
	matches func(u string) bool
}{
	{"kodik", func(u string) bool { return strings.Contains(u, "kodik") || strings.Contains(u, "aniqit") }},
	{"aniboom", func(u string) bool { return strings.Contains(u, "aniboom") }},
	{"cdnvideohub", func(u string) bool { return strings.Contains(u, "cdn-iframe") }},
	{"alloha", func(u string) bool { return strings.Contains(u, "alloha") || strings.Contains(u, "all.") }},
	{"sibnet", func(u string) bool { return strings.Contains(u, "sibnet") }},
	{"askor", func(u string) bool { return strings.Contains(u, "aksor.yani.tv") }},
	{"csst", func(u string) bool { return strings.Contains(u, "csst.online") }},
	{"sovetromantica_embed", func(u string) bool { return strings.Contains(u, "sovetromantica.com/embed") }},
	{"gogoplay", func(u string) bool {
		return strings.Contains(u, "gogoplay") || strings.Contains(u, "playtaku") ||
			strings.Contains(u, "playgo") || strings.Contains(u, "goload") ||
			strings.Contains(u, "streaming.php") || strings.Contains(u, "embedplus")
	}},
	{"streamtape", func(u string) bool { return strings.Contains(u, "streamtape") }},
	{"dood", func(u string) bool { return strings.Contains(u, "dood") }},
}

// matchEmbedExtractor returns the name of the first extractor whose URL
// rule matches link, in ExtractorFactory order.
func matchEmbedExtractor(link string) (string, bool) {
	for _, ex := range embedExtractors {
		if ex.matches(link) {
			return ex.name, true
		}
	}
	return "", false
}

// pendingExtractorError marks an embed URL whose extractor is not ported
// yet.
func pendingExtractorError(name string) error {
	return fmt.Errorf("extractor:%s pending: %w", name, contracts.ErrExtractFailed)
}

// resolveEmbeds ports the surface of ExtractorFactory.get_sources
// (anicli-py anicli/core/extractors.py:673-689) needed by the wave-1
// providers:
//
//   - URLs matched by an extractor rule carry a pending error (the Python
//     original would attempt extraction; that machinery is a later PR);
//   - URLs ending in .mp4/.m3u8 resolve directly to a quality-720
//     VideoSource (extractors.py:686-687);
//   - URLs matching nothing contribute nothing (Python: {}).
//
// Blending mirrors the Python dict.update semantics: a link that cannot
// resolve does not fail links that can. Only when nothing resolved AND a
// pending extractor was hit does the function surface the pending error.
func resolveEmbeds(links []string) (map[string]contracts.VideoSource, error) {
	sources := map[string]contracts.VideoSource{}
	var pending error

	for _, link := range links {
		if name, matched := matchEmbedExtractor(link); matched {
			if pending == nil {
				pending = pendingExtractorError(name)
			}
			continue
		}
		if strings.HasSuffix(link, ".mp4") || strings.HasSuffix(link, ".m3u8") {
			sources["720"] = contracts.VideoSource{URL: link, Quality: "720"}
		}
	}

	if len(sources) == 0 && pending != nil {
		return nil, pending
	}
	return sources, nil
}
