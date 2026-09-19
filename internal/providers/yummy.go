package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Yummy identity bases. site.yummyani.me is the catalog root the
// upstream Extractor.BASE_URL carries (anicli-api source/yummy_anime.py);
// the REST API itself lives on api.yani.tv (documented at
// https://yummy-anime.ru/api/swagger, swagger mirror in the upstream
// dev/swagger/yummy_anime.yml). The SSR mirror yummyanime.in is dead
// (HTTP 410, verified 2026-09-19) — the API surface is the only live
// one. The {"response": ...} envelope and the HTTP-200 error envelope
// come from the upstream parser module (yummy_anime_me_parser.py).
const (
	YummySiteBase = "https://site.yummyani.me"
	YummyAPIBase  = "https://api.yani.tv"
	// yummyCDNVideoHubBase is the CDNVideoHub player API the
	// iframeCVH iframes resolve through (upstream
	// player/parsers/cdnvideohub_parser.py).
	yummyCDNVideoHubBase = "https://plapi.cdnvideohub.com"
)

// yummyCVHReextracts the player constants from the route-chunk JS the
// CVH iframe page loads (upstream PageJsCVHParams,
// yummy_anime_me_parser.py — patterns verbatim; verified live
// 2026-09-19 against the ZhP7BGdK chunk: publisher 745, aggregator
// "mali").
var (
	yummyCVHPubRe  = regexp.MustCompile(`"data-publisher-id":\s?(\d+)`)
	yummyCVHAggrRe = regexp.MustCompile(`"data-aggregator":\s?"([^"]+)"`)
)

// yummyCVHQualityMap ports _RESOLUTION_MAPPING (anicli-api
// player/cdnvideohub.py) — the mapping the video/<vkId> sources use,
// distinct from the animego cdn-iframe table in internal/extractors
// (that one ports anicli-py extractors.py, where 2k is absent and 4k is
// labeled 2160).
var yummyCVHQualityMap = map[string]int{
	"mpegTinyUrl":   144,
	"mpegLowestUrl": 240,
	"mpegLowUrl":    360,
	"mpegMediumUrl": 480,
	"mpegHighUrl":   720,
	"mpegFullHdUrl": 1080,
	"mpegQhdUrl":    1440,
	"mpeg2kUrl":     2048,
	"mpeg4kUrl":     4096,
}

// Yummy is the YummyAnime provider (PR68): the api.yani.tv JSON API of
// yummyanime (site.yummyani.me), a faithful port of anicli-api's
// source/yummy_anime.py onto the Go provider contract. Many dubbers;
// episode listings arrive with every dub's embed URL in ONE call, so
// dubs hydrate eagerly (no DubsHydrator capability).
type Yummy struct {
	Base
	// apiBase is the REST root (YummyAPIBase in production).
	apiBase string
	// cvhBase is the CDNVideoHub player API root the iframeCVH
	// iframes resolve through (yummyCDNVideoHubBase in production).
	cvhBase string
	// ua is the playback User-Agent echoed on CVH video sources: the
	// okcdn edge ties a playback session to the UA that fetched the
	// links (anicli-api README, okcdn row), so it must equal the UA
	// netclient itself sends.
	ua string
}

// newYummy builds the provider against the given bases. The shared
// netclient is used for every leg (API, iframe page, player JS, plapi):
// api.yani.tv answers browser-fingerprint requests normally (verified
// live 2026-09-19 — the animevost-style DIRECT plain client is NOT
// needed).
func newYummy(siteBase, apiBase, cvhBase, ua string, http *netclient.Client) *Yummy {
	return &Yummy{
		Base: Base{
			id:          "yummy",
			name:        "YummyAnime",
			baseURL:     siteBase,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		apiBase: apiBase,
		cvhBase: cvhBase,
		ua:      ua,
	}
}

// SmokeQuery declares the provider-specific live probe (PR68): the
// shared RU probe «черная лагуна» cannot reach «Пираты «Чёрной
// лагуны»» — the index matches single tokens and returns 20 unrelated
// «чёрная*» titles for it (live-verified 2026-09-19), while the
// substring «лагуна» surfaces the target at rank 1. The declared query
// gets no RU/latin fallback (dreamcast PR51 precedent).
func (p *Yummy) SmokeQuery() string { return "лагуна" }

// yummyEnvelope is the shared API envelope: {"response": ...} on
// success, {"error": ..., "error_title": ..., "error_code": ...,
// "error_name": ...} on failures (upstream YummyAnimeApiErr200Error —
// the error shape may arrive ON HTTP 200; the matcher is ported as the
// Error-field check).
type yummyEnvelope struct {
	Response   json.RawMessage `json:"response"`
	Error      string          `json:"error"`
	ErrorTitle string          `json:"error_title"`
	ErrorCode  json.Number     `json:"error_code"`
	ErrorName  string          `json:"error_name"`
}

// yummyAbsURL absolutizes the protocol-relative URLs the API emits
// (posters, iframe URLs: "//static.yani.tv/...") onto https. Divergence
// from upstream (which passes them raw): the TUI downloads posters and
// hands streams to mpv, both of which need an absolute URL.
func yummyAbsURL(u string) string {
	switch {
	case u == "":
		return ""
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	default:
		return u
	}
}

// yummyAnimeItem mirrors the search-hit fields the provider consumes
// (upstream AnimeItemJson via Search.get_anime).
type yummyAnimeItem struct {
	AnimeID  json.Number `json:"anime_id"`
	Title    string      `json:"title"`
	AnimeURL string      `json:"anime_url"`
	Poster   struct {
		Medium string `json:"medium"`
	} `json:"poster"`
}

// Search GETs /anime?q=<query>&offset=0&limit=20 (upstream
// Extractor.search, yummy_anime.py:21-36). SearchResult.URL carries the
// anime_id — the videos endpoint keys on it (GetEpisodes); the
// id-as-URL contract follows the animevost precedent
// (contracts.SearchResult.URL: "direct URL ... or a unique slug").
// A miss answers HTTP 200 {"response":[]} (live-verified 2026-09-19) —
// zero results, no error; the HTTP-200 error envelope surfaces as the
// typed miss.
func (p *Yummy) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{"q": {query}, "offset": {"0"}, "limit": {"20"}}
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.apiBase + "/anime?" + params.Encode(),
		Op:      contracts.OpSearch,
		Headers: map[string]string{"Accept": "application/json"},
	})
	if err != nil {
		return nil, err
	}

	var env yummyEnvelope
	if jsonErr := json.Unmarshal(resp.Body, &env); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", jsonErr))
	}
	if env.Error != "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrNotFound, env.Error))
	}

	var items []yummyAnimeItem
	if jsonErr := json.Unmarshal(env.Response, &items); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search hits: %w", jsonErr))
	}

	results := make([]contracts.SearchResult, 0, len(items))
	for _, item := range items {
		results = append(results, contracts.SearchResult{
			Title:    item.Title,
			URL:      pythonStr(item.AnimeID),
			SourceID: p.ID(),
			Poster:   yummyAbsURL(item.Poster.Medium),
		})
	}
	return results, nil
}

// yummyVideoItem mirrors one row of /anime/{id}/videos (upstream
// VideoItemJson): every row is one (episode, dub, player) triple.
type yummyVideoItem struct {
	VideoID   json.Number `json:"video_id"`
	Number    string      `json:"number"`
	IframeURL string      `json:"iframe_url"`
	Data      struct {
		Player  string `json:"player"`
		Dubbing string `json:"dubbing"`
	} `json:"data"`
}

// GetEpisodes GETs /anime/<animeURL>/videos (upstream
// Anime.get_episodes: the search hit's anime_id keys the endpoint).
//
// Divergence from the literal Python grouping (documented, PR68): the
// wire keys episodes by the RAW number string, and the catalog mixes
// zero-padded and bare forms ("01".."12" then "1".."9" on the live
// 1080 payload) — the literal dict would surface the same episode
// twice. Upstream's own Episode.ordinal=int(num) declares the int to be
// the canonical identity, so the Go port groups on it: numeric numbers
// are canonicalized, first-seen order is kept, links append. Non-numeric
// numbers ("2.5", "OVA") pass through verbatim.
//
// Further deltas, all upstream-mandated by the Go contracts: episode
// titles stay empty (the API carries none; upstream's "Episode" is its
// default-naming convention) and alloha iframes are NOT skipped — the
// upstream comment skips them as "too complex reverse", but the Go
// extractor factory ports AllohaExtractor, so the links resolve.
func (p *Yummy) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.apiBase + "/anime/" + animeURL + "/videos",
		Op:      contracts.OpGetEpisodes,
		Headers: map[string]string{"Accept": "application/json"},
	})
	if err != nil {
		return nil, err
	}

	var env yummyEnvelope
	if jsonErr := json.Unmarshal(resp.Body, &env); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode videos response: %w", jsonErr))
	}
	if env.Error != "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrNotFound, env.Error))
	}

	var videos []yummyVideoItem
	if jsonErr := json.Unmarshal(env.Response, &videos); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode video rows: %w", jsonErr))
	}
	if len(videos) == 0 {
		// A nonexistent id answers HTTP 200 {"response":[]} (live
		// 2026-09-19): the typed miss replaces the Python silent [].
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: no videos for anime %s", contracts.ErrNotFound, animeURL))
	}

	type group struct {
		num  string
		dubs map[string][]string
	}
	order := make([]string, 0, 16)
	byNum := make(map[string]*group, 16)
	for _, video := range videos {
		num := video.Number
		if n, convErr := strconv.Atoi(num); convErr == nil {
			num = strconv.Itoa(n)
		}
		g, ok := byNum[num]
		if !ok {
			g = &group{num: num, dubs: map[string][]string{}}
			byNum[num] = g
			order = append(order, num)
		}
		g.dubs[video.Data.Dubbing] = append(g.dubs[video.Data.Dubbing], yummyAbsURL(video.IframeURL))
	}

	episodes := make([]contracts.Episode, 0, len(order))
	for _, num := range order {
		g := byNum[num]
		episodes = append(episodes, contracts.Episode{
			Num:       g.num,
			RawID:     g.num,
			RawEmbeds: g.dubs,
		})
	}
	return episodes, nil
}

// ResolveStream splits the dub's embed links the way upstream
// Source.get_videos does (yummy_anime.py:154-238): iframeCVH iframes
// take the dedicated CDNVideoHub chain, everything else walks the
// shared extractor factory (kodik, sibnet, alloha, aksor, …). Link
// results merge with dict.update semantics (later links overwrite) —
// the resolveEmbeds convention; a failing link is remembered and only
// surfaces when NOTHING resolved.
func (p *Yummy) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	var firstErr error
	for _, link := range episode.RawEmbeds[dubID] {
		var (
			sources map[string]contracts.VideoSource
			err     error
		)
		if strings.Contains(link, "/iframeCVH.html?") {
			sources, err = p.resolveCVH(ctx, link)
		} else {
			sources, err = resolveEmbeds(ctx, p.http, []string{link})
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for quality, src := range sources {
			stream.Links[quality] = src // dict.update: later links overwrite
		}
	}

	if len(stream.Links) == 0 && firstErr != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, firstErr)
	}
	return stream, nil
}

// resolveCVH ports the upstream Source.get_videos special case for
// "/iframeCVH.html?" URLs (yummy_anime.py:163-200) plus
// video_playlist_from_vk_id (player/cdnvideohub.py:45-89):
//
//  1. fetch the iframe page and take the module script's src
//     (upstream extract_cvh_path, lxml cssselect — the goquery selector
//     is attribute-order independent the same way);
//  2. fetch that JS chunk and scrape the data-publisher-id /
//     data-aggregator constants (PageJsCVHParams — regexes verbatim);
//  3. GET the playlist with pub/aggr/anime_id and pick the item whose
//     episode and voiceStudio match the iframe's query — the
//     dubbing_code query value decodes "+" as a space so it equals the
//     playlist's voiceStudio key (upstream unquote_plus note);
//  4. GET video/<vkId> and map mpeg* qualities, with hls/dash appended
//     at the max quality.
//
// No matching playlist item is a legitimate empty (a studio listed in
// the source metadata with no video uploaded — upstream returns []):
// empty links, nil error.
func (p *Yummy) resolveCVH(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:cdnvideohub: %w: %s", contracts.ErrExtractFailed, reason)
	}

	parsed, err := url.Parse(yummyAbsURL(rawURL))
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable iframe url: %v", err))
	}
	base := parsed.Scheme + "://" + parsed.Host

	// 1. The iframe page: its single module script carries the player
	// route chunk.
	iframe, err := p.http.Get(ctx, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(iframe.Body))
	if err != nil {
		return nil, shape(fmt.Sprintf("parse iframe page: %v", err))
	}
	src, ok := doc.Find(`script[type="module"][crossorigin][src]`).First().Attr("src")
	if !ok || src == "" {
		return nil, shape("iframe page carries no module script")
	}
	jsURL := src
	switch {
	case strings.HasPrefix(src, "//"):
		jsURL = "https:" + src
	case !strings.HasPrefix(src, "http"):
		jsURL = base + src
	}

	// 2. The JS chunk: the player constants.
	js, err := p.http.Get(ctx, jsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	pub := yummyCVHPubRe.FindStringSubmatch(string(js.Body))
	aggr := yummyCVHAggrRe.FindStringSubmatch(string(js.Body))
	if pub == nil || aggr == nil {
		return nil, shape("player JS missing data-publisher-id/data-aggregator")
	}

	// 3. The iframe query carries the episode identity. url.Query
	// already decoded "+" as a space, so dubbing_code equals the
	// playlist's voiceStudio key exactly like upstream's unquote_plus.
	episodeStr := parsed.Query().Get("episode")
	animeID := parsed.Query().Get("anime_id")
	dubbingCode := parsed.Query().Get("dubbing_code")
	episode, convErr := strconv.Atoi(episodeStr)
	if convErr != nil || animeID == "" || dubbingCode == "" {
		return nil, shape("iframe query lacks anime_id/episode/dubbing_code")
	}

	plParams := url.Values{"pub": {pub[1]}, "aggr": {aggr[1]}, "id": {animeID}}
	playlist, err := p.http.Get(ctx, p.cvhBase+"/api/v1/player/sv/playlist?"+plParams.Encode(), map[string]string{
		"Accept": "application/json, text/plain, */*",
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	var plData struct {
		Items []struct {
			VkID        json.Number `json:"vkId"`
			Episode     json.Number `json:"episode"`
			VoiceStudio string      `json:"voiceStudio"`
		} `json:"items"`
	}
	if err := json.Unmarshal(playlist.Body, &plData); err != nil {
		return nil, shape(fmt.Sprintf("playlist json: %v", err))
	}

	vkID := ""
	for _, item := range plData.Items {
		if item.Episode.String() == strconv.Itoa(episode) && item.VoiceStudio == dubbingCode {
			vkID = item.VkID.String()
			break
		}
	}
	if vkID == "" {
		// Studio listed in the metadata but no video uploaded — the
		// legitimate empty outcome (upstream returns []).
		return map[string]contracts.VideoSource{}, nil
	}

	// 4. The video sources.
	video, err := p.http.Get(ctx, p.cvhBase+"/api/v1/player/sv/video/"+vkID, map[string]string{
		"Accept": "application/json, text/plain, */*",
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	var vidData struct {
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(video.Body, &vidData); err != nil {
		return nil, shape(fmt.Sprintf("video json: %v", err))
	}

	// Pop hls/dash first (upstream sources.pop), map the mpeg* ladder,
	// then append hls/dash AT the max quality (upstream appends them
	// with max_quality; the quality map collapses upstream's list —
	// dash, appended last, wins the key).
	hls := vidData.Sources["hlsUrl"]
	dash := vidData.Sources["dashUrl"]
	playbackUA := map[string]string{"User-Agent": p.ua}

	results := map[string]contracts.VideoSource{}
	maxQuality := 0
	for key, link := range vidData.Sources {
		if link == "" || key == "hlsUrl" || key == "dashUrl" {
			continue
		}
		quality, ok := yummyCVHQualityMap[key]
		if !ok {
			continue // unknown source key: upstream would label it 0
		}
		qs := strconv.Itoa(quality)
		results[qs] = contracts.VideoSource{URL: link, Quality: qs, Type: "mp4", Headers: playbackUA}
		if quality > maxQuality {
			maxQuality = quality
		}
	}
	if maxQuality > 0 {
		max := strconv.Itoa(maxQuality)
		if hls != "" {
			results[max] = contracts.VideoSource{URL: hls, Quality: max, Type: "m3u8", Headers: playbackUA}
		}
		if dash != "" {
			results[max] = contracts.VideoSource{URL: dash, Quality: max, Type: "mpd", Headers: playbackUA}
		}
	}
	return results, nil
}
