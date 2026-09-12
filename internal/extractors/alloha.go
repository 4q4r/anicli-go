package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// allohaExtractor ports AllohaExtractor (extractors.py:328-391), an
// animego embed player.
//
// Divergence from Python (test-enabling, prod-equivalent): Python
// hardcodes https:// for the API base and Origin (extractors.py:350,
// 361); this port derives scheme://host from the embed URL. Production
// alloha embeds are https, so wire behavior is unchanged.
type allohaExtractor struct {
	http *netclient.Client
}

var (
	allohaIDRe    = regexp.MustCompile(`["']id["']:\s*["']?(\d+)["']?`)
	allohaTokenRe = regexp.MustCompile(`["']?token["']?:\s*["']([^"']+)["']`)
	allohaVarTok  = regexp.MustCompile(`var\s+token\s*=\s*["']([^"']+)["']`)
)

// Name identifies the extractor.
func (e *allohaExtractor) Name() string { return "alloha" }

// Matches ports the Python URL gate (extractors.py:331).
func (e *allohaExtractor) Matches(u string) bool {
	return strings.Contains(u, "alloha") || strings.Contains(u, "all.")
}

// Extract resolves an alloha player page through its /movie API.
func (e *allohaExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:alloha: %w: %s", contracts.ErrExtractFailed, reason)
	}

	rawURL = normalizeProtocolRelative(rawURL)

	resp, err := e.http.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:alloha: %w", err)
	}
	html := string(resp.Body)

	id := allohaIDRe.FindStringSubmatch(html)
	if id == nil {
		return nil, shape("no id on the player page (extractors.py:340-341)")
	}
	token := ""
	if m := allohaTokenRe.FindStringSubmatch(html); m != nil {
		token = m[1]
	} else if m := allohaVarTok.FindStringSubmatch(html); m != nil {
		token = m[1]
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	base := parsed.Scheme + "://" + parsed.Host

	form := url.Values{}
	form.Set("token", token)
	form.Set("av1", "0")
	form.Set("autoplay", "0")
	form.Set("audio", "")
	form.Set("subtitle", "")

	apiResp, err := e.http.PostForm(ctx, base+"/movie/"+id[1], form, map[string]string{
		"Origin":           base,
		"Referer":          rawURL,
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:alloha: %w", err)
	}

	var data struct {
		HLSSource []struct {
			Quality map[string]any `json:"quality"`
		} `json:"hlsSource"`
		Source []struct {
			Quality map[string]any `json:"quality"`
		} `json:"source"`
	}
	if err := json.Unmarshal(apiResp.Body, &data); err != nil {
		return nil, shape(fmt.Sprintf("movie json: %v", err))
	}

	// hlsSource wins over source (extractors.py:371).
	sourcesList := data.HLSSource
	if len(sourcesList) == 0 {
		sourcesList = data.Source
	}

	results := map[string]contracts.VideoSource{}
	for _, source := range sourcesList {
		for qStr, rawLink := range source.Quality {
			link, ok := rawLink.(string)
			if !ok || link == "" || qStr == "Object" {
				continue // falsy link or the "Object" artifact
			}
			if strings.Contains(link, " or ") {
				link = strings.SplitN(link, " or ", 2)[0]
			}
			q, convErr := strconv.Atoi(digitsOnly(qStr))
			if convErr != nil {
				continue
			}
			qs := strconv.Itoa(q)
			results[qs] = contracts.VideoSource{
				URL:     link,
				Quality: qs,
				Headers: map[string]string{"Origin": base, "Referer": rawURL},
			}
		}
	}
	return results, nil
}
