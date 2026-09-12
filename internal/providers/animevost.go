package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeVostBase is the JSON API root (anicli-py anicli/providers/
// animevost.py:17). Search and playlist are form-encoded POSTs.
const AnimeVostBase = "https://api.animevost.org/v1"

// formContentType marks a request body as form-encoded (the header
// PostForm used to set; kept explicit for Do-based calls).
var formContentType = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

// AnimeVost is the port of anicli-py anicli/providers/animevost.py.
// Source type BOTH, single fixed dub "AnimeVost", mp4 links carried in a
// JSON object raw embed between GetEpisodes and ResolveStream.
type AnimeVost struct {
	Base
}

// newAnimevost builds the provider against baseURL.
func newAnimevost(baseURL string, http *netclient.Client) *AnimeVost {
	return &AnimeVost{Base: Base{
		id:         "animevost",
		name:       "AnimeVost",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeBoth,
		http:       http,
	}}
}

// animevostSearch mirrors the fields consumed by animevost.py:29-36.
type animevostSearch struct {
	Data []struct {
		ID              json.Number `json:"id"`
		Title           string      `json:"title"`
		URLImagePreview string      `json:"urlImagePreview"`
	} `json:"data"`
}

// animevostPlaylistEntry mirrors the fields consumed by animevost.py:50-62.
type animevostPlaylistEntry struct {
	Name string `json:"name"`
	HD   string `json:"hd"`
	Std  string `json:"std"`
}

// Search POSTs {"name": query} to /search (anicli-py animevost.py:20-37).
// Decode failures return an empty result set — the Python original
// swallows them (except Exception: return []).
func (p *AnimeVost) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + "/search",
		Headers: formContentType,
		Body:    strings.NewReader(url.Values{"name": {query}}.Encode()),
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var data animevostSearch
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return []contracts.SearchResult{}, nil
	}

	results := make([]contracts.SearchResult, 0, len(data.Data))
	for _, item := range data.Data {
		results = append(results, contracts.SearchResult{
			Title:    item.Title,
			URL:      pythonStr(item.ID), // Python str(item.get("id")) → "None" when missing
			SourceID: p.ID(),
			Poster:   item.URLImagePreview,
		})
	}
	return results, nil
}

// GetEpisodes POSTs {"id": animeURL} to /playlist (anicli-py
// animevost.py:39-64). Episodes are numbered 1-based; the name falls back
// to the index string; links are stashed as one JSON object raw embed.
func (p *AnimeVost) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + "/playlist",
		Headers: formContentType,
		Body:    strings.NewReader(url.Values{"id": {animeURL}}.Encode()),
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	// Decode failures and {"error": ...} objects both yield an empty
	// list (animevost.py:43-48). Python decodes once into a generic
	// value and type-checks; here the array decode alone suffices —
	// a JSON body is either an array or an object, so an {"error":...}
	// response fails array-decoding and returns [] on the same path.
	var entries []animevostPlaylistEntry
	if jsonErr := json.Unmarshal(resp.Body, &entries); jsonErr != nil {
		return []contracts.Episode{}, nil
	}

	episodes := make([]contracts.Episode, 0, len(entries))
	for i, item := range entries {
		name := item.Name
		if name == "" {
			name = strconv.Itoa(i + 1)
		}
		links := map[string]string{}
		if item.HD != "" {
			links["hd"] = item.HD
		}
		if item.Std != "" {
			links["std"] = item.Std
		}
		payload, err := json.Marshal(links)
		if err != nil {
			// map[string]string is always marshalable; kept for honesty.
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
				fmt.Errorf("encode links for episode %d: %w", i+1, err))
		}

		idx := strconv.Itoa(i + 1)
		episodes = append(episodes, contracts.Episode{
			Num:   idx,
			Title: name,
			RawID: idx,
			RawEmbeds: map[string][]string{
				"AnimeVost": {string(payload)},
			},
		})
	}
	return episodes, nil
}

// ResolveStream decodes the JSON links payload: hd maps to quality 720
// and std to 480, both typed mp4 (anicli-py animevost.py:66-76). A
// missing dub defaults the payload to "{}" and yields an empty stream.
func (p *AnimeVost) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	raw := "{}"
	if embeds := episode.RawEmbeds[dubID]; len(embeds) > 0 {
		raw = embeds[0]
	}

	var links struct {
		HD  string `json:"hd"`
		Std string `json:"std"`
	}
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("decode links payload: %w", err))
	}
	if links.HD != "" {
		stream.Links["720"] = contracts.VideoSource{URL: links.HD, Quality: "720", Type: "mp4"}
	}
	if links.Std != "" {
		stream.Links["480"] = contracts.VideoSource{URL: links.Std, Quality: "480", Type: "mp4"}
	}
	return stream, nil
}
