package providers

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeVibBase is the site root [LIVE-VERIFIED 2026-09-25].
const AnimeVibBase = "https://www.animevib.ru"

// avServiceDub is the dub name for /video/ (movie) embeds: a kodik
// video page carries no translation metadata, and the single unnamed
// source needs a stable key (the amdServiceDub precedent).
const avServiceDub = "AnimeVib"

// avSmokeQuery is the declared smoke probe (PR51 mechanism): both
// shared probes miss this catalog — Black Lagoon is not on animevib
// [LIVE-VERIFIED 2026-09-25] — so the provider speaks for itself.
// «дандадан» surfaces 2 kodik-served results (the ongoing and its
// first season, both live-verified through the serial embed chain).
const avSmokeQuery = "дандадан"

// avKodikURLRe splits a kodik embed path into its kind
// (serial|video), media id and media hash. The trailing /720p (or any
// quality) segment is optional. Tested against the live shapes:
// /serial/62118/e5bc8c34…/720p and /video/113757/15dee337…/720p.
var avKodikURLRe = regexp.MustCompile(`/(serial|video)/(\d+)/([0-9a-f]{32})(?:/\d+p?)?/?$`)

// avPosterRe lifts the poster URL out of the card's inline
// background-image style.
var avPosterRe = regexp.MustCompile(`background-image:\s*url\(([^)]+)\)`)

// avSerialPage is the URL shape of a kodik serial page (the only page
// kind carrying the translations/episodes selects): <base>/serial/
// <media-id>/<media-hash>/<quality>.
func avSerialPage(base, mediaID, mediaHash string) string {
	return base + "/serial/" + mediaID + "/" + mediaHash + "/720p"
}

// avSeriaEmbed is the URL shape of a kodik seria (single-episode)
// page synthesized from a serial page's per-episode option. The shared
// kodik extractor scrapes the page's vInfo and resolves the direct
// links — the exact flow the kodik provider embeds ride.
func avSeriaEmbed(base, seriaID, seriaHash string) string {
	return base + "/seria/" + seriaID + "/" + seriaHash + "/720p"
}

// AnimeVib is the www.animevib.ru provider (Russian catalog). Not a
// port of a frozen anicli-py source: written against the live site
// (2026-09-25). The site is DLE (DataLife Engine — dle_js, engine/
// classes, /N-slug.html post URLs), not WordPress as the controller
// intel assumed: the DLE search form answers server-side, and the ?s=
// parameter is silently ignored (it echoes the homepage). Every post
// embeds ONE kodik player on iframe.player-shar:
//
//   - /serial/ embeds: the kodik player page itself lists the
//     translations (48 on an ongoing — with per-translation
//     data-media-id/data-media-hash) and, per translation, the
//     episodes with per-episode seria data-id/data-hash. The provider
//     fetches each translation's serial page (bounded-parallel) and
//     merges the (episode × dub) table; per-episode embeds are
//     synthesized /seria/ URLs resolved through the shared kodik
//     extractor.
//   - /video/ embeds (movies): a single episode under avServiceDub;
//     the embed URL resolves directly through the shared extractor.
//
// The stloadi.live ad player in the post's second tab is never classed
// player-shar and is never selected.
type AnimeVib struct {
	Base

	// maxParallel bounds the per-translation serial-page fetches.
	maxParallel int
}

// newAnimeVib builds the provider against baseURL. The site answers
// plain desktop-UA requests (verified via curl and the netclient
// fingerprint through the live probe), so no extra headers are sent.
func newAnimeVib(baseURL string, http *netclient.Client, maxParallel int) *AnimeVib {
	// netclient.Parallel treats ≤0 as unbounded — clamp to 1 so a bad
	// config value cannot unbound the serial-page fan-out (wave A
	// review F3, anikado precedent).
	if maxParallel <= 0 {
		maxParallel = 1
	}
	return &AnimeVib{
		Base: Base{
			id:          "animevib",
			name:        "AnimeVib",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		maxParallel: maxParallel,
	}
}

// SmokeQuery reports the provider-specific live smoke probe.
func (p *AnimeVib) SmokeQuery() string { return avSmokeQuery }

// Search GETs the DLE search form and scrapes the result cards
// [LIVE-VERIFIED 2026-09-25: GET
// /index.php?do=search&subaction=search&story=дандадан → HTTP 200, 2
// cards; story=ванпанчмен → 7 cards; junk query → 200 with zero
// cards]. Results render server-side as article.short.movie-item
// cards — a class pair the homepage catalog cards (.shortstory-in)
// and the sidebar recommendation rail (.side_ongoing) do not carry,
// so neither can leak into the results. Both RU and latin queries
// match the DLE index (verified live), so NamePreference stays
// undeclared (the default routing). The query is percent-encoded with
// pyQuote (spaces %20).
func (p *AnimeVib) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	searchURL := p.baseURL + "/index.php?do=search&subaction=search&story=" + pyQuote(query)

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
	doc.Find("article.movie-item").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a.short__title[href]").First()
		if linkNode.Length() == 0 {
			return
		}
		link, ok := linkNode.Attr("href")
		if !ok || link == "" {
			return
		}

		title := strings.TrimSpace(linkNode.Text())
		if title == "" {
			return
		}

		poster := ""
		if m := avPosterRe.FindStringSubmatch(item.Find("figure div[style]").First().AttrOr("style", "")); m != nil {
			poster = strings.TrimSpace(m[1])
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

// avEmbedInfo is the parsed iframe.player-shar embed: the absolute
// embed URL and the kodik URL-shape match.
type avEmbedInfo struct {
	url  string
	kind string // "serial" | "video"
	id   string // kodik media id
	hash string // kodik media hash
	base string // scheme://host of the embed
}

// avParseEmbed absolutizes the protocol-relative iframe src and splits
// the kodik URL shape. A player-shar iframe that is not a kodik
// serial/video embed is a typed extraction failure — the site has no
// other player family on that class (the stloadi ad tab carries no
// class at all).
func avParseEmbed(src, postURL string) (avEmbedInfo, error) {
	abs, err := avAbsolutize(src, postURL)
	if err != nil {
		return avEmbedInfo{}, fmt.Errorf("%w: bad player iframe src %q: %w",
			contracts.ErrExtractFailed, src, err)
	}
	m := avKodikURLRe.FindStringSubmatch(abs)
	if m == nil {
		return avEmbedInfo{}, fmt.Errorf("%w: unsupported player embed %q (want a kodik /serial/ or /video/ url)",
			contracts.ErrExtractFailed, abs)
	}
	parsed, err := url.Parse(abs)
	if err != nil {
		return avEmbedInfo{}, fmt.Errorf("%w: unparseable embed url: %w", contracts.ErrExtractFailed, err)
	}
	return avEmbedInfo{
		url:  abs,
		kind: m[1],
		id:   m[2],
		hash: m[3],
		base: parsed.Scheme + "://" + parsed.Host,
	}, nil
}

// avAbsolutize turns the site's protocol-relative embed src into an
// absolute URL using the post page's scheme.
func avAbsolutize(src, postURL string) (string, error) {
	switch {
	case strings.HasPrefix(src, "//"):
		parsed, err := url.Parse(postURL)
		if err != nil {
			return "", err
		}
		return parsed.Scheme + ":" + src, nil
	case strings.HasPrefix(src, "http"):
		return src, nil
	default:
		return "", fmt.Errorf("relative player src")
	}
}

// avTranslation is one entry of the kodik translations select.
type avTranslation struct {
	mediaID   string
	mediaHash string
	title     string
}

// avSeria is one episode of one translation.
type avSeria struct {
	num  string
	id   string
	hash string
}

// GetEpisodes fetches the post page and rebuilds the full
// (episode × dub) embed table from the kodik embed [LIVE-VERIFIED
// 2026-09-25 on Дандадан (serial 62118: 12 episodes × 48
// translations, ep-1 JAM seria 1362451/a899a9b6… vs AniDUB
// 1362576/2be98362…) and on Клинок, рассекающий демонов (video
// 113757: single-episode movie)].
//
// Request budget: the post page + the main serial page (which doubles
// as the default translation's page — its data-media-id/hash equal the
// iframe src) + one serial page per remaining translation,
// bounded-parallel. A translation page that fails to load contributes
// nothing: the table keeps the dubs that resolved, and only when
// NOTHING resolved does the first error surface (the resolveEmbeds
// no-silent-failure semantics).
func (p *AnimeVib) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	src, ok := doc.Find("iframe.player-shar[src]").First().Attr("src")
	if !ok || src == "" {
		// No player: a typed wall (wave A review F2) — a silent empty
		// would fake a healthy title with no episodes.
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: post page carries no player iframe; no anonymous episode surface",
				contracts.ErrNotFound))
	}

	embed, err := avParseEmbed(src, animeURL)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode, err)
	}

	if embed.kind == "video" {
		// Movie: kodik video pages carry no translations or episode
		// selects — one episode, one unnamed dub, nothing else to
		// dereference.
		return []contracts.Episode{{
			Num:   "1",
			RawID: "1",
			RawEmbeds: map[string][]string{
				avServiceDub: {embed.url},
			},
		}}, nil
	}

	// Serial: the main embed page IS a translation serial page (the
	// default dub's) — parse it, then fetch every sibling translation.
	main, err := p.fetchSerialPage(ctx, embed.url)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0, err)
	}

	// Dereference the main page's serial identity so its translation
	// entry reuses the already-fetched page.
	mainTrans := avTranslation{
		mediaID:   embed.id,
		mediaHash: embed.hash,
		title:     main.title,
	}

	translations := make([]avTranslation, 0, len(main.translations))
	for _, tr := range main.translations {
		if tr.mediaID == embed.id && tr.mediaHash == embed.hash {
			mainTrans.title = tr.title // named from the translations select
			continue
		}
		translations = append(translations, tr)
	}

	// Bounded-parallel serial-page fetch per translation; failures are
	// collected (first wins) and only surface when nothing else did.
	type avResult struct {
		title    string
		episodes []avSeria
	}
	results := make([]avResult, 0, len(translations))
	var (
		mu       sync.Mutex
		firstErr error
	)
	fetchOne := func(ctx context.Context, tr avTranslation) error {
		page, err := p.fetchSerialPage(ctx, avSerialPage(embed.base, tr.mediaID, tr.mediaHash))
		if err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			return nil // soft: one dead dub team must not kill the table
		}
		mu.Lock()
		results = append(results, avResult{title: tr.title, episodes: page.episodes})
		mu.Unlock()
		return nil
	}
	if err := netclient.Parallel(ctx, translations, p.maxParallel, fetchOne); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0, err)
	}

	// Merge into the (episode × dub) table: union of episode numbers,
	// each dub carrying the seria embeds it has material for.
	byNum := map[string]map[string][]string{} // num -> dub -> embeds
	addSeria := func(dub string, episodes []avSeria) {
		for _, ep := range episodes {
			if byNum[ep.num] == nil {
				byNum[ep.num] = map[string][]string{}
			}
			byNum[ep.num][dub] = append(byNum[ep.num][dub],
				avSeriaEmbed(embed.base, ep.id, ep.hash))
		}
	}
	addSeria(mainTrans.title, main.episodes)
	for _, res := range results {
		addSeria(res.title, res.episodes)
	}

	if len(byNum) == 0 {
		if firstErr != nil {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0, firstErr)
		}
		// A serial page with selects but no episode rows: a typed wall
		// (wave A review F2), never a silent empty.
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: serial page carries selects but no episode rows from any translation",
				contracts.ErrNotFound))
	}

	nums := make([]string, 0, len(byNum))
	for num := range byNum {
		nums = append(nums, num)
	}
	sort.Slice(nums, func(i, j int) bool {
		a, aerr := strconv.Atoi(nums[i])
		b, berr := strconv.Atoi(nums[j])
		if aerr != nil || berr != nil {
			return nums[i] < nums[j] // non-numeric tails sort by string
		}
		return a < b
	})

	episodes := make([]contracts.Episode, 0, len(nums))
	for _, num := range nums {
		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     num + " серия",
			RawID:     num,
			RawEmbeds: byNum[num],
		})
	}
	return episodes, nil
}

// avSerialPageData is the parsed kodik serial page: the dub title the
// page is serving, the translations select and the visible series
// select's per-episode seria hashes.
type avSerialPageData struct {
	title        string
	translations []avTranslation
	episodes     []avSeria
}

// fetchSerialPage GETs a kodik serial page and scrapes its selects.
// The dub title comes from the seasons select's data-translation-title
// (the translations select names every dub; the page itself only names
// the one it serves — when the attribute is missing the fallback is
// the provider name, which never happens for live serial pages).
func (p *AnimeVib) fetchSerialPage(ctx context.Context, pageURL string) (avSerialPageData, error) {
	resp, err := p.http.Get(ctx, pageURL, nil)
	if err != nil {
		return avSerialPageData{}, fmt.Errorf("fetch kodik serial page %s: %w", pageURL, err)
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return avSerialPageData{}, fmt.Errorf("%w: parse kodik serial page %s: %w",
			contracts.ErrExtractFailed, pageURL, err)
	}

	var data avSerialPageData
	// The seasons select names the dub this page serves
	// (data-translation-title); the fallback never fires on live
	// serial pages.
	data.title = strings.TrimSpace(doc.Find(".serial-seasons-box option[data-translation-title]").First().AttrOr("data-translation-title", ""))
	if data.title == "" {
		data.title = p.name
	}

	// The translations select: one option per dub team with the serial
	// page (media id/hash) carrying that dub's own per-episode seria
	// hashes. data-title is the bare dub name (the option text carries
	// an «(N эп.)» suffix).
	doc.Find(".serial-translations-box option[data-media-id][data-media-hash]").Each(func(_ int, opt *goquery.Selection) {
		mediaID := opt.AttrOr("data-media-id", "")
		mediaHash := opt.AttrOr("data-media-hash", "")
		title := strings.TrimSpace(opt.AttrOr("data-title", ""))
		if mediaID == "" || mediaHash == "" || title == "" {
			return
		}
		data.translations = append(data.translations, avTranslation{
			mediaID:   mediaID,
			mediaHash: mediaHash,
			title:     title,
		})
	})

	// The visible series select only: the hidden div.season-N groups
	// under .series-options repeat the same option markup and must not
	// double the table (live pages carry 24 hash-bearing options, 12
	// of them duplicated).
	seen := map[string]bool{}
	doc.Find(".serial-series-box select option[data-id][data-hash]").Each(func(_ int, opt *goquery.Selection) {
		val := opt.AttrOr("value", "")
		id := opt.AttrOr("data-id", "")
		hash := opt.AttrOr("data-hash", "")
		if val == "" || id == "" || hash == "" || seen[val+"/"+id+"/"+hash] {
			return
		}
		seen[val+"/"+id+"/"+hash] = true
		data.episodes = append(data.episodes, avSeria{num: val, id: id, hash: hash})
	})

	return data, nil
}

// ResolveStream runs the episode's embeds through the extractor
// factory (the anidub pattern): the kodik extractor resolves the
// synthesized /seria/ URLs (and /video/ movie embeds) to their direct
// links.
func (p *AnimeVib) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	// A dub the episode does not carry is a caller bug (wave A review
	// F2, animedia precedent): resolveEmbeds(nil) would answer a
	// silent empty MediaStream.
	links, ok := episode.RawEmbeds[dubID]
	if !ok || len(links) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrInvalidInput, episode.Num, dubID))
	}

	sources, err := resolveEmbeds(ctx, p.http, links)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}
