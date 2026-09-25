package providers

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniKadoBase is the site root [LIVE-VERIFIED 2026-09-25: direct
// HTTP 200, no JS challenge, anonymous viewing — an account exists
// only for bookmarks].
const AniKadoBase = "https://anikado.net"

// akServiceDub is the dub name for titles with exactly one unnamed
// kodik source (movies: the /video/ embed in the active kodik tab, no
// episode pages, no translator list).
const akServiceDub = "AniKado"

// akEpisodeNumRe tails an episode-page href ("…/episode-12.html");
// group 1 is the episode number.
var akEpisodeNumRe = regexp.MustCompile(`/episode-(\d+)\.html$`)

// akKodikMirrorRe matches the kodik.info embed host the episode pages
// serve. kodik's domains are interchangeable mirrors: the identical
// /seria/ path on kodikplayer.com answers HTTP 200 with the
// hash-consistent player page [LIVE-VERIFIED 2026-09-25], and
// kodikplayer.com is the host the site itself uses for its default
// (active) title-page tab — while kodik.info is SNI-filtered on some
// networks (observed: TLS reset mid-handshake).
var akKodikMirrorRe = regexp.MustCompile(`^//kodik\.info/`)

// AniKado is the anikado.net provider (Russian dubs, PR102). Like
// AniMedia it is not a port of a frozen anicli-py source: the provider
// is written against the live DLE install, characterized on
// 2026-09-25:
//
//   - Search: the DLE search form POST (do=search&subaction=search&
//     story=…) to /index.php?do=search. Results render server-side as
//     article.card elements inside #dle-content (10 per page; a junk
//     query answers the same shell with zero cards — an empty list,
//     not an error). GET with the same params answers identically;
//     the form method is POST.
//   - Episodes: the title page renders the whole episode list
//     server-side as .flex-episodes-links anchors
//     (…/episode-N.html; verified up to 52 anchors, no pagination).
//     Every episode page carries the per-(episode, dub) kodik embed
//     table as li.b-translator__item rows (data-this_translator name,
//     data-this_link embed), so the listing fans out one fetch per
//     episode bounded by network.max_parallel (the kickassanime
//     pattern).
//   - Dubs: the b-translator__item names verbatim (including the
//     «Субтитры» entries kodik serves as translations); the title
//     page's «Озвучка:» row mirrors the same kodik_translation
//     taxonomy.
//   - Streams: the kodik embeds resolve through the shared extractor
//     factory (kodik.info hosts normalized onto kodikplayer.com —
//     see akKodikMirrorRe).
//
// Known walls, typed per the no-silent-failure policy:
//
//   - The title page's two fallback player tabs are NOT resolvable
//     anonymously: the vkg tab is a client-side hydrated
//     <video-player> fed by the mali aggregator (its content mirrors
//     the kodik translations), and the tomion tab's
//     //tomion.org/yal/{id} iframe 404s outside its frame context
//     [LIVE-VERIFIED 2026-09-25]. A title page with neither episode
//     anchors nor a kodik tab iframe surfaces
//     contracts.ErrNotFound; a tomion embed reaching ResolveStream
//     surfaces contracts.ErrExtractFailed from the factory.
//   - An episode page without translator rows is a structural break
//     and fails loud naming the episode (the site's own player would
//     render an empty iframe there).
type AniKado struct {
	Base
	// maxParallel bounds the episode-page fan-out (config
	// network.max_parallel).
	maxParallel int
}

// newAniKado builds the provider against baseURL. anikado.net answers
// plain client requests (verified via curl and the netclient
// fingerprint through the live probe), so the shared netclient is
// kept: per-provider cookie jar, status mapping and the CF ladder
// wiring all apply as for every standard provider.
func newAniKado(baseURL string, http *netclient.Client, maxParallel int) *AniKado {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	return &AniKado{
		Base: Base{
			id:          "anikado",
			name:        "AniKado",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		maxParallel: maxParallel,
	}
}

// Search POSTs the DLE search form and scrapes the result cards
// [LIVE-VERIFIED 2026-09-25: POST story=черная лагуна → HTTP 200,
// «найдено 2 ответов», 2 rendered cards; junk query → 200 with zero
// cards. POST and GET answer byte-identically; the site's quicksearch
// form submits POST]. The query is form-encoded by url.Values.
func (p *AniKado) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{
		"do":        {"search"},
		"subaction": {"search"},
		"story":     {query},
	}
	resp, err := p.http.PostForm(ctx, p.baseURL+"/index.php?do=search", form, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	results := make([]contracts.SearchResult, 0, 10)
	doc.Find("#dle-content article.card").Each(func(_ int, card *goquery.Selection) {
		link := card.Find("a.card__img[href]").First()
		titleNode := card.Find("h2.card__title a").First()
		if link.Length() == 0 || titleNode.Length() == 0 {
			return
		}
		href, _ := link.Attr("href")
		title := strings.TrimSpace(titleNode.Text())
		if href == "" || title == "" {
			return
		}

		poster := ""
		if img := card.Find("img[src]").First(); img.Length() > 0 {
			if src, ok := img.Attr("src"); ok && src != "" {
				poster = p.baseURL + src // site-relative uploads path
			}
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

// akEpisodeRef is one title-page episode anchor: the wire episode
// number and its episode-page URL.
type akEpisodeRef struct {
	num  string
	page string
}

// GetEpisodes lists the (episode, dub) embed table for the anime at
// animeURL [LIVE-VERIFIED 2026-09-25 on a series (Пираты «Чёрной
// лагуны», 12 anchors × 4 translators), a movie (Бесконечный поезд.
// Фильм: single /video/ embed in the kodik tab, no anchors, no
// translator rows) and a playerless page (the typed wall)].
//
// Series: the title page's episode anchors fan out one fetch per
// episode page bounded by maxParallel; each page's
// li.b-translator__item rows become the (dub → embed) map of that
// episode. Movies: no anchors — the active kodik tab's iframe is the
// single episode under the service dub name (the AniMedia service-dub
// ruling).
func (p *AniKado) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	// Episodes: the title-page anchors, in document order (the site
	// renders them ascending). Movies have none.
	var refs []akEpisodeRef
	seenNums := map[string]bool{}
	doc.Find(".flex-episodes-links a[href]").Each(func(_ int, anchor *goquery.Selection) {
		href, _ := anchor.Attr("href")
		m := akEpisodeNumRe.FindStringSubmatch(href)
		if m == nil || seenNums[m[1]] {
			return
		}
		seenNums[m[1]] = true
		refs = append(refs, akEpisodeRef{num: m[1], page: href})
	})

	if len(refs) == 0 {
		return p.akMovieEpisode(doc, resp.StatusCode)
	}

	// The fan-out walks indices so results land back in document
	// order without a lookup.
	idxs := make([]int, len(refs))
	for i := range idxs {
		idxs[i] = i
	}
	episodes := make([]contracts.Episode, len(refs))
	fanErr := netclient.Parallel(ctx, idxs, p.maxParallel, func(ctx context.Context, idx int) error {
		ep, err := p.akFetchEpisode(ctx, refs[idx])
		if err != nil {
			return err
		}
		episodes[idx] = ep
		return nil
	})
	if fanErr != nil {
		return nil, fanErr
	}
	return episodes, nil
}

// akMovieEpisode builds the single-episode listing of a movie title
// from the title page's active kodik tab. No kodik tab iframe at all
// is the typed wall (an empty list here would fake a healthy title
// with no episodes).
func (p *AniKado) akMovieEpisode(doc *goquery.Document, status int) ([]contracts.Episode, error) {
	frame := doc.Find("#kodik-player iframe[src], .player-content iframe[src]").First()
	if frame.Length() == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, status,
			fmt.Errorf("title page carries no episode anchors and no player-tab iframe: %w", contracts.ErrNotFound))
	}
	src, _ := frame.Attr("src")

	return []contracts.Episode{{
		Num:       "1",
		RawID:     "1",
		RawEmbeds: map[string][]string{akServiceDub: {akNormalizeEmbed(src)}},
	}}, nil
}

// akFetchEpisode fetches one episode page and extracts its
// per-dub kodik embed table from the b-translator__item rows.
func (p *AniKado) akFetchEpisode(ctx context.Context, ref akEpisodeRef) (contracts.Episode, error) {
	resp, err := p.http.Get(ctx, ref.page, nil)
	if err != nil {
		return contracts.Episode{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode %s page: %w", ref.num, err))
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return contracts.Episode{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse episode %s page: %w", ref.num, err))
	}

	embeds := map[string][]string{}
	doc.Find("li.b-translator__item[data-this_link][data-this_translator]").Each(func(_ int, li *goquery.Selection) {
		name := strings.TrimSpace(li.AttrOr("data-this_translator", ""))
		link := strings.TrimSpace(li.AttrOr("data-this_link", ""))
		if name == "" || link == "" {
			return
		}
		embeds[name] = append(embeds[name], akNormalizeEmbed(link))
	})

	// No translator rows on an episode page is a structural break:
	// the site's own player renders an empty iframe there — surface
	// it typed instead of an episode with no dubs.
	if len(embeds) == 0 {
		return contracts.Episode{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("episode %s page carries no translator rows: %w", ref.num, contracts.ErrExtractFailed))
	}

	return contracts.Episode{
		Num:       ref.num,
		RawID:     ref.num,
		RawEmbeds: embeds,
	}, nil
}

// ResolveStream resolves one dub's embed through the shared extractor
// factory: kodik embeds dominate this site and resolve to quality-
// keyed mp4 links. A dub the episode does not carry is a caller bug
// (typed ErrInvalidInput); a real resolve that yields nothing (a
// non-kodik embed such as the tomion tab's host) is the typed
// extract wall.
func (p *AniKado) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	links, ok := episode.RawEmbeds[dubID]
	if !ok || len(links) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrInvalidInput, episode.Num, dubID))
	}

	sources, err := resolveEmbeds(ctx, p.http, links)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	if len(sources) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: no extractor yielded links for episode %s dub %q",
				contracts.ErrExtractFailed, episode.Num, dubID))
	}
	stream.Links = sources
	return stream, nil
}

// akNormalizeEmbed turns a captured player embed into the URL the
// shared extractor factory consumes: protocol-relative srcs gain the
// https scheme, and the kodik.info embed host is normalized onto the
// interchangeable kodikplayer.com mirror (see akKodikMirrorRe).
// Everything after the host — path and query, including the site's
// doubled ?hide_selectors=true quirk — passes through verbatim.
func akNormalizeEmbed(src string) string {
	src = akKodikMirrorRe.ReplaceAllString(src, "//kodikplayer.com/")
	return normalizeProtocolURL(src)
}
