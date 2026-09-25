package providers

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniMikuBase is the site root: beta.animiku.tokyo, the RU anime
// catalog advertising 4K/FHD quality tiers («Аниме в 4K», «Аниме в
// FHD» menu entries; the /4k/ and /fhd/ category pages — also reachable
// as ?do=cat&category=4k). The engine is DataLife Engine under a
// heavily customized template (60+ dle markers, /engine/ asset paths)
// with the mrdeath/aaparser module bridge to the kodik player stack —
// anonymous on every probed path (search, release pages, player bridge;
// live-verified 2026-09-25).
//
// No frozen Python original exists (anidub/anistar/animemobi
// precedent): the provider was written from the live site, probed and
// capture-verified 2026-09-25.
const AniMikuBase = "https://beta.animiku.tokyo"

// animikuPlayerBridge is the mrdeath/aaparser AJAX endpoint that
// renders the kodik player for a release. It answers POST
// (news_id=<id>&action=load_player) with the player HTML fragment; the
// same URL asked with GET answers HTTP 200 and an EMPTY body
// (live-verified 2026-09-25), so the POST form is load-bearing. No
// Referer, cookies or X-Requested-With header are required.
const animikuPlayerBridge = "/engine/ajax/controller.php?mod=anime_grabber&module=kodik_playlist_ajax"

// AniMiku is the beta.animiku.tokyo provider: the RU DLE catalog whose
// release pages embed the kodik player through the aaparser bridge.
// One bridge answer carries the whole episode graph — the translator
// row IS the dub list («MC Entertainment», «СВ-Дубль», «Субтитры», …),
// serials add the b-simple_episode__item grid (data-this_episode ×
// data-this_translator × data-this_link protocol-relative
// kodikplayer.com refs), movies carry the link on the translator item
// itself (the kodik_translates_alt shape). The dub↔episode matrix is
// sparse (not every dub covers every episode), so each episode keeps
// only the dubs that list it. All embeds resolve through the shared
// kodik extractor factory.
//
// Quality tiers (archaeology note, 2026-09-25): the player switcher
// also lists «AniLiberty (FHD)» and — on 4K-category releases — the
// ad-free 4K player, but both are RUNTIME resolvers (site-hosted
// /animiku/libplayer/v3/?query=<title> iframe that searches the
// anilibria.top API by title client-side; the old anilibria.tv iframe
// field is dead, 410 Gone, per the site's own template comments). No
// deterministic embed URL exists to extract, so the kodik player — the
// site's default and per-episode source — is the stream contract here;
// the FHD/4K tiers stay documented, not wired.
//
// NamePreference: deliberately NOT declared (PR42 semantics) — the DLE
// index matches Cyrillic word prefixes with е/ё equivalence («черная
// лагуна» surfaced 4 rows live 2026-09-25), so the provider stays in
// the RU group and the shared smoke probe hits (no SmokeQuery
// declaration either, PR51 semantics).
type AniMiku struct {
	Base
}

// newAnimiku builds the provider against the given base. The shared
// netclient is used for every leg (search, player bridge): the site
// answers browser-fingerprint requests normally, no challenge anywhere.
func newAnimiku(base string, http *netclient.Client) *AniMiku {
	return &AniMiku{
		Base: Base{
			id:          "animiku",
			name:        "AniMiku",
			baseURL:     base,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
	}
}

// Search GETs the DLE full-search form (the site header form is
// method=get: /index.php?do=search&subaction=search&story=<query> —
// unlike animemobi's POST variant) and parses the answer's
// article.news-container-chapter rows: the first newsid anchor carries
// the release URL, p.title-text the title, img.xfieldimage the poster
// (relative /uploads/ paths absolutized; absolute CDN URLs pass
// through). Rows without a newsid link (site service pages) drop out.
//
// DLE caps the form answer at one page; no pagination is attempted
// (yummy/anilib/anistar/animemobi single-page precedent).
func (p *AniMiku) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	q := url.Values{}
	q.Set("do", "search")
	q.Set("subaction", "search")
	q.Set("story", query)
	resp, err := p.http.Get(ctx, p.baseURL+"/index.php?"+q.Encode(), nil)
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
	doc.Find("article.news-container-chapter").Each(func(_ int, row *goquery.Selection) {
		link := row.Find("a[href*='newsid=']").First()
		href, ok := link.Attr("href")
		if !ok || href == "" || seen[href] {
			return
		}
		seen[href] = true
		title := strings.TrimSpace(row.Find("p.title-text").First().Text())
		poster, _ := row.Find("img.xfieldimage").First().Attr("src")
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      href,
			SourceID: p.ID(),
			Poster:   animikuAbsURL(p.baseURL, poster),
		})
	})
	return results, nil
}

// animikuAbsURL absolutizes the root-relative asset URLs the markup
// mixes freely (the /uploads/ posters); absolute https passes through
// (animemobiAbsURL precedent).
func animikuAbsURL(base, u string) string {
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

// GetEpisodes fetches ONE bridge answer and maps it onto episodes. The
// release id is the newsid query param — the only release URL shape
// this catalog issues (search rows and category cards both link
// index.php?newsid=N; the canonical link repeats it) — so a URL
// without it is the typed invalid-input, and the release page itself
// never needs fetching.
//
// Serial shape: li.b-simple_episode__item rows carry data-this_episode
// (the episode number), data-this_translator (a kodik translation id
// resolved against the li.b-translator__item row for the dub name) and
// data-this_link (the protocol-relative kodikplayer.com embed — kept
// verbatim; the shared extractor normalizes the scheme). Episode order
// is first-seen; a repeated (episode, dub) pair overwrites, matching
// resolveEmbeds' dict.update merge. Multi-season kodik releases do not
// occur on this site (each release pins ONE season bucket — the 500-ep
// Shippuden ships a single continuous 1..500 grid, live-verified), so
// episode numbers are used as-is.
//
// Movie shape (also the OVA shape on this site): the episode grid is
// absent and each li.b-translator__item carries its own data-this_link
// — the release collapses to one episode keyed "1" listing every dub.
//
// An answer with neither a grid nor per-dub links is the typed
// not-found.
func (p *AniMiku) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	parsed, err := url.Parse(animeURL)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: unparseable release url %s: %w", contracts.ErrInvalidInput, animeURL, err))
	}
	newsID := parsed.Query().Get("newsid")
	if newsID == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: release url carries no newsid: %s", contracts.ErrInvalidInput, animeURL))
	}

	resp, err := p.http.PostForm(ctx, p.baseURL+animikuPlayerBridge, map[string][]string{
		"news_id": {newsID},
		"action":  {"load_player"},
	}, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if err != nil {
		return nil, err
	}

	doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if parseErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse player bridge answer: %w", parseErr))
	}

	// Dub names: kodik translation id -> voice-over name.
	dubs := map[string]string{}
	doc.Find("li.b-translator__item[data-this_translator]").Each(func(_ int, li *goquery.Selection) {
		id, _ := li.Attr("data-this_translator")
		name := strings.TrimSpace(li.Text())
		if id != "" && name != "" {
			dubs[id] = name
		}
	})

	// Serial shape: group the episode grid by episode number.
	type group struct {
		num   string
		title string
		refs  map[string]string // dub name -> embed ref
	}
	var order []string
	byNum := map[string]*group{}
	grid := false
	doc.Find("li.b-simple_episode__item[data-this_episode]").Each(func(_ int, li *goquery.Selection) {
		grid = true
		num := strings.TrimSpace(li.AttrOr("data-this_episode", ""))
		if num == "" {
			return
		}
		ref, ok := li.Attr("data-this_link")
		if !ok || ref == "" {
			return
		}
		dub := dubs[strings.TrimSpace(li.AttrOr("data-this_translator", ""))]
		if dub == "" {
			// Unknown translation id: key by the id itself — never
			// silently drop an embed.
			dub = strings.TrimSpace(li.AttrOr("data-this_translator", "?"))
		}
		g, ok := byNum[num]
		if !ok {
			g = &group{num: num, title: strings.TrimSpace(li.Text()), refs: map[string]string{}}
			byNum[num] = g
			order = append(order, num)
		}
		g.refs[dub] = ref
	})

	// Movie shape: no grid, per-dub links on the translator items.
	if !grid {
		mov := &group{num: "1", refs: map[string]string{}}
		doc.Find("li.b-translator__item[data-this_link]").Each(func(_ int, li *goquery.Selection) {
			ref, _ := li.Attr("data-this_link")
			name := strings.TrimSpace(li.Text())
			if ref != "" && name != "" {
				mov.refs[name] = ref
			}
		})
		if len(mov.refs) > 0 {
			order = append(order, mov.num)
			byNum[mov.num] = mov
		}
	}

	if len(order) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: no episodes or dub links in the player answer for newsid %s", contracts.ErrNotFound, newsID))
	}

	episodes := make([]contracts.Episode, 0, len(order))
	for _, num := range order {
		g := byNum[num]
		embeds := make(map[string][]string, len(g.refs))
		for dub, ref := range g.refs {
			embeds[dub] = []string{ref}
		}
		episodes = append(episodes, contracts.Episode{
			Num:       g.num,
			Title:     g.title,
			RawID:     newsID + ":" + num,
			RawEmbeds: embeds,
		})
	}
	return episodes, nil
}

// ResolveStream resolves the dub's embed ref through the shared extractor
// factory (the kodik extractor Matches the kodikplayer.com host; an
// unresolvable embed surfaces as the factory's typed failure, never a
// silent zero). The returned sources type by URL shape: kodik ladders
// are HLS manifests (.m3u8), everything else plays progressive
// (animemobi's labeling precedent).
func (p *AniMiku) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
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
		src.Type = animikuStreamType(src.URL)
		links[quality] = src
	}
	return contracts.MediaStream{DubName: dubID, Links: links}, nil
}

// animikuStreamType labels a link by its URL shape: the kodik ladders
// end in .m3u8 HLS manifests, everything else plays as mp4.
func animikuStreamType(u string) string {
	if strings.Contains(u, ".m3u8") {
		return "m3u8"
	}
	return "mp4"
}
