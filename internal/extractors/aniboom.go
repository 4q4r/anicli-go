package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// aniboomExtractor ports AniboomExtractor (extractors.py:211-255), one
// of the animego embed players.
type aniboomExtractor struct {
	http *netclient.Client
}

// aniboomHeaders ports the Python header set (extractors.py:217-222).
// The explicit User-Agent there re-asserts the configured one, which the
// netclient already sends on every request — same wire result.
func aniboomHeaders() map[string]string {
	return map[string]string{
		"Referer":         "https://animego.org/",
		"Accept-Language": "ru-RU",
		"Origin":          "https://aniboom.one",
	}
}

var (
	aniboomParamsRe = regexp.MustCompile(`data-parameters="([^"]+)"`)
	aniboomHLSRe    = regexp.MustCompile(`"hls":\s*\{"src":"(https?.*?\.m3u8)"`)
)

// Name identifies the extractor.
func (e *aniboomExtractor) Name() string { return "aniboom" }

// Matches ports the Python URL gate (extractors.py:214).
func (e *aniboomExtractor) Matches(u string) bool { return strings.Contains(u, "aniboom") }

// Extract resolves an aniboom embed page.
func (e *aniboomExtractor) Extract(ctx context.Context, url string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:aniboom: %w: %s", contracts.ErrExtractFailed, reason)
	}
	headers := aniboomHeaders()

	resp, err := e.http.Get(ctx, url, headers)
	if err != nil {
		return nil, fmt.Errorf("extractor:aniboom: %w", err)
	}
	html := string(resp.Body)

	m := aniboomParamsRe.FindStringSubmatch(html)
	if m == nil {
		// Fallback: scrape the escaped hls src off the page
		// (extractors.py:227-231).
		stripped := strings.ReplaceAll(html, "\\", "")
		if hls := aniboomHLSRe.FindStringSubmatch(stripped); hls != nil {
			return map[string]contracts.VideoSource{
				"1080": {URL: hls[1], Quality: "1080", Headers: headers},
			}, nil
		}
		return nil, shape("neither data-parameters nor an hls src on the page")
	}

	// Unescape chain, verbatim (extractors.py:234-235).
	raw := m[1]
	raw = strings.ReplaceAll(raw, "\\", "")
	raw = strings.ReplaceAll(raw, "&quot;}", "}")
	raw = strings.ReplaceAll(raw, "&quot;{", "{")
	raw = strings.ReplaceAll(raw, "&quot;", `"`)
	raw = strings.ReplaceAll(raw, `}"`, "}")
	raw = strings.ReplaceAll(raw, `"{`, "{")

	var data struct {
		HLS *struct {
			Src string `json:"src"`
		} `json:"hls"`
		DASH *struct {
			Src string `json:"src"`
		} `json:"dash"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, shape(fmt.Sprintf("data-parameters json: %v", err))
	}

	results := map[string]contracts.VideoSource{}
	if data.HLS != nil && data.HLS.Src != "" {
		src := strings.ReplaceAll(data.HLS.Src, `\/`, `/`)
		results["1080"] = contracts.VideoSource{
			URL: src, Quality: "1080", Headers: headers, Type: "m3u8",
		}
	}
	// Dash overwrites the same 1080 key when it carries an .mpd
	// (extractors.py:246-250).
	if data.DASH != nil && strings.HasSuffix(data.DASH.Src, ".mpd") {
		src := strings.ReplaceAll(data.DASH.Src, `\/`, `/`)
		results["1080"] = contracts.VideoSource{
			URL: src, Quality: "1080", Headers: headers,
		}
	}
	return results, nil
}
