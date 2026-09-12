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

// DefaultAniSearchBaseURL is the AniSearch site root.
const DefaultAniSearchBaseURL = "https://www.anisearch.com"

// AniSearchClient is the AniSearch HTML metadata provider.
type AniSearchClient struct {
	net     *netclient.Client
	baseURL string
	log     *slog.Logger
}

// NewAniSearchClient builds the provider; baseURL may be empty for the
// production default; logger may be nil.
func NewAniSearchClient(net *netclient.Client, baseURL string, logger *slog.Logger) *AniSearchClient {
	if baseURL == "" {
		baseURL = DefaultAniSearchBaseURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AniSearchClient{net: net, baseURL: strings.TrimSuffix(baseURL, "/"), log: logger}
}

// ID identifies the provider in manager ordering.
func (c *AniSearchClient) ID() string { return "anisearch" }

// SearchAlternativeTitles returns normalized title aliases scraped from
// the AniSearch anime index (anchor texts + title attributes of
// /anime/ links), stopping at maxScrapedAliases.
func (c *AniSearchClient) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	queryParams := url.Values{}
	queryParams.Set("char", query)

	resp, err := c.net.Get(ctx, c.baseURL+"/anime/index?"+queryParams.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("metadata anisearch: %w", err)
	}

	return scrapeAnchors(resp.Body, "a[href*='/anime/']"), nil
}

// scrapeAnchors extracts normalized, deduplicated alias candidates from
// every anchor matching the selector: anchor text plus title attribute,
// capped at maxScrapedAliases (python MAX_SCRAPED_ALIASES early stop).
func scrapeAnchors(html []byte, selector string) []string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(html)))
	if err != nil {
		return nil
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

	doc.Find(selector).EachWithBreak(func(_ int, node *goquery.Selection) bool {
		if !add(node.Text()) {
			return false
		}
		if titleAttr, ok := node.Attr("title"); ok {
			if !add(titleAttr) {
				return false
			}
		}
		return true
	})
	return out
}
