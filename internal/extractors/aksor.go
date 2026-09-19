package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// aksorExtractor ports Aksor (anicli-api player/aksor.py:22-56), the
// aksor.tv video player reached from the YummyAnime catalog (PR68): a
// plain REST player — the video id from the embed path resolves through
// GET /api/video/<id> straight into the quality map.
//
// Divergences from Python (test-enabling, prod-equivalent), mirroring
// the kodik/aniboom precedents:
//   - Python hardcodes https://player.aksor.tv as the API host
//     (aksor_parser.py fetch); this port derives scheme://host from the
//     embed URL. Production embeds are https player.aksor.tv URLs, so
//     wire behavior is identical, while tests can drive the flow over
//     httptest.
//   - the URL gate widens the upstream regex
//     (https?://(www.)?(player)?.aksor(.yani)?.tv/video) to the
//     "aksor" substring — the factory-wide convention; only aksor
//     hosts carry that substring.
//   - a quality key outside the upstream _QUALITY_KEYS table is
//     skipped instead of raising KeyError (aksor.py:40).
type aksorExtractor struct {
	http *netclient.Client
}

// aksorQualityKeys ports _QUALITY_KEYS (aksor.py:27): the API's q-keys
// onto vertical resolutions. q2k/q4k are the site's naming for 2048/
// 4096.
var aksorQualityKeys = map[string]int{
	"q1080": 1080,
	"q360":  360,
	"q480":  480,
	"q720":  720,
	"q2k":   2048,
	"q4k":   4096,
}

// Name identifies the extractor.
func (e *aksorExtractor) Name() string { return "aksor" }

// Matches keeps the substring gate convention (see the type comment for
// the relation to the upstream regex).
func (e *aksorExtractor) Matches(u string) bool {
	return strings.Contains(u, "aksor")
}

// Extract resolves an aksor embed into quality-keyed sources.
func (e *aksorExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:aksor: %w: %s", contracts.ErrExtractFailed, reason)
	}

	// Python strips the query before taking the id (aksor.py:30:
	// url.split("?", 1)[0]).
	path, _, _ := strings.Cut(rawURL, "?")
	id := path[strings.LastIndex(path, "/")+1:]
	if id == "" {
		return nil, shape("embed url carries no video id")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	base := parsed.Scheme + "://" + parsed.Host

	resp, err := e.http.Get(ctx, base+"/api/video/"+id, map[string]string{
		"Accept":  "application/json",
		"Referer": "https://old.yummyani.me/",
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:aksor: %w", err)
	}

	var data struct {
		Qualities map[string]*string `json:"qualities"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, shape(fmt.Sprintf("qualities json: %v", err))
	}

	results := map[string]contracts.VideoSource{}
	for key, linkPtr := range data.Qualities {
		if linkPtr == nil || *linkPtr == "" {
			continue // Python: `if not video_url: continue` (aksor.py:34)
		}
		quality, ok := aksorQualityKeys[key]
		if !ok {
			continue // unknown q-key: skipped, not a KeyError
		}
		link := *linkPtr
		// Python type: video_url.split(".")[-1] (aksor.py:42) — the
		// suffix after the LAST dot of the whole URL.
		mediaType := link[strings.LastIndex(link, ".")+1:]
		qs := strconv.Itoa(quality)
		results[qs] = contracts.VideoSource{URL: link, Quality: qs, Type: mediaType}
	}
	return results, nil
}
