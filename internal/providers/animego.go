package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeGoBase is the site root (anicli-py anicli/providers/animego.py:14).
const AnimeGoBase = "https://animego.one"

// AnimeGo is the port of anicli-py anicli/providers/animego.py: HTML
// scraping via goquery (Python used selectolax; selectors are ported
// verbatim), a /player and /anime/series JSON-API pair returning HTML
// fragments inside JSON, and lazy dub hydration via FetchDubs.
type AnimeGo struct {
	Base
}

// newAnimego builds the provider against baseURL.
//
// The Python header set (animego.py:20-25) re-asserts the configured
// user agent; the netclient already sends cfg.UserAgent on every
// request, so it is not duplicated here.
func newAnimego(baseURL string, http *netclient.Client) *AnimeGo {
	headers := map[string]string{
		"Referer":          baseURL,
		"X-Requested-With": "XMLHttpRequest",
		"Accept-Language":  "ru-RU",
	}
	return &AnimeGo{Base: Base{
		id:         "animego",
		name:       "AnimeGo",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeBoth,
		headers:    headers,
		http:       http,
	}}
}

// Search scrapes /search/anime (anicli-py animego.py:27-50). Selector
// logic is a verbatim port: `.row > .col-ul-2` items, `.text-truncate
// a[title]` for the title/href, `.lazy[data-original]` for the poster.
func (p *AnimeGo) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("q", query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/search/anime?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	var results []contracts.SearchResult
	doc.Find(".row > .col-ul-2").Each(func(_ int, item *goquery.Selection) {
		titleNode := item.Find(".text-truncate a[title]").First()
		if titleNode.Length() == 0 {
			return
		}

		link, _ := titleNode.Attr("href")
		title, _ := titleNode.Attr("title")

		var poster string
		if thumb := item.Find(".lazy[data-original]").First(); thumb.Length() > 0 {
			poster, _ = thumb.Attr("data-original")
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes scrapes the anime page for the numeric id, then the player
// API fragment (anicli-py animego.py:52-91). Series pages yield one
// episode per `#video-carousel .mb-0` entry; anything else is treated as
// a film with a single episode whose embeds are parsed inline.
func (p *AnimeGo) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     animeURL,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	idNode := doc.Find(".br-2 .my-list-anime").First()
	if idNode.Length() == 0 {
		return []contracts.Episode{}, nil
	}
	rawID, _ := idNode.Attr("id")
	animeID := strings.TrimPrefix(rawID, "my-list-")
	if animeID == "" {
		return []contracts.Episode{}, nil
	}

	playerURL := fmt.Sprintf("%s/anime/%s/player?_allow=true", p.baseURL, animeID)
	playerResp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     playerURL,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var player struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(playerResp.Body, &player); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, playerResp.StatusCode,
			fmt.Errorf("decode player response: %w", err))
	}

	epDoc, err := goquery.NewDocumentFromReader(strings.NewReader(player.Content))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse player content: %w", err))
	}

	var episodes []contracts.Episode
	if carousel := epDoc.Find("#video-carousel"); carousel.Length() > 0 {
		carousel.Find(".mb-0").Each(func(_ int, item *goquery.Selection) {
			num, _ := item.Attr("data-episode")
			epID, _ := item.Attr("data-id")
			title, _ := item.Attr("data-episode-title")

			if num == "" || epID == "" {
				return
			}
			episodes = append(episodes, contracts.Episode{
				Num:       num,
				Title:     title,
				RawID:     epID,
				RawEmbeds: map[string][]string{},
			})
		})
		return episodes, nil
	}

	// Film path (animego.py:82-90).
	film := contracts.Episode{
		Num:       "1",
		Title:     "Фильм",
		RawID:     animeID,
		RawEmbeds: map[string][]string{},
	}
	p.parseEmbeds(epDoc, &film)
	return []contracts.Episode{film}, nil
}

// FetchDubs hydrates episode.RawEmbeds from /anime/series (port of
// animego.py:93-105 fetch_dubs_for_episode). Episodes that already carry
// embeds are returned untouched (the Python guard at animego.py:94).
func (p *AnimeGo) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	if len(episode.RawEmbeds) > 0 {
		return episode, nil
	}

	params := url.Values{}
	params.Set("id", episode.RawID)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/anime/series?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var series struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(resp.Body, &series); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode series response: %w", err))
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(series.Content))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse series content: %w", err))
	}

	p.parseEmbeds(doc, episode)
	return episode, nil
}

// parseEmbeds fills episode.RawEmbeds from a dubbing/players fragment
// (port of animego.py:107-129 _parse_embeds): `#video-dubbing .mb-1`
// maps data-dubbing ids to names; `#video-players > .mb-1` (falling back
// to `#video-players > span`) entries carry data-player URLs tagged with
// data-provide-dubbing.
func (p *AnimeGo) parseEmbeds(doc *goquery.Document, episode *contracts.Episode) {
	dubbers := map[string]string{}
	doc.Find("#video-dubbing .mb-1").Each(func(_ int, item *goquery.Selection) {
		id, _ := item.Attr("data-dubbing")
		if id == "" {
			return
		}
		dubbers[id] = strings.TrimSpace(item.Text())
	})

	embeds := map[string][]string{}
	nodes := doc.Find("#video-players > .mb-1")
	if nodes.Length() == 0 {
		nodes = doc.Find("#video-players > span")
	}
	nodes.Each(func(_ int, item *goquery.Selection) {
		playerURL, _ := item.Attr("data-player")
		dubID, _ := item.Attr("data-provide-dubbing")

		if playerURL == "" || dubID == "" {
			return
		}
		if strings.HasPrefix(playerURL, "//") {
			playerURL = "https:" + playerURL
		}
		dubName := dubbers[dubID]
		if dubName == "" {
			dubName = "Unknown" // Python dubbers.get(dub_id, "Unknown")
		}
		embeds[dubName] = append(embeds[dubName], playerURL)
	})

	episode.RawEmbeds = embeds
}

// ResolveStream resolves the embed URLs of the chosen dub (port of
// animego.py:131-138) through the extractor factory; direct media URLs
// resolve via the factory fallback.
func (p *AnimeGo) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	sources, err := resolveEmbeds(ctx, p.http, episode.RawEmbeds[dubID])
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}
