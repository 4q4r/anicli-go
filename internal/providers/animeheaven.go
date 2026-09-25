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

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeHeavenBase is the site root [LIVE-VERIFIED 2026-09-25].
const AnimeHeavenBase = "https://animeheaven.me"

// ahServiceDub is the single dub name every animeheaven episode
// carries: the site is sub-only (JA audio, EN subs — no dub option
// anywhere on the player), and the one unnamed source needs a stable
// key (the avServiceDub precedent).
const ahServiceDub = "Sub"

// ahGateKeyRe lifts the gate key out of an episode anchor's
// onmouseover/onmouseover attribute: gateh("…") opens the episodes
// menu, gatea("…") opens the player — both carry the same key. The
// LIVE markup single-quotes its attributes and puts a SPACE after the
// paren (onmouseover='gateh( "150ade…")', captured 2026-09-25), which
// the reference scrapers' gate[ha]\(" regex no longer matches; \s*
// tolerates both shapes.
var ahGateKeyRe = regexp.MustCompile(`gate[ha]\(\s*"([^"]+)"`)

// ahNumRe is the episode number text (div.watch2): digits only —
// animeheaven numbers are plain integers.
var ahNumRe = regexp.MustCompile(`\d+`)

// AnimeHeaven is the animeheaven.me provider (EN sub-only catalog).
// Not a port of a frozen anicli-py source: written against the live
// site and the AniVault scraper family (SH0MIK/Anivault-Scraper,
// jsmat0m/Anivault-Scraper — src/scrapers/animeheaven.ts), re-verified
// live 2026-09-25. The site is NOT behind Cloudflare (the reference
// skips FlareSolverr too). Three anonymous legs:
//
//   - /fastsearch.php?xhr=1&s=<query>: anchor cards a[href*="anime.php?"],
//     the id IS the href query part; div.fastname title with an
//     img[alt] fallback.
//   - /anime.php?<id>: per-episode anchors a[onmouseover*="gateh("] /
//     a[onclick*="gatea("] — the gate key is the quoted gateh/gatea
//     argument, the number is the div.watch2 text; rendered
//     newest-first, the provider sorts ascending.
//   - /gate.php with Cookie: key=<episode key> (stateless — a cold
//     cookie jar with only that cookie answers; verified live) and the
//     site Referer: a <video> element of DIRECT mp4 <source>s. The
//     first /video.mp4 source is the playable edge (rk.animeheaven.me,
//     HTTP 206 with Range support); the 2nd/3rd sources are the site's
//     own onerror-fallback CDNs (&error / &error2 suffixes — direct
//     hits answer 404) and are naturally never picked by the
//     first-match rule.
type AnimeHeaven struct {
	Base
}

// newAnimeHeaven builds the provider against baseURL. The site answers
// plain desktop-UA requests (verified via curl and the netclient
// fingerprint through the live probe), so no extra default headers are
// sent beyond what each leg explicitly needs.
func newAnimeHeaven(baseURL string, http *netclient.Client) *AnimeHeaven {
	return &AnimeHeaven{
		Base: Base{
			id:          "animeheaven",
			name:        "AnimeHeaven",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
	}
}

// NamePreference reports the latin-only search index (PR42): the
// catalog is EN; romaji/english titles match, Cyrillic queries are
// guaranteed-zero.
func (p *AnimeHeaven) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// Search GETs the fastsearch AJAX endpoint and scrapes the anchor
// cards [LIVE-VERIFIED 2026-09-25: GET /fastsearch.php?xhr=1&s=black
// +lagoon → HTTP 200, 3 cards (Roberta's Blood Trail, The Second
// Barrage, Black Lagoon); junk query → HTTP 200 «No results found»
// shell, zero cards]. The query rides url.Values encoding (spaces
// "+", the reference axios shape).
func (p *AnimeHeaven) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	q := url.Values{"xhr": {"1"}, "s": {query}}
	resp, err := p.http.Get(ctx, p.baseURL+"/fastsearch.php?"+q.Encode(),
		map[string]string{"Accept": "text/html,*/*"})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	var results []contracts.SearchResult
	doc.Find(`a[href*="anime.php?"]`).Each(func(_ int, card *goquery.Selection) {
		href, ok := card.Attr("href")
		if !ok {
			return
		}
		// The id IS the href query part ("/anime.php?11t3p" → "11t3p").
		_, rawID, found := strings.Cut(href, "?")
		rawID = strings.TrimSpace(rawID)
		if !found || rawID == "" {
			return
		}

		title := strings.TrimSpace(card.Find(".fastname").First().Text())
		if title == "" {
			title = strings.TrimSpace(card.Find("img").AttrOr("alt", ""))
		}
		if title == "" {
			return
		}

		poster := ""
		if src := strings.TrimSpace(card.Find("img").AttrOr("src", "")); src != "" {
			poster = ahAbsolute(src, p.baseURL)
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      p.baseURL + "/anime.php?" + rawID,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// ahEpisode is one parsed episode anchor: the gate key and number.
type ahEpisode struct {
	key string
	num int
}

// GetEpisodes fetches the anime page and rebuilds the episode list
// [LIVE-VERIFIED 2026-09-25 on Naruto Shippuden (nc7bk): 71 anchors,
// rendered 71→1, distinct gate keys; ep-1 key
// 1383adfc6a074fcceed863c8b9e2b5db]. The page's anchors point at
// gate.php and carry the key in their gateh/gatea handlers; the
// number is the div.watch2 text. Entries dedupe by key (the reference
// rule) and sort ascending by number. Every episode carries the ONE
// sub dub, whose embed reference is the gate key itself — ResolveStream
// turns it into the cookie the gate.php leg needs.
func (p *AnimeHeaven) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	rawID, err := ahAnimeID(animeURL)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: %w", contracts.ErrInvalidInput, err))
	}

	resp, err := p.http.Get(ctx, p.baseURL+"/anime.php?"+rawID, nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	seen := make(map[string]bool)
	var entries []ahEpisode
	doc.Find(`a[onmouseover*="gateh("], a[onclick*="gatea("]`).Each(func(_ int, el *goquery.Selection) {
		attr, ok := ahGateAttr(el)
		if !ok {
			return
		}
		key := ahParseGateKey(attr)
		if key == "" || seen[key] {
			return
		}
		rawNum := strings.TrimSpace(el.Find(".watch2").First().Text())
		m := ahNumRe.FindString(rawNum)
		if m == "" {
			return
		}
		num, err := strconv.Atoi(m)
		if err != nil {
			return
		}
		seen[key] = true
		entries = append(entries, ahEpisode{key: key, num: num})
	})
	// The page renders newest-first; the contract expects ascending.
	sort.Slice(entries, func(i, j int) bool { return entries[i].num < entries[j].num })

	// A parsed page with zero gate anchors is a typed wall: an empty
	// list here would fake a healthy title with no episodes (the
	// roster doctrine — anizone/animedia/anikado/anitokyo/animiku/
	// animevib; this site's own space-after-paren drift shows the
	// markup-shift case is real).
	if len(entries) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("anime page carries no gate episode anchors: %w", contracts.ErrNotFound))
	}

	episodes := make([]contracts.Episode, 0, len(entries))
	for _, e := range entries {
		num := strconv.Itoa(e.num)
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: "Episode " + num,
			RawID: e.key,
			RawEmbeds: map[string][]string{
				ahServiceDub: {e.key},
			},
		})
	}
	return episodes, nil
}

// ResolveStream opens the gate: GET /gate.php with the episode key as
// the key cookie and the site root as Referer [LIVE-VERIFIED
// 2026-09-25 on the ep-1 key above: HTTP 200, four <source> elements,
// the primary rk.animeheaven.me/video.mp4 answering HTTP 206
// video/mp4 with Range support — both with and without the Referer;
// the &error-fallback CDNs 404 when hit directly]. The link picks the
// FIRST /video.mp4 source (the reference rule), degrading to the first
// http(s) source; no source is a typed extraction failure. The site
// exposes no quality selector — the captured file's MP4 tkhd reports
// 928x720, so the single link is labelled 720. The Referer rides on
// the link (the anistar convention): the edge answers without it
// today, but the gate pins playback to the site origin and the header
// is load-bearing the moment that tightens.
func (p *AnimeHeaven) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	// A dub the episode does not carry is a caller bug (wave A review
	// F2, animedia precedent, mirrored from animevib): a silent empty
	// MediaStream would read as a healthy resolution.
	embeds, ok := episode.RawEmbeds[dubID]
	if !ok || len(embeds) == 0 || embeds[0] == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrInvalidInput, episode.Num, dubID))
	}

	referer := p.baseURL + "/"
	resp, err := p.http.Get(ctx, p.baseURL+"/gate.php", map[string]string{
		"Cookie":  "key=" + embeds[0],
		"Referer": referer,
		"Accept":  "text/html,*/*",
	})
	if err != nil {
		return stream, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("parse gate page: %w", err))
	}

	primary, fallback := "", ""
	doc.Find("video source").Each(func(_ int, el *goquery.Selection) {
		src := strings.TrimSpace(el.AttrOr("src", ""))
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			return
		}
		if fallback == "" {
			fallback = src
		}
		if primary == "" && strings.Contains(src, "/video.mp4") {
			primary = src
		}
	})
	if primary == "" {
		primary = fallback
	}
	if primary == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("gate page carries no playable source: %w", contracts.ErrExtractFailed))
	}

	stream.Links["720"] = contracts.VideoSource{
		URL:     primary,
		Quality: "720",
		Type:    "mp4",
		Headers: map[string]string{"Referer": referer},
	}
	return stream, nil
}

// ahAnimeID extracts the anime id from a search-result URL: everything
// after the "?" (the id IS the query part — the site's own addressing
// scheme). A URL without one is caller error.
func ahAnimeID(animeURL string) (string, error) {
	parsed, err := url.Parse(animeURL)
	if err != nil {
		return "", fmt.Errorf("parse anime url %q: %w", animeURL, err)
	}
	rawID := strings.TrimSpace(parsed.RawQuery)
	if rawID == "" {
		return "", fmt.Errorf("anime url %q carries no id query part", animeURL)
	}
	return rawID, nil
}

// ahGateAttr returns the episode anchor's gate attribute: the
// onmouseover when present, the onclick otherwise.
func ahGateAttr(el *goquery.Selection) (string, bool) {
	if attr, ok := el.Attr("onmouseover"); ok && strings.TrimSpace(attr) != "" {
		return attr, true
	}
	attr, ok := el.Attr("onclick")
	return attr, ok && strings.TrimSpace(attr) != ""
}

// ahParseGateKey lifts the quoted gateh/gatea argument out of an anchor
// attribute; "" when the attribute is not a gate handler.
func ahParseGateKey(attr string) string {
	m := ahGateKeyRe.FindStringSubmatch(attr)
	if m == nil {
		return ""
	}
	return m[1]
}

// ahAbsolute resolves a site-relative URL against the site root (the
// reference absoluteUrl(url, BASE) semantics).
func ahAbsolute(src, baseURL string) string {
	parsed, err := url.Parse(src)
	if err != nil {
		return src
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return src
	}
	return base.ResolveReference(parsed).String()
}
