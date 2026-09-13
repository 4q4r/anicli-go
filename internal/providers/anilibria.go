package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniLibria production endpoints. The API host differs from the site host:
// streams must carry the site host as Referer (anicli-py
// anicli/providers/anilibria.py:17-18).
const (
	// AniLibriaAPIBase is the JSON API root (aniliberty.top).
	AniLibriaAPIBase = "https://aniliberty.top/api/v1"
	// AniLibriaHost is the site root used as the stream Referer.
	AniLibriaHost = "https://anilibria.top"
)

// AniLibria is the port of anicli-py anicli/providers/anilibria.py.
// Source type BOTH, single fixed dub "AniLibria", HLS qualities carried
// inside a hls_json: raw-embed payload between GetEpisodes and
// ResolveStream (verbatim from the Python original).
type AniLibria struct {
	Base
	hostURL string
}

// newAnilibria builds the provider against apiBase with hostURL used as
// the stream Referer.
func newAnilibria(apiBase, hostURL string, http *netclient.Client) *AniLibria {
	return &AniLibria{
		Base: Base{
			id:          "anilibria",
			name:        "AniLibria",
			baseURL:     apiBase,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		hostURL: hostURL,
	}
}

// anilibriaSearchItem mirrors the fields consumed by anilibria.py:27-33.
type anilibriaSearchItem struct {
	ID    json.Number `json:"id"`
	Alias string      `json:"alias"`
	Name  struct {
		Main string `json:"main"`
	} `json:"name"`
}

// anilibriaRelease mirrors the fields consumed by anilibria.py:41-56.
type anilibriaRelease struct {
	Episodes []struct {
		ID      json.Number `json:"id"`
		Ordinal json.Number `json:"ordinal"`
		HLS1080 string      `json:"hls_1080"`
		HLS720  string      `json:"hls_720"`
		HLS480  string      `json:"hls_480"`
	} `json:"episodes"`
}

// Search queries the release search endpoint (anicli-py anilibria.py:21-34).
// Divergence from Python: the query is URL-encoded here; the Python
// original interpolated it raw into the f-string URL (task ruling).
func (p *AniLibria) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	searchURL := p.baseURL + "/app/search/releases?query=" + url.QueryEscape(query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var items []anilibriaSearchItem
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(items))
	for _, item := range items {
		title := item.Name.Main
		if title == "" {
			// Python: item.get("name", {}).get("main", "Unknown").
			title = "Unknown"
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      item.Alias,
			SourceID: p.ID(),
			Meta:     map[string]any{"id": item.ID},
		})
	}
	return results, nil
}

// GetEpisodes lists episodes of a release by its alias (anicli-py
// anilibria.py:36-58). Quality links are stashed as one hls_json: raw
// embed per episode.
//
// Divergence from Python (cosmetic): json.dumps inserts ", " separators
// and preserves insertion order; Go json.Marshal emits compact output
// with sorted keys. Only ResolveStream consumes the payload and JSON
// object key order is not semantically load-bearing there.
func (p *AniLibria) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	releaseURL := p.baseURL + "/anime/releases/" + animeURL

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    releaseURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var release anilibriaRelease
	if err := json.Unmarshal(resp.Body, &release); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode release response: %w", err))
	}

	episodes := make([]contracts.Episode, 0, len(release.Episodes))
	for _, ep := range release.Episodes {
		links := map[string]string{}
		if ep.HLS1080 != "" {
			links["1080"] = ep.HLS1080
		}
		if ep.HLS720 != "" {
			links["720"] = ep.HLS720
		}
		if ep.HLS480 != "" {
			links["480"] = ep.HLS480
		}
		payload, err := json.Marshal(links)
		if err != nil {
			// map[string]string is always marshalable; kept for honesty.
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
				fmt.Errorf("encode hls links for episode %s: %w", ep.ID.String(), err))
		}

		episodes = append(episodes, contracts.Episode{
			Num:   pythonStr(ep.Ordinal),
			RawID: pythonStr(ep.ID),
			RawEmbeds: map[string][]string{
				"AniLibria": {"hls_json:" + string(payload)},
			},
		})
	}
	return episodes, nil
}

// ResolveStream decodes the hls_json payload stashed by GetEpisodes for
// the chosen dub (anicli-py anilibria.py:60-78). Protocol-relative and
// other non-http URLs gain an "https:" prefix, matching the Python
// string concatenation.
func (p *AniLibria) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	for _, raw := range episode.RawEmbeds[dubID] {
		payload, ok := strings.CutPrefix(raw, "hls_json:")
		if !ok {
			continue
		}
		var links map[string]string
		if err := json.Unmarshal([]byte(payload), &links); err != nil {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("decode hls_json payload: %w", err))
		}
		for quality, u := range links {
			if !strings.HasPrefix(u, "http") {
				u = "https:" + u
			}
			stream.Links[quality] = contracts.VideoSource{
				URL:     u,
				Quality: quality,
				Headers: map[string]string{"Referer": p.hostURL},
			}
		}
	}
	return stream, nil
}
