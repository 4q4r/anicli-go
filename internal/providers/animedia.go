package providers

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniMediaBase is the site root [LIVE-VERIFIED 2026-09-18].
const AniMediaBase = "https://amd.online"

// AniMedia is the amd.online provider (Russian dubs). Like AniDUB it
// is not a port of a frozen anicli-py source: the historical
// animedia.online JSON v3 API (api.animedia.online/anime/search,
// /anime/{id}, …) is dead — every old path answers 404 — while the
// site itself lives on at amd.online as a DataLife Engine install
// fronted by DDoS-Guard (plain client requests pass; no JS challenge).
// Characterized live on 2026-09-18; the provider is written against
// the observed DLE shapes:
//
//   - Search: the DLE search form POST (do=search&subaction=search&
//     story=…). A GET with the same params returns the site chrome
//     with a recommendation feed instead of results, so the form
//     method is load-bearing. Results render server-side as .poster
//     cards directly following the searchpage article inside
//     #dle-content; the ~14 header recommendation widgets rendered
//     before #dle-content reuse the same .poster template and are
//     excluded by that scoping.
//   - Episodes + dubs: the anime page carries the player twice (PWA
//     and desktop blocks). Every dub — including the is-active
//     default — is an amd-kodik-voice button carrying a per-dub kodik
//     embed URL; episode numbers come from the nav_video_links
//     anchors (data-vid, repeated across blocks — deduped). A kodik
//     embed switches episodes purely by its episode query parameter,
//     so the one page fetch yields every (episode, dub) embed pair by
//     substituting the anchor's data-vid into each dub's src.
//   - Streams: the kodik embeds resolve through the shared extractor
//     factory (kodik first in the Python registration order), live-
//     probed 2026-09-18: 360/480/720 mp4 for a Steins;Gate embed.
//
// Known walls, typed per the no-silent-failure policy:
//
//   - Titles served through the frame_video_mod player on other hosts
//     (rutube for id 704, aser.pro for the «Врата Штейна 0» archive
//     page) carry no kodik data at all — GetEpisodes surfaces
//     contracts.ErrExtractFailed naming the serving host.
//   - The desktop <video-player data-aggregator="mali"> element is
//     hydrated client-side from the amedia.so app platform: empty
//     server-side, not part of the anonymous surface (its content
//     mirrors the kodik translations).
//   - Airing titles list anchors for episodes kodik has no material
//     for yet (the live episode page renders an empty iframe there);
//     such embeds fail at ResolveStream with the extractor's typed
//     error — the site's own player shows the same episode as
//     unplayable.
type AniMedia struct {
	Base
}

// amdServiceDub is the dub name for titles with exactly one unnamed
// kodik source (movies: a single /video/ iframe, no voice buttons).
const amdServiceDub = "AniMedia"

// amdSmokeQuery is the declared smoke probe (PR51 mechanism): both
// shared probes miss this catalog — Black Lagoon is not on amd.online
// [LIVE-VERIFIED 2026-09-18] — so the provider speaks for itself.
// «врата штейна» surfaces 3 kodik-served results (the series and the
// movie live-verified playable).
const amdSmokeQuery = "врата штейна"

// newAniMedia builds the provider against baseURL. amd.online answers
// plain client requests (verified via curl and the netclient
// fingerprint through the live probe), so the shared netclient is
// kept: per-provider cookie jar, status mapping and the CF ladder
// wiring all apply as for every standard provider.
func newAniMedia(baseURL string, http *netclient.Client) *AniMedia {
	return &AniMedia{Base: Base{
		id:          "animedia",
		name:        "AniMedia",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// SmokeQuery reports the provider-specific live smoke probe.
func (p *AniMedia) SmokeQuery() string { return amdSmokeQuery }

// amdAbsolutize turns the site's protocol-relative embed srcs (and
// any site-relative URL) into absolute https URLs.
func (p *AniMedia) amdAbsolutize(src string) string {
	switch {
	case strings.HasPrefix(src, "//"):
		return "https:" + src
	case strings.HasPrefix(src, "http"):
		return src
	default:
		return p.baseURL + src
	}
}

// amdSubstituteEpisode rewrites the episode query parameter of a kodik
// embed to num. A src without an episode param (and a num kodik could
// not consume), a non-numeric num, and a src already carrying exactly
// the requested episode are all returned verbatim: the substitution
// only ever performs a real change, never a re-serialization. All
// other params (season, only_translations, hide_selectors) are
// preserved through the re-encode.
func amdSubstituteEpisode(src, num string) string {
	for _, r := range num {
		if r < '0' || r > '9' {
			return src
		}
	}
	u, err := url.Parse(src)
	if err != nil {
		return src
	}
	q := u.Query()
	existing := q.Get("episode")
	if existing == "" || existing == num {
		return src
	}
	q.Set("episode", num)
	u.RawQuery = q.Encode()
	return u.String()
}

// Search POSTs the DLE search form and scrapes the result cards
// [LIVE-VERIFIED 2026-09-18: POST story=врата → HTTP 200, «найдено:
// 11 новостей», 10 rendered cards; junk query → 200 with zero cards.
// DLE's «найдено N» count over-reports by including non-article hits;
// the rendered cards are the truth]. The query is form-encoded by
// url.Values (spaces as '+') — the exact encoding the site's own
// <form method="post"> submits.
func (p *AniMedia) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{
		"do":        {"search"},
		"subaction": {"search"},
		"story":     {query},
	}
	resp, err := p.http.PostForm(ctx, p.baseURL+"/", form, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	// Result cards are the .poster siblings FOLLOWING the searchpage
	// article inside #dle-content — never the header recommendation
	// widgets rendered before it.
	results := make([]contracts.SearchResult, 0, 8)
	doc.Find("#dle-content > article.searchpage").
		NextAll().
		Filter(".poster").
		Each(func(_ int, card *goquery.Selection) {
			link := card.Find("a.poster__link[href]").First()
			title := strings.TrimSpace(card.Find("h3.poster__title").First().Text())
			if link.Length() == 0 || title == "" {
				return
			}
			href, _ := link.Attr("href")
			if href == "" {
				return
			}

			poster := ""
			if img := card.Find(".poster__img img[src]").First(); img.Length() > 0 {
				if src, ok := img.Attr("src"); ok && src != "" {
					poster = p.amdAbsolutize(src)
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

// amdDub is one parsed voice button: its human name and the kodik
// embed src carrying its translation id.
type amdDub struct {
	name string
	src  string
}

// GetEpisodes fetches the anime page and reconstructs the full
// (episode, dub) embed table from the one document [LIVE-VERIFIED
// 2026-09-18 on a series (Steins;Gate, 24 anchors × 11 dubs), a movie
// (Бесконечный поезд: single /video/ iframe, no anchors, no buttons)
// and two walled titles (Путешествие к бессмертию via rutube, «Врата
// Штейна 0» via aser.pro)].
func (p *AniMedia) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	// Dubs: the voice buttons in document order, deduped by the
	// data-voice id (the PWA and desktop blocks repeat the same set).
	var dubs []amdDub
	seenVoices := map[string]bool{}
	doc.Find("button.amd-kodik-voice[data-voice][data-kodik-src]").Each(func(_ int, btn *goquery.Selection) {
		voice, _ := btn.Attr("data-voice")
		if seenVoices[voice] {
			return
		}
		seenVoices[voice] = true
		name := strings.TrimSpace(btn.Text())
		src, _ := btn.Attr("data-kodik-src")
		if name == "" || src == "" {
			return
		}
		dubs = append(dubs, amdDub{name: name, src: p.amdAbsolutize(src)})
	})

	// Titles with a single unnamed kodik source (movies) render one
	// iframe and no buttons: the service dub carries it.
	if len(dubs) == 0 {
		doc.Find("iframe.amd-kodik-iframe[data-src]").EachWithBreak(func(_ int, frame *goquery.Selection) bool {
			src, _ := frame.Attr("data-src")
			if strings.TrimSpace(src) == "" {
				return true // an aired-out episode shell; keep looking
			}
			dubs = append(dubs, amdDub{name: amdServiceDub, src: p.amdAbsolutize(src)})
			return false
		})
	}

	// No kodik data anywhere: the title is served through a player
	// this provider cannot resolve anonymously (rutube / aser.pro
	// frame_video_mod et al). The typed wall names the serving host —
	// an empty list here would fake a healthy title with no episodes.
	if len(dubs) == 0 {
		player := "unknown"
		if frame := doc.Find("iframe.frame_video_mod[src]").First(); frame.Length() > 0 {
			if src, _ := frame.Attr("src"); src != "" {
				player = amdHost(src)
			}
		}
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: amd.online serves this title through the unsupported %q player; "+
				"no anonymous kodik data on the page", contracts.ErrExtractFailed, player))
	}

	// Episodes: the nav anchor data-vid values, deduped across the
	// repeated blocks; ascending when numeric (the site renders them
	// in ascending episode order). No anchors at all means a
	// single-embed title (movie): one episode numbered 1.
	var nums []string
	seenNums := map[string]bool{}
	doc.Find("a.nav_video_links[data-vid]").Each(func(_ int, anchor *goquery.Selection) {
		vid, _ := anchor.Attr("data-vid")
		if vid == "" || seenNums[vid] {
			return
		}
		seenNums[vid] = true
		nums = append(nums, vid)
	})
	if len(nums) == 0 {
		nums = []string{"1"}
	}
	if amdAllDigits(nums) {
		amdSortNumeric(nums)
	}

	episodes := make([]contracts.Episode, 0, len(nums))
	for _, num := range nums {
		embeds := make(map[string][]string, len(dubs))
		for _, dub := range dubs {
			embeds[dub.name] = []string{amdSubstituteEpisode(dub.src, num)}
		}
		episodes = append(episodes, contracts.Episode{
			Num:       num,
			RawID:     num,
			RawEmbeds: embeds,
		})
	}
	return episodes, nil
}

// ResolveStream resolves one dub's embed through the shared extractor
// factory: kodik embeds dominate this site and resolve to quality-
// keyed mp4 links. A dub the episode does not carry is a caller bug
// (typed ErrInvalidInput); a real resolve that yields nothing (an
// episode kodik has no material for yet, or a non-kodik embed) is the
// typed extract wall.
func (p *AniMedia) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
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

// amdAllDigits reports whether every value is a non-empty decimal
// number.
func amdAllDigits(values []string) bool {
	for _, v := range values {
		if _, err := strconv.Atoi(v); err != nil {
			return false
		}
	}
	return len(values) > 0
}

// amdHost extracts the host of a player embed src for diagnostics
// ("unknown" when unparseable).
func amdHost(src string) string {
	u, err := url.Parse(strings.TrimPrefix(src, "//"))
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Host
}

// amdSortNumeric orders episode numbers by their integer value (all
// values are validated numeric by amdAllDigits first).
func amdSortNumeric(values []string) {
	keys := make([]int, len(values))
	for i, v := range values {
		n, _ := strconv.Atoi(v)
		keys[i] = n
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
