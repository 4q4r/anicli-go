package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// DefaultKitsuEndpoint is the Kitsu JSON:API root.
const DefaultKitsuEndpoint = "https://kitsu.io/api/edge"

// kitsuResponse models the interesting slice of the JSON:API reply.
type kitsuResponse struct {
	Data []struct {
		Attributes struct {
			CanonicalTitle    string            `json:"canonicalTitle"`
			Titles            map[string]string `json:"titles"`
			AbbreviatedTitles []string          `json:"abbreviatedTitles"`
		} `json:"attributes"`
	} `json:"data"`
}

// KitsuClient is the Kitsu REST metadata provider.
type KitsuClient struct {
	net      *netclient.Client
	endpoint string
	log      *slog.Logger
}

// NewKitsuClient builds the provider; endpoint may be empty for the
// production default; logger may be nil.
func NewKitsuClient(net *netclient.Client, endpoint string, logger *slog.Logger) *KitsuClient {
	if endpoint == "" {
		endpoint = DefaultKitsuEndpoint
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &KitsuClient{net: net, endpoint: strings.TrimSuffix(endpoint, "/"), log: logger}
}

// ID identifies the provider in manager ordering.
func (c *KitsuClient) ID() string { return "kitsu" }

// SearchAlternativeTitles returns normalized title aliases from a Kitsu
// search (canonicalTitle + titles map + abbreviatedTitles), page limit 8.
func (c *KitsuClient) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	queryParams := url.Values{}
	queryParams.Set("filter[text]", query)
	queryParams.Set("page[limit]", strconv.Itoa(8))

	resp, err := c.net.Get(ctx, c.endpoint+"/anime?"+queryParams.Encode(), map[string]string{
		"Accept": "application/vnd.api+json",
	})
	if err != nil {
		return nil, fmt.Errorf("metadata kitsu: %w", err)
	}

	var reply kitsuResponse
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		return nil, fmt.Errorf("metadata kitsu: decode response: %w", err)
	}

	seen := map[string]bool{}
	var out []string
	add := func(title string) {
		normalized := normalizeTitle(title)
		if normalized == "" || seen[normalized] {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	for _, row := range reply.Data {
		add(row.Attributes.CanonicalTitle)
		for _, title := range row.Attributes.Titles {
			add(title)
		}
		for _, title := range row.Attributes.AbbreviatedTitles {
			add(title)
		}
	}
	return out, nil
}
