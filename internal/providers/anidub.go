package providers

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnidubBase is the site root [LIVE-VERIFIED 2026-09-13].
const AnidubBase = "https://online.anidub.com"

// AniDUB is the online.anidub.com provider (Russian dub). Unlike the
// other roster members it is not a port of a frozen anicli-py source:
// the site was characterized live on 2026-09-13 and the provider was
// written against the observed DLE shapes. Search hits the DLE search
// form (still server-side rendered here, unlike sameband); episodes
// come from the per-episode sibnet embeds of the backup player tab and
// resolve through the shared extractor factory.
type AniDUB struct {
	Base
}

// newAnidub builds the provider against baseURL. The site answers
// plain desktop-UA requests (verified via curl), so no extra headers
// are sent.
func newAnidub(baseURL string, http *netclient.Client) *AniDUB {
	return &AniDUB{Base: Base{
		id:         "anidub",
		name:       "AniDUB",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeVideo,
		http:       http,
	}}
}

// Search GETs the DLE search form and scrapes the result cards
// [LIVE-VERIFIED 2026-09-13: GET
// /?do=search&subaction=search&story=naruto → HTTP 200, 13 cards;
// story=наруто → 14 cards; junk query → 200 with zero cards. Results
// render server-side inside .sect-content.sect-items using the same
// .th-item card template the paginated catalog (/page/N/, 94 pages)
// uses — scoped so the top owl-slider .th-item cards (ongoings) never
// leak in]. The query is percent-encoded with pyQuote (spaces %20).
// The server does the matching; no client-side filter.
func (p *AniDUB) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	searchURL := p.baseURL + "/?do=search&subaction=search&story=" + pyQuote(query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
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
	doc.Find(".sect-items .th-item").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a.th-in[href]").First()
		titleNode := item.Find(".th-title").First()
		if linkNode.Length() == 0 || titleNode.Length() == 0 {
			return
		}

		link, _ := linkNode.Attr("href")
		if link == "" {
			return
		}

		poster := ""
		if img := item.Find(".th-img img[src]").First(); img.Length() > 0 {
			if src, ok := img.Attr("src"); ok && !strings.HasPrefix(src, "http") {
				poster = p.baseURL + src
			} else if ok {
				poster = src
			}
		}

		results = append(results, contracts.SearchResult{
			Title:    strings.TrimSpace(titleNode.Text()),
			URL:      link,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes scrapes the .fplayer block of an anime page
// [LIVE-VERIFIED 2026-09-13]. The page carries two player tabs:
//
//   - "Основной плеер": one span (ПЛЕЕР #1) whose data attribute is a
//     full-title playlist player on an external host — its episodes
//     cannot be split without fetching that player, so it is skipped;
//   - "Запасной плеер": one span per episode, text "Серия N", data
//     attribute a video.sibnet.ru/shell.php embed.
//
// Movie pages carry a single "Серия 1" span and no primary tab. Spans
// are emitted in document order; the single dub key is "AniDUB".
func (p *AniDUB) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    animeURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	var episodes []contracts.Episode
	doc.Find(".fplayer .series-tab span[data]").Each(func(_ int, span *goquery.Selection) {
		title := strings.TrimSpace(span.Text())
		num, ok := strings.CutPrefix(title, "Серия")
		if !ok {
			return // ПЛЕЕР #1 and other non-episode spans
		}
		num = strings.TrimSpace(num)
		if num == "" {
			return
		}

		embed, _ := span.Attr("data")
		if embed == "" {
			return
		}

		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: title,
			RawID: num,
			RawEmbeds: map[string][]string{
				"AniDUB": {embed},
			},
		})
	})
	return episodes, nil
}

// ResolveStream runs the episode's embeds through the extractor
// factory (the animego pattern): the sibnet extractor resolves the
// backup-player shell URLs to their mp4 source.
func (p *AniDUB) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
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
