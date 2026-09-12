package extractors

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// doodExtractor ports DoodStreamExtractor (extractors.py:538-558).
//
// Divergence from Python (documented): the original hardcodes
// https://dood.la as the pass_md5 base (extractors.py:549); this port
// derives the origin from the embed URL instead. Dood serves the same
// player under many mirror domains (dood.la, dood.ws, ...), all of which
// answer /pass_md5 themselves, so deriving the origin is both
// mirror-correct and keeps the extractor testable offline.
type doodExtractor struct {
	http *netclient.Client
}

var (
	doodPassMD5Re = regexp.MustCompile(`/pass_md5/([^']*)`)
	doodTokenRe   = regexp.MustCompile(`\?token=([^&']+)`)
)

// Name identifies the extractor.
func (e *doodExtractor) Name() string { return "dood" }

// Matches ports the Python URL gate (extractors.py:540).
func (e *doodExtractor) Matches(u string) bool { return strings.Contains(u, "dood") }

// Extract resolves a dood embed page through the pass_md5 flow.
func (e *doodExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:dood: %w: %s", contracts.ErrExtractFailed, reason)
	}

	resp, err := e.http.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:dood: %w", err)
	}
	html := string(resp.Body)

	passMD5 := doodPassMD5Re.FindStringSubmatch(html)
	token := doodTokenRe.FindStringSubmatch(html)
	if passMD5 == nil || token == nil {
		return nil, shape("no pass_md5 path or token on the page")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	base := parsed.Scheme + "://" + parsed.Host

	md5Resp, err := e.http.Get(ctx, base+"/pass_md5/"+passMD5[1],
		map[string]string{"Referer": rawURL})
	if err != nil {
		return nil, fmt.Errorf("extractor:dood: %w", err)
	}

	// final = <md5 body> + <FULL "?token=..." match> + "&expiry=<ms>"
	// (extractors.py:554): the Python uses match.group(0), keeping the
	// "?token=" prefix.
	finalURL := string(md5Resp.Body) + token[0] + "&expiry=" +
		strconv.FormatInt(time.Now().UnixMilli(), 10)
	return map[string]contracts.VideoSource{
		"1080": {
			URL:     finalURL,
			Quality: "1080",
			Type:    "mp4",
			Headers: map[string]string{"Referer": base},
		},
	}, nil
}
