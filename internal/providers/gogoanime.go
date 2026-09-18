package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// GogoAnimeBase is the site root [LIVE-VERIFIED 2026-09-18]. The
// gogoanime platform rebranded to Anitaku (Kohi-den extensions-source
// issue #410): anitaku.io is the live domain, running the same
// WordPress/dramastream family the previous gogoanime.by target used.
// The by-domain is a decaying shell (stale search index, churned slugs,
// dead mirrors), so the provider targets the rebrand.
const GogoAnimeBase = "https://anitaku.io"

// gogoSearchAjax is the dramastream search endpoint. [LIVE-VERIFIED
// 2026-09-18] the WordPress /?s= form 301s to /browse/ on anitaku.io;
// the live search is POST admin-ajax with action=ts_ac_do_search — the
// GET form silently ignores ts_ac_query and answers with recent posts.
const gogoSearchAjax = "/wp-admin/admin-ajax.php"

// GogoAnime serves the anitaku.io WordPress platform: a POST admin-ajax
// search, episode lists rendered in each /series/ page's .eplister, and
// per-episode mirror lists stored as base64-encoded iframe HTML in the
// select.mirror options (the theme does atob(value) into #pembed).
// Mirror URLs run through the extractor factory. All shapes verified
// live 2026-09-18.
type GogoAnime struct {
	Base
}

// newGogoAnime builds the provider against baseURL. The netclient already
// sends a user agent on every request; the Referer is a PR5 task ruling
// kept for the embed-heavy site.
func newGogoAnime(baseURL string, http *netclient.Client) *GogoAnime {
	return &GogoAnime{
		Base: Base{
			id:          "gogoanime",
			name:        "GogoAnime",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			headers:     map[string]string{"Referer": baseURL},
			http:        http,
		},
	}
}

// gogoSearchResponse mirrors the ts_ac_do_search answer [LIVE-VERIFIED
// 2026-09-18]: {"series":[{"all":[{post_title, post_image, post_link,
// …}]}]} — the empty-result case is all:[] plus a render template.
type gogoSearchResponse struct {
	Series []struct {
		All []struct {
			Title string `json:"post_title"`
			Image string `json:"post_image"`
			Link  string `json:"post_link"`
		} `json:"all"`
	} `json:"series"`
}

// Search runs the Anitaku search ajax [LIVE-VERIFIED 2026-09-18]:
// POST {base}/wp-admin/admin-ajax.php with form fields
// action=ts_ac_do_search, ts_ac_query=<query>. Every series.all group
// contributes its entries; result URLs stay the absolute /series/ hrefs
// the site emits, verbatim.
func (p *GogoAnime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{}
	form.Set("action", "ts_ac_do_search")
	form.Set("ts_ac_query", query)

	headers := map[string]string{
		"Referer":      p.baseURL,
		"Content-Type": "application/x-www-form-urlencoded",
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + gogoSearchAjax,
		Headers: headers,
		Body:    strings.NewReader(form.Encode()),
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var data gogoSearchResponse
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", err))
	}

	var results []contracts.SearchResult
	for _, group := range data.Series {
		for _, item := range group.All {
			if item.Link == "" {
				continue
			}
			results = append(results, contracts.SearchResult{
				Title:    item.Title,
				URL:      item.Link,
				SourceID: p.ID(),
				Poster:   item.Image,
			})
		}
	}
	return results, nil
}

// GetEpisodes lists the server-rendered .eplister grid of a series page
// [LIVE-VERIFIED 2026-09-18: the live One Piece page renders the
// newest ~84 .eplister li entries — 1178 … 1100 — newest-first; the
// site exposes no older-episode ajax]. The provider reverses to
// ascending, the order the legacy ajax path produced. RawID is the
// absolute episode URL.
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
			fmt.Errorf("parse series page: %w", err))
	}

	var episodes []contracts.Episode
	doc.Find(".eplister li").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a").First()
		if linkNode.Length() == 0 {
			return
		}
		href, _ := linkNode.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" {
			return
		}

		num := strings.TrimSpace(item.Find(".epl-num").First().Text())
		if num == "" {
			num = "0"
		}

		title := strings.TrimSpace(item.Find(".epl-title").First().Text())
		if title == "" {
			title = "Episode " + num
		}

		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     title,
			RawID:     href,
			RawEmbeds: map[string][]string{},
		})
	})

	// The grid renders newest-first; flip to ascending.
	for i, j := 0, len(episodes)-1; i < j; i, j = i+1, j-1 {
		episodes[i], episodes[j] = episodes[j], episodes[i]
	}
	return episodes, nil
}

// FetchDubs hydrates episode.RawEmbeds from the episode page's
// select.mirror server list [LIVE-VERIFIED 2026-09-18: each option
// stores one server as base64-encoded iframe HTML in its value
// attribute — the theme's loadMi does atob(value) into #pembed; the
// leading "Select Video Server" placeholder carries an empty value and
// is skipped, as are options that do not decode to an iframe src].
// Option labels are blank on the live pages, so servers land under the
// "Unknown" slot, matching the previous convention for unlabeled
// servers.
func (p *GogoAnime) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	episodeURL := episode.RawID
	if !strings.HasPrefix(episodeURL, "http") {
		episodeURL = p.baseURL + episodeURL
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     episodeURL,
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
	doc.Find("select.mirror option[value]").Each(func(_ int, item *goquery.Selection) {
		encoded, _ := item.Attr("value")
		if encoded == "" {
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return
		}
		frame, err := goquery.NewDocumentFromReader(bytes.NewReader(decoded))
		if err != nil {
			return
		}
		videoURL, ok := frame.Find("iframe").First().Attr("src")
		if !ok {
			return
		}
		videoURL = strings.TrimSpace(videoURL)
		if videoURL == "" {
			return
		}

		serverName := strings.TrimSpace(item.Text())
		if serverName == "" {
			serverName = "Unknown"
		}
		embeds[serverName] = append(embeds[serverName], videoURL)
	})

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream resolves the chosen server's mirrors onto media URLs
// through the extractor factory [LIVE-VERIFIED 2026-09-18: anitaku.io
// mirrors are direct embed URLs — the gogoanime.by referer-gated
// /player/ proxy no longer exists]. Live mirrors are megacloud- and
// blogger-family embeds; classic gogo hosts (streaming.php et al.)
// resolve through the gogoplay encrypt-ajax extractor whenever the
// site serves them again.
//
// Like the Python original (gogoanime.py:122-134) the dub list is
// fetched lazily when the episode carries none, and links merge with
// dict.update semantics; an extraction failure surfaces only when no
// link resolved anything.
func (p *GogoAnime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if len(episode.RawEmbeds) == 0 {
		hydrated, err := p.FetchDubs(ctx, &episode)
		if err != nil {
			return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
		}
		episode = *hydrated
	}

	links := episode.RawEmbeds[dubID]
	if len(links) == 0 {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("dub %q carries no mirrors to resolve", dubID))
	}

	sources, err := p.factorySources(ctx, links...)
	if err != nil {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
	}
	return contracts.MediaStream{DubName: dubID, Links: sources}, nil
}

// factorySources runs URLs through the extractor factory and converts a
// nothing-resolved outcome into a typed error (the factory itself mirrors
// the Python {} for unmatched hosts).
func (p *GogoAnime) factorySources(ctx context.Context, links ...string) (map[string]contracts.VideoSource, error) {
	sources, err := resolveEmbeds(ctx, p.http, links)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	if len(sources) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("no sources extracted from %s", strings.Join(links, ", ")))
	}
	return sources, nil
}
