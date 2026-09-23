package providers

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeMobiBase is the site root: animemobi.com, the RU mobile-oriented
// anime catalog («бесплатно и без регистрации», streamed and downloaded
// straight off the site's own releases). The site is a DataLife Engine
// catalog on UTF-8 (unlike anistar's cp1251) with an anonymous, plain-HTTPS
// surface — no challenge on any probed path (2026-09-23). A serving mirror
// exists at animemobi.top (the download endpoint redirects to it; see the
// download note on ResolveStream).
//
// No frozen Python original exists (anidub/anistar precedent): the provider
// was written from the live site, probed and capture-verified 2026-09-23.
const AnimeMobiBase = "https://animemobi.com"

// animemobiSmokeQuery is the declared live probe (PR51 mechanism): the
// shared RU probe «черная лагуна» MISSES the catalog — the site titles
// inflect («Пираты «Черной лагуны»») and DLE's word-prefix search never
// matches «лагуна» against «лагуны» (live-verified 2026-09-23: 24 menu
// cards, zero shortstory rows). The declared probe «боруто» surfaces the
// current Boruto release first, whose 293 episode anchors all ride the
// reachable kodikplayer.com embed host.
const animemobiSmokeQuery = "боруто"

// animemobiDubFallback is the dub key for releases whose page carries no
// «Озвучка:» credit (the site's own subtitled /anime-sub/ section among
// them) — the catalog's own name, mirroring anistar's fallback pattern.
const animemobiDubFallback = "AnimeMobi"

// animemobiDubRe digs the voice-over credit out of the release page's
// file-info block («<b>Озвучка:</b> &#91;Многоголосый]<br>»). The leading
// «Команда Озвучки:» field never matches — its text is «Озвучки», not
// «Озвучка:</b>». Entity-decoded by the caller; the credit may carry
// literal [ ] brackets around the team names, which are stripped.
var animemobiDubRe = regexp.MustCompile(`Озвучка:</b>\s*(?:<[^>]+>)*\s*([^<]+)`)

// animemobiSeriesRe / animemobiMovieRe split an anchor label into the
// episode number and the rest. Observed shapes: «Серия 01»… «Серия 12»,
// «Фильм 01», and the label-free whole-season «Смотреть» (falls through
// to episode 1). Zero-padded numbers normalize to canonical decimals
// ("01" → "1").
var (
	animemobiSeriesRe = regexp.MustCompile(`^Серия\s+(\d+)`)
	animemobiMovieRe  = regexp.MustCompile(`^Фильм\s+(\d+)`)
)

// AnimeMobi is the animemobi.com provider: the RU DLE mobile catalog with
// its torrent-download section and kodik-family players (the kodikplayer.com
// and aniqit.com embed hosts both resolve through the shared extractor
// factory). One voice-over per release (the «Озвучка:» credit keys the dub),
// one anchor per episode — episodes hydrate eagerly, no DubsHydrator
// capability.
//
// NamePreference: deliberately NOT declared (PR42 semantics) — the DLE
// index matches the Cyrillic fragments of the composite site titles
// («наруто» verified live 2026-09-23), so the provider stays in the RU
// group and the search fan-out routes it the RU variants
// (anilibria-torrent precedent).
type AnimeMobi struct {
	Base
}

// newAnimeMobi builds the provider against the given base. The shared
// netclient is used for every leg (search, release page, embed
// resolution): the site answers browser-fingerprint requests normally.
func newAnimeMobi(base string, http *netclient.Client) *AnimeMobi {
	return &AnimeMobi{
		Base: Base{
			id:          "animemobi",
			name:        "AnimeMobi",
			baseURL:     base,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
	}
}

// SmokeQuery reports the provider-specific live probe; see
// animemobiSmokeQuery.
func (p *AnimeMobi) SmokeQuery() string { return animemobiSmokeQuery }

// animemobiAnimeSection reports whether a release URL belongs to the two
// playable anime sections. The DLE full-search mixes site news, manga
// reader pages, AMVs, covers and top-lists into the answer (the "black
// lagoon" capture: 10 rows, only 3 anime releases — live-verified
// 2026-09-23); the search fan-out must surface playable releases only,
// so everything outside /anime-rus/ and /anime-sub/ is dropped (anistar's
// news/manga filter precedent).
func animemobiAnimeSection(u string) bool {
	return strings.Contains(u, "/anime-rus/") || strings.Contains(u, "/anime-sub/")
}

// Search POSTs the DLE full-search form to the site root
// (do=search&subaction=search&story=<query>, UTF-8 — unlike anistar's
// cp1251 form) and parses the answer's result rows. The operator's
// User-Agent decides which DLE skin the site renders, and the skins
// mark result rows differently (both verified live 2026-09-23, same
// query, same answers):
//
//   - the smartphone skin (mobile UA): div.shortstory rows with
//     h2.title anchors and a div.story-img thumbnail;
//   - the desktop skin (desktop UA): div.base rows with div.bheading
//     h1.heading anchors and the poster inside div.maincont.
//
// The row's heading anchor carries the release URL and title;
// SearchResult.URL is the release page URL — GetEpisodes keys on it.
//
// DLE caps the form answer at one page (~10 rows); no pagination is
// attempted (yummy/anilib/anistar single-page precedent).
func (p *AnimeMobi) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := map[string][]string{
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

	doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if parseErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", parseErr))
	}

	results := make([]contracts.SearchResult, 0, 10)
	seen := map[string]bool{}
	// Both skins' heading anchors, one traversal: the selector only
	// matches rows carrying a result link, so the desktop skin's
	// form container (a div.base without bheading) drops out on its own.
	doc.Find("div.shortstory h2.title a[href], div.base div.bheading h1.heading a[href]").Each(func(_ int, link *goquery.Selection) {
		href, ok := link.Attr("href")
		if !ok || href == "" || !animemobiAnimeSection(href) {
			return
		}
		if seen[href] {
			return
		}
		seen[href] = true
		title := strings.TrimSpace(link.Text())
		// The row container differs per skin; the poster img inside it
		// does not (an /uploads/ thumbnail in both).
		poster := ""
		if row := link.Closest("div.shortstory, div.base"); row.Length() > 0 {
			poster, _ = row.Find("img[src*='/uploads/']").First().Attr("src")
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

// animemobiAbsURL absolutizes the root-relative asset URLs the markup
// mixes freely (the /uploads/ posters); absolute https passes through.
func animemobiAbsURL(base, u string) string {
	switch {
	case u == "":
		return ""
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "/"):
		return base + u
	default:
		return u
	}
}

// animemobiEpisodeNum maps an anchor label onto the episode number: the
// «Серия NN»/«Фильм NN» labels carry zero-padded numbers (normalized to
// canonical decimals), any other label («Смотреть» — the whole-season
// anchor) counts as episode 1.
func animemobiEpisodeNum(label string) string {
	m := animemobiSeriesRe.FindStringSubmatch(label)
	if m == nil {
		m = animemobiMovieRe.FindStringSubmatch(label)
	}
	if m == nil {
		return "1"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return m[1]
	}
	return strconv.Itoa(n)
}

// animemobiDubName extracts the release's voice-over credit («Озвучка:»
// field, entity-decoded, [ ] brackets stripped) and falls back to
// animemobiDubFallback when the page credits none (the /anime-sub/
// section).
func animemobiDubName(page string) string {
	m := animemobiDubRe.FindStringSubmatch(page)
	if m == nil {
		return animemobiDubFallback
	}
	dub := strings.TrimSpace(html.UnescapeString(m[1]))
	dub = strings.TrimSuffix(strings.TrimPrefix(dub, "["), "]")
	if dub == "" {
		return animemobiDubFallback
	}
	return dub
}

// GetEpisodes fetches the release page and maps its a.onlinevideo anchors
// onto episodes: each anchor is one (episode, embed) pair — «Серия NN»
// labels the per-episode kodikplayer.com /seria/ links, «Фильм NN» the
// movies, and a label-free «Смотреть» anchor is the whole-season
// /season/ link counted as episode 1. The page's single «Озвучка:» credit
// keys the dub in RawEmbeds (one voice-over per release on this site);
// its values ride through the shared extractor factory at resolve time.
//
// A page without anchors (site news share the .html URL shape) is the
// typed not-found.
func (p *AnimeMobi) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}
	page := string(resp.Body)
	dub := animemobiDubName(page)

	doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(page))
	if parseErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse release page: %w", parseErr))
	}

	type group struct {
		num      string
		title    string
		embedRef string
	}
	var order []string
	byNum := map[string]*group{}
	doc.Find("a.onlinevideo[href]").Each(func(_ int, anchor *goquery.Selection) {
		ref, ok := anchor.Attr("href")
		if !ok || ref == "" {
			return
		}
		label := strings.TrimSpace(anchor.Text())
		num := animemobiEpisodeNum(label)
		g, ok := byNum[num]
		if !ok {
			g = &group{num: num, title: label}
			byNum[num] = g
			order = append(order, num)
		}
		g.embedRef = ref
	})
	if len(order) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: no onlinevideo anchors on page %s", contracts.ErrNotFound, animeURL))
	}

	episodes := make([]contracts.Episode, 0, len(order))
	for _, num := range order {
		g := byNum[num]
		episodes = append(episodes, contracts.Episode{
			Num:       g.num,
			Title:     g.title,
			RawID:     g.num,
			RawEmbeds: map[string][]string{dub: {g.embedRef}},
		})
	}
	return episodes, nil
}

// ResolveStream resolves the dub's embed ref through the shared extractor
// factory (the kodik extractor Matches both the kodikplayer.com and
// aniqit.com hosts; older aniqit.com releases may be unresolvable from
// non-CIS networks — that surfaces as the factory's typed failure, never
// a silent zero). The returned sources type by URL shape: kodik ladders
// are HLS manifests (.m3u8), everything else plays progressive.
func (p *AnimeMobi) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	refs, ok := episode.RawEmbeds[dubID]
	if !ok || len(refs) == 0 {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("%w: dub %q has no embed references on episode %s", contracts.ErrNotFound, dubID, episode.Num))
	}

	sources, err := resolveEmbeds(ctx, p.http, refs)
	if err != nil {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	if len(sources) == 0 {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("%w: no playable links for dub %q episode %s", contracts.ErrExtractFailed, dubID, episode.Num))
	}

	links := make(map[string]contracts.VideoSource, len(sources))
	for quality, src := range sources {
		src.Type = animemobiStreamType(src.URL)
		links[quality] = src
	}
	return contracts.MediaStream{DubName: dubID, Links: links}, nil
}

// animemobiStreamType labels a link by its URL shape: the kodik ladders
// end in .m3u8 HLS manifests, everything else plays as mp4 (anistar's
// anistarStreamType precedent).
func animemobiStreamType(u string) string {
	if strings.Contains(u, ".m3u8") {
		return "m3u8"
	}
	return "mp4"
}

// Download note (surfacing decision, 2026-09-23): the release pages carry
// a per-release torrent download (<div class="yadisk"><a href="…/
// index.php?do=download&id=N">). The chain works anonymously: GET the
// release page once (PHPSESSID session), then GET the do=download URL
// with the release page as Referer — the endpoint 302s to the
// animemobi.top mirror and the follow-up answers the actual .torrent
// (application/x-bittorrent, tracker tr.animemobi.ru; live-verified).
// That is a download pipeline, not a playback one: it does not fit the
// stream contract (Search/GetEpisodes/ResolveStream), and the roster's
// established surface for DL content is a TorrentBase sibling provider
// (anilibria-torrent precedent) — a separate commission, not smuggled
// into this provider's episode graph.
