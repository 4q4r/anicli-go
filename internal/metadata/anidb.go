package metadata

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// DefaultAniDBBaseURL is the AniDB site root.
const DefaultAniDBBaseURL = "https://anidb.net"

// anidbSelectors mirrors the Python tuple: the /anime/ link selector,
// its table-scoped variant and the .animetitle class.
var anidbSelectors = []string{
	"a[href*='/anime/']",
	"table tr td a[href*='/anime/']",
	".animetitle",
}

// AniDBClient is the AniDB HTML metadata provider.
type AniDBClient struct {
	net     *netclient.Client
	baseURL string
	log     *slog.Logger
}

// NewAniDBClient builds the provider; baseURL may be empty for the
// production default; logger may be nil.
func NewAniDBClient(net *netclient.Client, baseURL string, logger *slog.Logger) *AniDBClient {
	if baseURL == "" {
		baseURL = DefaultAniDBBaseURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AniDBClient{net: net, baseURL: strings.TrimSuffix(baseURL, "/"), log: logger}
}

// ID identifies the provider in manager ordering.
func (c *AniDBClient) ID() string { return "anidb" }

// SearchAlternativeTitles returns normalized title aliases scraped from
// the AniDB search form (adb.search + do.search=1), walking the selector
// tuple over one parsed document and stopping at maxScrapedAliases
// (python early-return inside the selector loop).
func (c *AniDBClient) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	queryParams := url.Values{}
	queryParams.Set("adb.search", query)
	queryParams.Set("do.search", "1")

	resp, err := c.net.Get(ctx, c.baseURL+"/search/anime?"+queryParams.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("metadata anidb: %w", err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return nil, fmt.Errorf("metadata anidb: parse response: %w", err)
	}

	seen := map[string]bool{}
	var out []string
	add := func(title string) bool {
		normalized := normalizeTitle(title)
		if normalized == "" || seen[normalized] {
			return true
		}
		seen[normalized] = true
		out = append(out, normalized)
		return len(out) < maxScrapedAliases
	}

	for _, selector := range anidbSelectors {
		full := len(out) >= maxScrapedAliases
		doc.Find(selector).EachWithBreak(func(_ int, node *goquery.Selection) bool {
			if !add(node.Text()) {
				full = true
				return false
			}
			if titleAttr, ok := node.Attr("title"); ok {
				if !add(titleAttr) {
					full = true
					return false
				}
			}
			return true
		})
		if full {
			break
		}
	}
	return out, nil
}
