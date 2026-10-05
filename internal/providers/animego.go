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

// AnimeGoBase is the site root. animego.org (and the animego.one
// mirror this port originally targeted) is dead; animego.me is the
// live AnimeGO continuation (same branding ©2017-2026, same
// /anime/{slug}-{id} URL scheme, kodik+aniboom player ecosystem —
// verified live 2026-09-18). PR48.
const AnimeGoBase = "https://animego.me"

// AnimeGo is the animego.me port of the animego provider. The site was
// rewritten since the animego.org era: a Turbo/Stimulus frontend whose
// data paths are HTML scraping of /search/anime, a JSON-wrapped HTML
// fragment at /player/{animeID} (episodes carousel + the first
// episode's provider buttons) and a JSON-wrapped HTML fragment at
// /player/videos/{episodeID} (one episode's provider buttons). The
// PR44 owner model survives unchanged: episode one keeps its real
// embed links, the rest carry the release's dub KEYS with EMPTY lists,
// hydrated lazily by FetchDubs.
type AnimeGo struct {
	Base
}

// newAnimego builds the provider against baseURL.
func newAnimego(baseURL string, http *netclient.Client) *AnimeGo {
	headers := map[string]string{
		"Referer":          baseURL,
		"X-Requested-With": "XMLHttpRequest",
		"Accept-Language":  "ru-RU",
	}
	return &AnimeGo{Base: Base{
		id:          "animego",
		name:        "AnimeGo",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		headers:     headers,
		http:        http,
	}}
}

// playerEnvelope is the site-wide JSON wrapper of every /player
// endpoint: the payload HTML rides data.content.
type playerEnvelope struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Content string `json:"content"`
	} `json:"data"`
}

// Search scrapes /search/anime (the original path still serves the
// full results page on animego.me). Items are `.ani-grid__item`
// blocks; the title link is `.ani-grid__item-title a[title]` and the
// poster the `.ani-grid__item-picture img[src]`. The site emits
// RELATIVE hrefs — they are absolutized against the provider base so
// GetEpisodes receives a fetchable URL.
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
	doc.Find(".ani-grid__item").Each(func(_ int, item *goquery.Selection) {
		titleNode := item.Find(".ani-grid__item-title a[title]").First()
		if titleNode.Length() == 0 {
			return
		}

		href, _ := titleNode.Attr("href")
		title, _ := titleNode.Attr("title")

		var poster string
		if thumb := item.Find(".ani-grid__item-picture img[src]").First(); thumb.Length() > 0 {
			poster, _ = thumb.Attr("src")
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      p.absoluteURL(href),
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// absoluteURL resolves a site-relative href against the provider base.
func (p *AnimeGo) absoluteURL(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	if !strings.HasPrefix(href, "/") {
		return href
	}
	return p.baseURL + href
}

// GetEpisodes loads the anime page, follows its player-loader URL
// (/player/{animeID}) and parses the returned fragment. Series pages
// yield one episode per carousel item (`[data-episode-number]`
// carrying `data-episode`); the same fragment's provider buttons
// describe episode one's streams, whose dub keys are distributed
// release-wide. Pages without a carousel are films: one episode whose
// embeds are parsed inline from the same fragment.
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

	loader := doc.Find("[data-anime-player-loader-url-value]").First()
	if loader.Length() == 0 {
		return []contracts.Episode{}, nil
	}
	loaderPath, _ := loader.Attr("data-anime-player-loader-url-value")
	animeID := pathID(loaderPath)
	if animeID == "" {
		return []contracts.Episode{}, nil
	}

	content, err := p.fetchPlayerFragment(ctx, p.baseURL+loaderPath)
	if err != nil {
		return nil, err
	}
	epDoc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse player content: %w", err))
	}

	var episodes []contracts.Episode
	epDoc.Find("[data-episode-number][data-episode]").Each(func(_ int, item *goquery.Selection) {
		num, _ := item.Attr("data-episode-number")
		epID, _ := item.Attr("data-episode")
		if num == "" || epID == "" {
			return
		}
		episodes = append(episodes, contracts.Episode{
			Num:       num,
			RawID:     epID,
			RawEmbeds: map[string][]string{},
		})
	})

	if len(episodes) > 0 {
		// Episode one's provider buttons ride the SAME fragment (zero
		// extra requests — the PR44 tier-1 fetch is free here); the
		// remaining episodes carry the dub keys with EMPTY lists.
		p.parseEmbeds(epDoc, &episodes[0])
		applyReleaseDubKeys(episodes)
		return episodes, nil
	}

	// Film path: no carousel — the fragment's provider buttons ARE the
	// film's embeds.
	film := contracts.Episode{
		Num:       "1",
		Title:     "Фильм",
		RawID:     animeID,
		RawEmbeds: map[string][]string{},
	}
	p.parseEmbeds(epDoc, &film)
	return []contracts.Episode{film}, nil
}

// fetchPlayerFragment GETs a /player endpoint and returns its
// data.content HTML.
func (p *AnimeGo) fetchPlayerFragment(ctx context.Context, playerURL string) (string, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     playerURL,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return "", err
	}

	var envelope playerEnvelope
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return "", contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode player response: %w", err))
	}
	return envelope.Data.Content, nil
}

// FetchDubs hydrates episode.RawEmbeds from /player/videos/{episodeID}.
// Episodes that already carry actual embed LINKS are returned untouched
// (release-scope dub KEYS with empty lists are exactly the state
// hydration exists to fill).
func (p *AnimeGo) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	if hasAnyEmbedLinks(episode.RawEmbeds) {
		return episode, nil
	}

	content, err := p.fetchPlayerFragment(ctx, p.baseURL+"/player/videos/"+episode.RawID)
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse videos content: %w", err))
	}

	p.parseEmbeds(doc, episode)
	return episode, nil
}

// parseEmbeds fills episode.RawEmbeds from a player fragment: provider
// buttons (`button[data-anime-player-target="provider"]`) carry the
// embed URL in data-player and the dubbing studio in
// data-translation-title (missing titles fall back to "Unknown").
func (p *AnimeGo) parseEmbeds(doc *goquery.Document, episode *contracts.Episode) {
	embeds := map[string][]string{}
	doc.Find(`button[data-anime-player-target="provider"]`).Each(func(_ int, item *goquery.Selection) {
		playerURL, _ := item.Attr("data-player")
		if playerURL == "" {
			return
		}
		if strings.HasPrefix(playerURL, "//") {
			playerURL = "https:" + playerURL
		}
		dubName, _ := item.Attr("data-translation-title")
		if dubName == "" {
			dubName = "Unknown"
		}
		embeds[dubName] = append(embeds[dubName], playerURL)
	})

	episode.RawEmbeds = embeds
}

// pathID extracts the trailing numeric id of a site-relative path
// ("/player/2115" → "2115").
func pathID(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return ""
	}
	id := path[idx+1:]
	for _, r := range id {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return id
}

// hasAnyEmbedLinks reports whether any dub key carries at least one
// link (release-scope KEYS with empty lists do not count — they are
// the tier-1 state hydration exists to fill).
func hasAnyEmbedLinks(embeds map[string][]string) bool {
	for _, links := range embeds {
		if len(links) > 0 {
			return true
		}
	}
	return false
}

// applyReleaseDubKeys distributes the FIRST episode's dub keys onto
// every other episode of the release as keys with EMPTY link lists
// (PR44 owner model: the dub-provider list is release-scoped — one
// request covers it — while the streams are per-episode and resolve
// on demand). Episodes already carrying a key keep it untouched.
// PR122: relocated from the deleted anilib.go — animego is the sole
// remaining consumer (the anilib Lua script implements the same
// distribution inside its episodes()).
func applyReleaseDubKeys(episodes []contracts.Episode) {
	if len(episodes) == 0 {
		return
	}
	first := episodes[0].RawEmbeds
	for i := 1; i < len(episodes); i++ {
		if episodes[i].RawEmbeds == nil {
			episodes[i].RawEmbeds = map[string][]string{}
		}
		for dub := range first {
			if _, ok := episodes[i].RawEmbeds[dub]; !ok {
				episodes[i].RawEmbeds[dub] = []string{}
			}
		}
	}
}

// ResolveStream resolves the embed URLs of the chosen dub through the
// extractor factory; direct media URLs resolve via the factory
// fallback.
//
// PR44 owner model: a known-but-empty dub self-hydrates that ONE
// episode here (a single /player/videos request) — resolving never
// runs bulk. An unknown dub key hydrates nothing.
func (p *AnimeGo) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	if links, ok := episode.RawEmbeds[dubID]; ok && len(links) == 0 {
		if _, err := p.FetchDubs(ctx, &episode); err != nil {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
		}
	}

	sources, err := resolveEmbeds(ctx, p.http, episode.RawEmbeds[dubID])
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}
