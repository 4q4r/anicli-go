package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniTokyoBase is the site root [LIVE-VERIFIED 2026-09-25: UTF-8, plain
// client requests pass through the Cloudflare front with no challenge on
// any probed path — search, release pages and the video.php wrappers all
// answer anonymously, cookies not required]. Route note: on the network
// this provider was characterized from, the direct route to the site
// times out while the configured proxy answers — same class as the
// hdrezka/anifilm geo-fence notes, `network.proxy_url` re-routes it.
const AniTokyoBase = "https://anitokyo.tv"

// AniTokyo is the anitokyo.tv provider (Russian dubs and subtitles). Not
// a port of a frozen anicli-py source (animedia/animemobi precedent):
// written from the live site, probed and capture-verified 2026-09-25. The
// site is a DataLife Engine install whose player is the RalodePlayer DLE
// module — the release page embeds one JSON blob describing EVERY
// (dub, episode) pair, so episodes and dubs hydrate from a single fetch:
//
//   - Search: the DLE search form POST (do=search&subaction=search&
//     story=…) renders result cards server-side as article.story.
//     shortstory; the index spans several release sections (/anime/,
//     /ongoing/, /ova/, /movie/ surface; the dedicated /hentai/ section
//     and non-release links are dropped — animemobi's section filter
//     precedent).
//   - Episodes + dubs: RalodePlayer.init(<dubs>,<meta>) in the release
//     page. <dubs> maps dub id → {name (the voice team), items}; each
//     item is one episode of that dub (aname label, lssort number) whose
//     scode iframe points at the site's own /video.php?id=N&cat=K
//     wrapper. Episodes group across dubs by lssort.
//   - Streams: the video.php wrapper page carries exactly one player
//     iframe — a kodik embed (codetype 110, the dominant host) or a sibnet
//     shell page (codetype 13). Both resolve through the shared extractor
//     factory.
//
// Known walls, typed per the no-silent-failure policy:
//
//   - Announcement («Анонс») pages carry no RalodePlayer data at all —
//     GetEpisodes surfaces contracts.ErrNotFound (the site itself renders
//     those releases playerless).
//   - A wrapper page without its player iframe is the typed extract wall.
//   - Items flagged err != "0" by the module still surface: their wrappers
//     fail at resolve time with the extractor's typed error.
type AniTokyo struct {
	Base
}

// anitokyoSmokeQuery is the declared live probe (PR51 mechanism): the
// shared RU probe «черная лагуна» misses the catalog — live-verified
// 2026-09-25 (zero cards; the title is simply absent) — so the provider
// speaks for itself. «дандадан» surfaces the three Dandadan releases, two
// of them fully playable (60-dub TV seasons).
const anitokyoSmokeQuery = "дандадан"

// anitokyoDubRe digs the player iframe src out of a video.php wrapper
// page. The wrapper's only real <iframe> element is the player (the ad
// overlay lives in jQuery selector strings, not markup); first match wins.
var anitokyoEmbedRe = regexp.MustCompile(`<iframe[^>]+src="([^"]+)"`)

// anitokyoScodeRe digs the wrapper URL out of an item's scode iframe
// markup (site-relative: "/video.php?id=445222&cat=1").
var anitokyoScodeRe = regexp.MustCompile(`src="([^"]+)"`)

// newAniTokyo builds the provider against baseURL. The shared netclient
// is kept for every leg (search form, release page, video.php wrapper):
// the site answers browser-fingerprint requests normally (per-provider
// cookie jar, status mapping and the CF ladder wiring all apply).
func newAniTokyo(baseURL string, http *netclient.Client) *AniTokyo {
	return &AniTokyo{Base: Base{
		id:          "anitokyo",
		name:        "AniTokyo",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// SmokeQuery reports the provider-specific live probe; see
// anitokyoSmokeQuery.
func (p *AniTokyo) SmokeQuery() string { return anitokyoSmokeQuery }

// anitokyoDub is one parsed RalodePlayer dub: the voice team's name and
// its items (episode entries) keyed by the module's item id.
type anitokyoDub struct {
	Name  string                     `json:"name"`
	Items map[string]anitokyoEpisode `json:"items"`
}

// anitokyoEpisode is one item of the RalodePlayer blob: one episode of
// one dub. The wire carries every field as strings (verified across a
// 60-dub TV season, a movie and the module defaults); lssort is the
// episode number, aname the site label («1 серия»), scode the wrapper
// iframe markup.
type anitokyoEpisode struct {
	Aname  string `json:"aname"`
	Lssort string `json:"lssort"`
	Scode  string `json:"scode"`
}

// Search POSTs the DLE search form and scrapes the result cards
// [LIVE-VERIFIED 2026-09-25: POST story=дандадан → HTTP 200, «найдено 3
// ответов», 3 rendered cards; the shared smoke query «черная лагуна» →
// 200 with zero cards]. Cards render inside #dle-content as
// article.story.shortstory; the sidebar recommendation widgets use other
// templates and drop out on the article selector. The query is
// form-encoded by url.Values — the exact encoding the site's own
// <form method="post"> submits. DLE caps the answer at one page; no
// pagination is attempted (yummy/anilib/anistar single-page precedent).
func (p *AniTokyo) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{
		"do":        {"search"},
		"subaction": {"search"},
		"story":     {query},
	}
	resp, err := p.http.PostForm(ctx, p.baseURL+"/", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if err != nil {
		return nil, err
	}

	doc, parseErr := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if parseErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", parseErr))
	}

	results := make([]contracts.SearchResult, 0, 10)
	doc.Find("article.story.shortstory").Each(func(_ int, card *goquery.Selection) {
		link := card.Find("h2.story-title a[href]").First()
		if link.Length() == 0 {
			return
		}
		href, _ := link.Attr("href")
		if !anitokyoPlayableSection(href) {
			return
		}
		title := strings.TrimSpace(link.Text())

		poster := ""
		if img := card.Find(".story-poster img[src]").First(); img.Length() > 0 {
			poster, _ = img.Attr("src")
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      href,
			SourceID: p.ID(),
			Poster:   animemobiAbsURL(p.baseURL, poster),
		})
	})
	return results, nil
}

// anitokyoPlayableSection reports whether a release URL belongs to one of
// the catalog's playable sections: /anime/, /ongoing/, /ova/ and /movie/
// (the «твоё имя» capture surfaces an /ova/ card — live-verified
// 2026-09-25). The dedicated /hentai/ section and any other link shape
// (site news, user pages) are dropped.
func anitokyoPlayableSection(u string) bool {
	for _, sec := range []string{"/anime/", "/ongoing/", "/ova/", "/movie/"} {
		if strings.Contains(u, sec) {
			return true
		}
	}
	return false
}

// GetEpisodes fetches the release page and decodes its RalodePlayer blob
// into the (episode, dub) table [LIVE-VERIFIED 2026-09-25 on a 60-dub
// 12-episode TV season (Dandadan TV-1), a 3-dub movie (SAO Unanswered
// Butterfly, video.php cat=2) and an announcement page with no player
// data]. Episodes group across dubs by their lssort and sort ascending
// when numeric; every dub ref is the site's own /video.php wrapper URL,
// resolved lazily by ResolveStream. A page without the blob is the typed
// not-found.
func (p *AniTokyo) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}

	dubs, err := anitokyoParseDubs(resp.Body)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode, err)
	}

	// Group items across dubs by episode number (lssort); deterministic
	// order: numeric when every value is numeric (the site numbers its
	// items in ascending episode order), insertion order otherwise.
	type episodeRow struct {
		title string
		refs  map[string][]string
	}
	rows := map[string]*episodeRow{}
	var nums []string
	for _, dub := range dubs {
		for _, item := range dub.Items {
			num := item.Lssort
			if num == "" {
				continue
			}
			src := anitokyoScodeRe.FindStringSubmatch(item.Scode)
			if len(src) < 2 || src[1] == "" {
				continue
			}
			row, ok := rows[num]
			if !ok {
				row = &episodeRow{title: item.Aname, refs: map[string][]string{}}
				rows[num] = row
				nums = append(nums, num)
			}
			row.refs[dub.Name] = []string{animemobiAbsURL(p.baseURL, src[1])}
		}
	}
	if len(nums) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: RalodePlayer blob carries no episodes on page %s", contracts.ErrNotFound, animeURL))
	}
	if amdAllDigits(nums) {
		amdSortNumeric(nums)
	} else {
		sorted := append([]string(nil), nums...)
		sort.Strings(sorted)
		nums = sorted
	}

	episodes := make([]contracts.Episode, 0, len(nums))
	for _, num := range nums {
		row := rows[num]
		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     row.title,
			RawID:     num,
			RawEmbeds: row.refs,
		})
	}
	return episodes, nil
}

// anitokyoParseDubs extracts and decodes the RalodePlayer.init(<dubs>,…)
// first argument. The decoder stops at the end of the first JSON value,
// so the trailing meta object needs no special handling. A page without
// the init call is the typed not-found (announcement pages — the site
// itself renders them playerless); a blob that does not decode is the
// typed extract wall.
func anitokyoParseDubs(body []byte) ([]anitokyoDub, error) {
	const call = "RalodePlayer.init("
	idx := bytes.Index(body, []byte(call))
	if idx < 0 {
		return nil, fmt.Errorf("%w: no RalodePlayer player data on page", contracts.ErrNotFound)
	}

	var dubMap map[string]anitokyoDub
	dec := json.NewDecoder(bytes.NewReader(body[idx+len(call):]))
	if err := dec.Decode(&dubMap); err != nil {
		return nil, fmt.Errorf("%w: decode RalodePlayer blob: %w", contracts.ErrExtractFailed, err)
	}

	dubs := make([]anitokyoDub, 0, len(dubMap))
	for _, dub := range dubMap {
		if strings.TrimSpace(dub.Name) == "" || len(dub.Items) == 0 {
			continue // a registered-but-empty dub slot contributes nothing
		}
		dubs = append(dubs, dub)
	}
	return dubs, nil
}

// ResolveStream resolves one dub's embed: the /video.php wrapper page is
// fetched, its single player iframe scraped (kodik for codetype 110, a
// sibnet shell for codetype 13) and run through the shared extractor
// factory. A dub the episode does not carry is the typed not-found; a
// wrapper without its player iframe is the typed extract wall.
func (p *AniTokyo) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	refs, ok := episode.RawEmbeds[dubID]
	if !ok || len(refs) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrNotFound, episode.Num, dubID))
	}

	embeds := make([]string, 0, len(refs))
	for _, ref := range refs {
		resp, err := p.http.Get(ctx, ref, nil)
		if err != nil {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
		}
		src := anitokyoEmbedSrc(resp.Body)
		if src == "" {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
				fmt.Errorf("%w: video.php wrapper has no player iframe (%s)", contracts.ErrExtractFailed, ref))
		}
		embeds = append(embeds, animemobiAbsURL(p.baseURL, src))
	}

	sources, err := resolveEmbeds(ctx, p.http, embeds)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	if len(sources) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: no extractor yielded links for episode %s dub %q",
				contracts.ErrExtractFailed, episode.Num, dubID))
	}

	links := make(map[string]contracts.VideoSource, len(sources))
	for quality, src := range sources {
		src.Type = anitokyoStreamType(src.URL)
		links[quality] = src
	}
	stream.Links = links
	return stream, nil
}

// anitokyoEmbedSrc scrapes the player iframe src off a video.php wrapper
// page; "" when the page carries none.
func anitokyoEmbedSrc(body []byte) string {
	m := anitokyoEmbedRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// anitokyoStreamType labels a link by its URL shape: HLS manifests play
// as m3u8, everything else (the kodik and sibnet mp4 files) as mp4
// (animemobi/anistar precedent).
func anitokyoStreamType(u string) string {
	if strings.Contains(u, ".m3u8") {
		return "m3u8"
	}
	return "mp4"
}

// amdAllDigits/amdSortNumeric are the package's shared numeric-sort
// helpers (animedia.go); the lssort ordering reuses them.
