package skip

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// DefaultAnimeSkipEndpoint is the Anime Skip GraphQL root
// (anime-skip.com/docs/api).
const DefaultAnimeSkipEndpoint = "https://api.anime-skip.com/api/query"

// Classification thresholds ported from python skip_manager.py.
const (
	// opClassifyMaxStartSeconds bounds the untyped-interval heuristic:
	// a neutral interval starting before it is an opening.
	opClassifyMaxStartSeconds = 360.0
)

// animeSkipQueries is the fallback chain of query shapes (python
// get_chapters_bundle): the endpoint schema has drifted over time, so
// three shapes are tried in order until one yields timestamps.
var animeSkipQueries = []string{
	`query($malId: Int!, $episodeNumber: Float!) {
              episodeByMalId(malId: $malId, episodeNumber: $episodeNumber) {
                timestamps { skipType type startTime endTime from to }
              }
            }`,
	`query($malId: Int!, $episodeNumber: Float!) {
              findEpisodeByMalId(malId: $malId, episodeNumber: $episodeNumber) {
                timestamps { skipType type startTime endTime from to }
                skipTimes { skipType type startTime endTime from to }
              }
            }`,
	`query($malId: Int!, $episodeNumber: Float!) {
              episode(malId: $malId, episodeNumber: $episodeNumber) {
                timestamps { skipType type startTime endTime from to }
              }
            }`,
}

// AnimeSkipOptions customizes the Anime Skip GraphQL client.
type AnimeSkipOptions struct {
	// Endpoint overrides the GraphQL root (tests point this at
	// httptest).
	Endpoint string
	// ClientID is the X-Client-ID header value.
	ClientID string
}

// AnimeSkipClient resolves episode timestamps from the Anime Skip
// GraphQL API (ported from anicli-py skip_manager.AnimeSkipApiClient).
type AnimeSkipClient struct {
	http HTTP
	opts AnimeSkipOptions
}

// NewAnimeSkipClient builds the client; a zero Endpoint falls back to
// the default.
func NewAnimeSkipClient(http HTTP, opts AnimeSkipOptions) *AnimeSkipClient {
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultAnimeSkipEndpoint
	}
	return &AnimeSkipClient{http: http, opts: opts}
}

// ID implements Provider.
func (c *AnimeSkipClient) ID() string { return ProviderAnimeSkip }

// graphqlBody is the GraphQL POST payload.
type graphqlBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// GetSkipTimes walks the query-shape fallback chain and returns the
// deduplicated op/ed intervals. A valid-but-empty answer across all
// shapes is an empty slice with a nil error; transport, HTTP and decode
// failures on every shape surface as an error.
func (c *AnimeSkipClient) GetSkipTimes(ctx context.Context, shikimoriID int64, episodeNum float64) ([]Interval, error) {
	headers := map[string]string{"X-Client-ID": c.opts.ClientID}
	variables := map[string]any{
		"malId":         shikimoriID,
		"episodeNumber": episodeNum,
	}

	var lastErr error
	for _, query := range animeSkipQueries {
		body := graphqlBody{Query: query, Variables: variables}

		resp, err := c.http.PostJSON(ctx, c.opts.Endpoint, body, headers)
		if err != nil {
			lastErr = fmt.Errorf("animeskip graphql post: %w", err)
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal(resp.Body, &payload); err != nil {
			lastErr = fmt.Errorf("animeskip decode response: %w", err)
			continue
		}

		intervals := collectGraphQLIntervals(payload["data"])
		deduped := dedupeAnimeIntervals(intervals)
		if len(deduped) > 0 {
			return deduped, nil
		}
		// Valid answer, no timestamps: keep walking the chain.
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return []Interval{}, nil
}

// collectGraphQLIntervals recursively extracts timestamp-like intervals
// from a GraphQL payload subtree (python _collect_intervals_recursive):
// start/end may arrive as startTime/from/start and endTime/to/end, and
// types classify through skipType/type/label with a start-time
// heuristic for neutral values.
func collectGraphQLIntervals(node any) []Interval {
	var out []Interval
	collectGraphQLNode(node, &out)
	return out
}

func collectGraphQLNode(node any, out *[]Interval) {
	switch typed := node.(type) {
	case map[string]any:
		start := numericValue(typed, "startTime", "from", "start")
		end := numericValue(typed, "endTime", "to", "end")
		if start != nil && end != nil && *end > *start {
			rawType := strings.ToLower(strings.TrimSpace(stringValue(typed, "skipType", "type", "label")))
			switch {
			case strings.Contains(rawType, "op") || strings.Contains(rawType, "opening") || rawType == "intro":
				*out = append(*out, Interval{SkipType: "op", StartTime: *start, EndTime: *end})
			case strings.Contains(rawType, "ed") || strings.Contains(rawType, "ending") || strings.Contains(rawType, "credit"):
				*out = append(*out, Interval{SkipType: "ed", StartTime: *start, EndTime: *end})
			default:
				// Neutral type: don't discard useful timings.
				skipType := "ed"
				if *start < opClassifyMaxStartSeconds {
					skipType = "op"
				}
				*out = append(*out, Interval{SkipType: skipType, StartTime: *start, EndTime: *end})
			}
		}
		for _, value := range typed {
			collectGraphQLNode(value, out)
		}
	case []any:
		for _, item := range typed {
			collectGraphQLNode(item, out)
		}
	}
}

// numericValue returns the first numeric field among names (json
// numbers only; python also tolerated numeric strings, kept for
// parity).
func numericValue(node map[string]any, names ...string) *float64 {
	for _, name := range names {
		value, ok := node[name]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			v := typed
			return &v
		case string:
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
				return &parsed
			}
		}
	}
	return nil
}

// stringValue returns the first non-empty string field among names.
func stringValue(node map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := node[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// dedupeAnimeIntervals normalizes the collected set (python
// deduped): only op/ed survive, end must exceed start and identical
// (type, start-ms, end-ms) triples collapse.
func dedupeAnimeIntervals(intervals []Interval) []Interval {
	type key struct {
		skipType string
		startMS  int64
		endMS    int64
	}

	seen := make(map[key]Interval, len(intervals))
	order := make([]key, 0, len(intervals))
	for _, iv := range intervals {
		if iv.SkipType != "op" && iv.SkipType != "ed" {
			continue
		}
		if iv.EndTime <= iv.StartTime {
			continue
		}
		k := key{skipType: iv.SkipType, startMS: int64(iv.StartTime * 1000), endMS: int64(iv.EndTime * 1000)}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = iv
		order = append(order, k)
	}

	out := make([]Interval, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartTime != out[j].StartTime {
			return out[i].StartTime < out[j].StartTime
		}
		return out[i].SkipType < out[j].SkipType
	})
	return out
}
