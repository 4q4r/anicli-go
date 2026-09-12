package extractors

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// streamTapeExtractor ports StreamTapeExtractor (extractors.py:523-535).
type streamTapeExtractor struct {
	http *netclient.Client
}

// streamTapeRe ports robotlink'\)\.innerHTML = '(.+?)'\+ \('xcd(.+?)'\)
// (extractors.py:529).
var streamTapeRe = regexp.MustCompile(`robotlink'\)\.innerHTML = '(.+?)'\+ \('xcd(.+?)'\)`)

// Name identifies the extractor.
func (e *streamTapeExtractor) Name() string { return "streamtape" }

// Matches ports the Python URL gate (extractors.py:525).
func (e *streamTapeExtractor) Matches(u string) bool { return strings.Contains(u, "streamtape") }

// Extract resolves a streamtape page to its stitched mp4 link.
func (e *streamTapeExtractor) Extract(ctx context.Context, url string) (map[string]contracts.VideoSource, error) {
	resp, err := e.http.Get(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:streamtape: %w", err)
	}
	m := streamTapeRe.FindStringSubmatch(string(resp.Body))
	if m == nil {
		return nil, fmt.Errorf("extractor:streamtape: %w: no robotlink assembly on the page",
			contracts.ErrExtractFailed)
	}
	link := "https:" + m[1] + "xcd" + m[2]
	return map[string]contracts.VideoSource{
		"1080": {URL: link, Quality: "1080", Type: "mp4"},
	}, nil
}
