package extractors

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// sibnetExtractor ports SibnetExtractor (extractors.py:34-60).
type sibnetExtractor struct {
	http *netclient.Client
}

// sibnetSrcRe ports src:\s*"\/?([^"]+\.mp4)" (extractors.py:45); the
// Python \/ escape is a plain slash.
var sibnetSrcRe = regexp.MustCompile(`src:\s*"/?([^"]+\.mp4)"`)

// Name identifies the extractor.
func (e *sibnetExtractor) Name() string { return "sibnet" }

// Matches ports the Python URL gate (extractors.py:38).
func (e *sibnetExtractor) Matches(u string) bool { return strings.Contains(u, "sibnet") }

// Extract resolves a sibnet shell page to its single mp4.
func (e *sibnetExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:sibnet: %w: %s", contracts.ErrExtractFailed, reason)
	}

	rawURL = normalizeProtocolRelative(rawURL)

	// Python fetches with Referer: url (extractors.py:44) — the
	// normalized embed URL.
	resp, err := e.http.Get(ctx, rawURL, map[string]string{"Referer": rawURL})
	if err != nil {
		return nil, fmt.Errorf("extractor:sibnet: %w", err)
	}

	m := sibnetSrcRe.FindStringSubmatch(string(resp.Body))
	if m == nil {
		return nil, shape("no src: mp4 on the player page")
	}
	slug := m[1]
	final := slug
	if !strings.HasPrefix(slug, "http") {
		final = "https://video.sibnet.ru/" + slug
	}
	return map[string]contracts.VideoSource{
		"480": {URL: final, Quality: "480", Headers: map[string]string{"Referer": rawURL}},
	}, nil
}
