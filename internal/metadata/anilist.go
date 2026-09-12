package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// DefaultAniListEndpoint is the AniList GraphQL entry point.
const DefaultAniListEndpoint = "https://graphql.anilist.co"

// anilistGraphQL is the alias query, ported verbatim from the Python
// original (page 1, perPage 8 set at call time).
const anilistGraphQL = `query($search: String!, $page: Int, $perPage: Int) {
  Page(page: $page, perPage: $perPage) {
    media(search: $search, type: ANIME) {
      title { romaji english native }
      synonyms
    }
  }
}`

// anilistResponse models the interesting slice of the GraphQL reply.
type anilistResponse struct {
	Data struct {
		Page struct {
			Media []struct {
				Title struct {
					Romaji  string `json:"romaji"`
					English string `json:"english"`
					Native  string `json:"native"`
				} `json:"title"`
				Synonyms []string `json:"synonyms"`
			} `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

// AniListClient is the AniList GraphQL metadata provider.
type AniListClient struct {
	net      *netclient.Client
	endpoint string
	log      *slog.Logger
}

// NewAniListClient builds the provider; endpoint may be empty for the
// production default; logger may be nil.
func NewAniListClient(net *netclient.Client, endpoint string, logger *slog.Logger) *AniListClient {
	if endpoint == "" {
		endpoint = DefaultAniListEndpoint
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AniListClient{net: net, endpoint: endpoint, log: logger}
}

// ID identifies the provider in manager ordering.
func (c *AniListClient) ID() string { return "anilist" }

// SearchAlternativeTitles returns normalized title aliases from an AniList
// search. A GraphQL reply without data yields an empty result (python
// returned set()); transport and decode failures return errors that the
// manager treats as provider-unavailable.
func (c *AniListClient) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	payload := map[string]any{
		"query": anilistGraphQL,
		"variables": map[string]any{
			"search":  query,
			"page":    1,
			"perPage": 8,
		},
	}

	resp, err := c.net.PostJSON(ctx, c.endpoint, payload, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return nil, fmt.Errorf("metadata anilist: %w", err)
	}

	var reply anilistResponse
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		return nil, fmt.Errorf("metadata anilist: decode response: %w", err)
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
	for _, media := range reply.Data.Page.Media {
		add(media.Title.Romaji)
		add(media.Title.English)
		add(media.Title.Native)
		for _, synonym := range media.Synonyms {
			add(synonym)
		}
	}
	return out, nil
}
