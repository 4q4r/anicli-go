package providers

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// GogoAnimeBase is the site root (anicli-py anicli/providers/
// gogoanime.py:13; domain re-verified 2026-09-12).
const GogoAnimeBase = "https://gogoanime3.co"

// GogoAnimeAjaxBase is the episode-list AJAX endpoint (gogoanime.py:71).
const GogoAnimeAjaxBase = "https://ajax.gogo-load.com/ajax/load-list-episode"

// GogoAnime is the port of anicli-py anicli/providers/gogoanime.py:
// HTML search, an AJAX episode listing keyed by the hidden #movie_id /
// #alias_anime form fields, and lazy per-episode server ("dub") hydration
// off the .anime_muti_link block.
type GogoAnime struct {
	Base

	// ajaxBase is the load-list-episode endpoint root.
	ajaxBase string
}

// newGogoAnime builds the provider against baseURL and the AJAX endpoint
// root.
//
// The Python header set (gogoanime.py:19) re-asserts only the configured
// user agent, which the netclient already sends on every request; the
// Referer is a PR5 task ruling (the embed-heavy site and its gogoplay
// players expect one).
func newGogoAnime(baseURL, ajaxBase string, http *netclient.Client) *GogoAnime {
	return &GogoAnime{
		Base: Base{
			id:         "gogoanime",
			name:       "GogoAnime",
			baseURL:    baseURL,
			sourceType: contracts.SourceTypeVideo,
			headers:    map[string]string{"Referer": baseURL},
			http:       http,
		},
		ajaxBase: ajaxBase,
	}
}

// Search scrapes /search.html?keyword=<query> (anicli-py
// gogoanime.py:21-42). Result URLs stay the relative /category/ hrefs the
// site emits, verbatim like the Python original.
func (p *GogoAnime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("keyword", query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/search.html?" + params.Encode(),
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
	doc.Find(".last_episodes li").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a").First()
		if linkNode.Length() == 0 {
			return
		}

		title, _ := linkNode.Attr("title")
		href, _ := linkNode.Attr("href")

		var poster string
		if img := item.Find("img").First(); img.Length() > 0 {
			poster, _ = img.Attr("src")
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      href,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes resolves the hidden movie id and alias off the anime page,
// then scrapes the AJAX episode list (anicli-py gogoanime.py:44-95).
// The ajax order is reversed verbatim (no numeric sort), and dubs are NOT
// hydrated here: raw embeds stay empty until FetchDubs/ResolveStream.
func (p *GogoAnime) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	if !strings.HasPrefix(animeURL, "http") {
		animeURL = p.baseURL + animeURL
	}

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

	movieIDNode := doc.Find("#movie_id").First()
	if movieIDNode.Length() == 0 {
		return []contracts.Episode{}, nil
	}
	movieID, _ := movieIDNode.Attr("value")

	alias := ""
	if aliasNode := doc.Find("#alias_anime").First(); aliasNode.Length() > 0 {
		alias, _ = aliasNode.Attr("value")
	}

	params := url.Values{}
	params.Set("ep_start", "0")
	params.Set("ep_end", "10000")
	// Python params={"id": None} makes requests DROP the parameter; an
	// empty value must not become a bare "id=" either.
	if movieID != "" {
		params.Set("id", movieID)
	}
	params.Set("default_ep", "0")
	params.Set("alias", alias)

	ajaxResp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.ajaxBase + "?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	epDoc, err := goquery.NewDocumentFromReader(bytes.NewReader(ajaxResp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, ajaxResp.StatusCode,
			fmt.Errorf("parse episode list: %w", err))
	}

	var episodes []contracts.Episode
	epDoc.Find("li").Each(func(_ int, li *goquery.Selection) {
		aNode := li.Find("a").First()
		if aNode.Length() == 0 {
			return
		}
		href, _ := aNode.Attr("href")
		if href == "" {
			// Python crashes on href.strip() over None here; the port
			// skips the broken entry instead (documented guard).
			return
		}
		href = strings.TrimSpace(href)

		num := "0" // Python fallback when the .name node is missing
		if name := aNode.Find(".name").First(); name.Length() > 0 {
			num = strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(name.Text()), "EP", ""))
		}

		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     "Episode " + num,
			RawID:     href,
			RawEmbeds: map[string][]string{},
		})
	})

	// Python returns episodes[::-1]: the ajax list is newest-first.
	for i, j := 0, len(episodes)-1; i < j; i, j = i+1, j-1 {
		episodes[i], episodes[j] = episodes[j], episodes[i]
	}
	return episodes, nil
}

// FetchDubs hydrates episode.RawEmbeds from the episode page's
// .anime_muti_link server list (port of gogoanime.py:97-120). Server
// names are the anchor texts with the "Choose this server" label
// stripped; protocol-relative data-video values gain the https: scheme.
func (p *GogoAnime) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	// Python builds base_url + raw_id unconditionally (gogoanime.py:99);
	// the ajax hrefs are always relative slugs.
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + episode.RawID,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse episode page: %w", err))
	}

	embeds := map[string][]string{}
	doc.Find(".anime_muti_link a").Each(func(_ int, item *goquery.Selection) {
		serverName := strings.TrimSpace(
			strings.ReplaceAll(item.Text(), "Choose this server", ""))
		videoURL, _ := item.Attr("data-video")
		if videoURL == "" {
			return
		}
		if !strings.HasPrefix(videoURL, "http") {
			videoURL = "https:" + videoURL
		}
		embeds[serverName] = append(embeds[serverName], videoURL)
	})

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream resolves the chosen server's embed URLs (port of
// gogoanime.py:122-134). Like the Python original it lazily fetches the
// dub list when the episode carries none; embed URLs run through the
// extractor factory, direct media URLs through the fallback.
func (p *GogoAnime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if len(episode.RawEmbeds) == 0 {
		hydrated, err := p.FetchDubs(ctx, &episode)
		if err != nil {
			return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
		}
		episode = *hydrated
	}

	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}
	links := episode.RawEmbeds[dubID]

	sources, err := resolveEmbeds(ctx, p.http, links)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}
