package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// KickassAnimeBase is the site root [LIVE-VERIFIED 2026-09-18]. The
// kaa.lt JSON API (the same surface the Anivexa-API kickassanime
// provider rides) answered every probe directly — no challenge page,
// no proxy requirement observed from this network.
const KickassAnimeBase = "https://kaa.lt"

// kickassAnimeHLSBase builds the playable manifests: every usable
// server src carries the media id in its ?id= query parameter, and the
// id resolves onto the krussdomi HLS edge as
// {base}/{id}/master.m3u8 [LIVE-VERIFIED 2026-09-18: the master
// manifest answers 200 with three video variants and switchable
// Japanese/English/German/Spanish/French audio renditions].
const kickassAnimeHLSBase = "https://hls.krussdomi.com/manifest"

// kickassAnimeReferer is the playback Referer the krussdomi edge
// expects (the Anivexa recipe pins it on every stream).
const kickassAnimeReferer = "https://krussdomi.com/"

// kaaMovieWatchURIRe tails the movie watch_uri ("…/ep-0-8d7564"):
// group 1 is the full episode slug used as RawID. The wire number is
// meaningless for movies (live captures show ep-0), so the movie
// renders as episode 1 — the Anivexa recipe's ruling.
var kaaMovieWatchURIRe = regexp.MustCompile(`/(?i)(ep-(\d+)-([0-9a-f]+))$`)

// Kickassanime serves the kaa.lt catalog: a fuzzy JSON search, a
// paginated per-show episode API, and per-episode server lists whose
// media ids resolve onto the krussdomi HLS edge. Shapes verified live
// 2026-09-18 against the running site.
type Kickassanime struct {
	Base
	// maxParallel bounds the episode-page fan-out (config
	// network.max_parallel).
	maxParallel int
}

// newKickassanime builds the provider against baseURL. The netclient
// already sends a browser-fingerprint user agent; the API wants the
// JSON accept header on every GET.
func newKickassanime(baseURL string, http *netclient.Client, maxParallel int) *Kickassanime {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	return &Kickassanime{
		Base: Base{
			id:          "kickassanime",
			name:        "KickassAnime",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			headers:     map[string]string{"Accept": "application/json"},
			http:        http,
		},
		maxParallel: maxParallel,
	}
}

// kaaSearchResponse mirrors the fsearch answer [LIVE-VERIFIED
// 2026-09-18]: {"result":[…], "maxPage":N}. Entries loosely matched by
// the fuzzy index omit title_en (the real "one piece" capture carries
// such an entry), so the title falls back to the native field.
type kaaSearchResponse struct {
	Result []struct {
		Slug    string `json:"slug"`
		Title   string `json:"title"`
		TitleEn string `json:"title_en"`
		Type    string `json:"type"`
		Year    int    `json:"year"`
	} `json:"result"`
}

// kaaShowResponse mirrors the show endpoint. type "movie" switches the
// episode builder onto the watch_uri path; locales drive nothing
// server-side (the dub audio rides inside the master manifest), they
// only document what the show offers.
type kaaShowResponse struct {
	Slug     string   `json:"slug"`
	Type     string   `json:"type"`
	Locales  []string `json:"locales"`
	WatchURI string   `json:"watch_uri"`
}

// kaaPageRef is one entry of the episode API's pages descriptor.
type kaaPageRef struct {
	Number int    `json:"number"`
	From   string `json:"from"`
	To     string `json:"to"`
	Eps    []int  `json:"eps"`
}

// kaaEpisodesResponse mirrors the paginated episode API
// [LIVE-VERIFIED 2026-09-18: Dandadan renders one page of 12 entries;
// One Piece renders 12 pages of 100, each follow-up page fetched with
// the page's FIRST episode number as ?ep=].
type kaaEpisodesResponse struct {
	Pages  []kaaPageRef      `json:"pages"`
	Result []kaaEpisodeEntry `json:"result"`
}

// kaaEpisodeEntry is one wire episode. episode_number is decoded
// loosely (number on every live capture, but the Anivexa recipe's
// Number.isFinite guard implies the field has been seen loose) and
// coerced in pageEpisodes.
type kaaEpisodeEntry struct {
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	EpisodeNumber any    `json:"episode_number"`
}

// kaaEpisodeServersResponse mirrors the per-episode server list
// [LIVE-VERIFIED 2026-09-18]: each server's src is a krussdomi player
// URL whose ?id= parameter names the media on the HLS edge.
type kaaEpisodeServersResponse struct {
	Servers []struct {
		Name      string `json:"name"`
		ShortName string `json:"shortName"`
		Src       string `json:"src"`
	} `json:"servers"`
}

// Search runs the fuzzy search [LIVE-VERIFIED 2026-09-18]: POST
// {base}/api/fsearch with JSON {"page":1,"query":<query>}. Result URLs
// are the bare show slugs — the episode API keys on them directly.
func (p *Kickassanime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	body, err := json.Marshal(struct {
		Page  int    `json:"page"`
		Query string `json:"query"`
	}{Page: 1, Query: query})
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("marshal search body: %w", err))
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "POST",
		URL:    p.baseURL + "/api/fsearch",
		Headers: map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
		},
		Body: strings.NewReader(string(body)),
		Op:   contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var data kaaSearchResponse
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(data.Result))
	for _, item := range data.Result {
		if item.Slug == "" {
			continue
		}
		title := item.TitleEn
		if title == "" {
			title = item.Title
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      item.Slug,
			SourceID: p.ID(),
		})
	}
	return results, nil
}

// GetEpisodes lists a show's episodes. animeURL is the show slug from
// Search. TV shows walk the paginated episode API — the first page is
// fetched with ?ep=1, every follow-up with the page's first episode
// number (the Anivexa recipe's pg.eps[0] hop) — fanned out bounded-
// concurrent and concatenated in wire (ascending) order. Movies skip
// the episode API entirely: the single showing comes from the show's
// watch_uri tail, rendered as episode 1 (the wire number is ep-0 on
// live captures and must not leak). RawID is "{showSlug}/{epSlug}" —
// the servers endpoint needs both halves and Episode carries no show
// field.
func (p *Kickassanime) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	showSlug := strings.Trim(animeURL, "/")
	show, err := p.fetchShow(ctx, showSlug)
	if err != nil {
		return nil, err
	}

	if show.Type == "movie" {
		return p.movieEpisodes(showSlug, show.WatchURI), nil
	}
	return p.tvEpisodes(ctx, showSlug)
}

// fetchShow GETs {base}/api/show/{slug} for the type/episode routing.
func (p *Kickassanime) fetchShow(ctx context.Context, showSlug string) (*kaaShowResponse, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/api/show/" + url.PathEscape(showSlug),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var show kaaShowResponse
	if err := json.Unmarshal(resp.Body, &show); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode show %q: %w", showSlug, err))
	}
	return &show, nil
}

// movieEpisodes renders the movie watch_uri tail as the single
// episode. A watch_uri that lost its ep tail yields nothing (fail
// loud downstream: an empty list is the honest answer).
func (p *Kickassanime) movieEpisodes(showSlug, watchURI string) []contracts.Episode {
	m := kaaMovieWatchURIRe.FindStringSubmatch(watchURI)
	if m == nil {
		return nil
	}
	return []contracts.Episode{{
		Num:       "1",
		Title:     "",
		RawID:     showSlug + "/" + m[1],
		RawEmbeds: map[string][]string{},
	}}
}

// tvEpisodes walks the paginated episode API and concatenates the
// pages in wire order.
func (p *Kickassanime) tvEpisodes(ctx context.Context, showSlug string) ([]contracts.Episode, error) {
	first, err := p.episodePage(ctx, showSlug, 1)
	if err != nil {
		return nil, err
	}

	pages := first.Pages
	rest := make([][]contracts.Episode, len(pages)) // page 0 slot stays nil
	if len(pages) > 1 {
		restErr := netclient.Parallel(ctx, pages[1:], p.maxParallel, func(ctx context.Context, pg kaaPageRef) error {
			if len(pg.Eps) == 0 || pg.Number < 1 || pg.Number > len(pages) {
				return nil
			}
			page, err := p.episodePage(ctx, showSlug, pg.Eps[0])
			if err != nil {
				return err
			}
			eps, err := pageEpisodes(showSlug, page)
			if err != nil {
				return err
			}
			rest[pg.Number-1] = eps
			return nil
		})
		if restErr != nil {
			return nil, restErr
		}
	}

	episodes, err := pageEpisodes(showSlug, first)
	if err != nil {
		return nil, err
	}
	for _, batch := range rest {
		episodes = append(episodes, batch...)
	}
	return episodes, nil
}

// episodePage GETs one page of the episode API (?ep=<start>&lang=ja-JP).
func (p *Kickassanime) episodePage(ctx context.Context, showSlug string, startEp int) (*kaaEpisodesResponse, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL: p.baseURL + "/api/show/" + url.PathEscape(showSlug) +
			"/episodes?ep=" + strconv.Itoa(startEp) + "&lang=ja-JP",
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var data kaaEpisodesResponse
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode episodes of %q: %w", showSlug, err))
	}
	return &data, nil
}

// pageEpisodes converts one wire page into episodes. Entries with a
// non-positive or non-numeric episode_number are skipped (the Anivexa
// recipe filters Number.isFinite(num) && num >= 1).
func pageEpisodes(showSlug string, page *kaaEpisodesResponse) ([]contracts.Episode, error) {
	episodes := make([]contracts.Episode, 0, len(page.Result))
	for _, item := range page.Result {
		num, ok := kaaEpisodeNumber(item.EpisodeNumber)
		if !ok || num < 1 || item.Slug == "" {
			continue
		}
		numStr := strconv.FormatFloat(num, 'f', -1, 64)
		episodes = append(episodes, contracts.Episode{
			Num:       numStr,
			Title:     item.Title,
			RawID:     fmt.Sprintf("%s/ep-%s-%s", showSlug, numStr, item.Slug),
			RawEmbeds: map[string][]string{},
		})
	}
	return episodes, nil
}

// kaaEpisodeNumber coerces the wire episode_number: JSON numbers
// arrive as float64, string values parse if numeric, anything else is
// a skip.
func kaaEpisodeNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// FetchDubs implements contracts.DubsHydrator (PR44: the session
// hydrates eagerly per episode). The episode's RawID is
// "{showSlug}/{epSlug}"; the server list endpoint answers with the
// named mirrors [LIVE-VERIFIED 2026-09-18]. Every id-bearing src
// becomes a constructed master-manifest embed under the server's name;
// dash-typed servers are skipped (their ids answer 502 on the manifest
// path — two independent ids probed) and id-less srcs have nothing to
// build a manifest from.
func (p *Kickassanime) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	showSlug, epSlug, found := strings.Cut(episode.RawID, "/")
	if !found || showSlug == "" || epSlug == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode RawID %q is not {showSlug}/{epSlug}", episode.RawID))
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/api/show/" + url.PathEscape(showSlug) + "/episode/" + url.PathEscape(epSlug),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var data kaaEpisodeServersResponse
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode servers of %q: %w", episode.RawID, err))
	}

	embeds := map[string][]string{}
	for _, server := range data.Servers {
		if strings.Contains(server.Src, "type=dash") {
			continue
		}
		u, err := url.Parse(server.Src)
		if err != nil {
			continue
		}
		id := u.Query().Get("id")
		if id == "" {
			continue
		}
		name := server.Name
		if name == "" {
			name = server.ShortName
		}
		if name == "" {
			name = "Unknown"
		}
		embeds[name] = append(embeds[name],
			fmt.Sprintf("%s/%s/master.m3u8", kickassAnimeHLSBase, id))
	}

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream returns the chosen server's master manifest as the
// single "auto" source: the krussdomi master carries every video
// variant AND the switchable audio renditions (Japanese + English dub
// among them [LIVE-VERIFIED 2026-09-18]), so splitting variants into
// per-quality links would strip the audio tracks. Like the gogoanime
// port, the dub list is fetched lazily when the episode carries none,
// and an unknown dub or an unresolved manifest fails typed.
func (p *Kickassanime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
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

	// One server slot carries exactly one master manifest by
	// construction (FetchDubs appends one embed per server).
	linksOut := map[string]contracts.VideoSource{
		"auto": {
			URL:     links[0],
			Quality: "auto",
			Type:    "m3u8",
			Headers: map[string]string{"Referer": kickassAnimeReferer},
		},
	}
	return contracts.MediaStream{DubName: dubID, Links: linksOut}, nil
}

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// the kaa.lt fuzzy index matches romaji/english titles only — Cyrillic
// queries there are guaranteed-zero.
func (p *Kickassanime) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// SmokeQuery implements contracts.SmokeQueryProvider (PR52): the
// shared probes (черная лагуна / black lagoon) surface kaa.lt entries
// whose episode server lists are currently empty server-side — the
// chain dies at hydration through no provider-code fault
// [LIVE-VERIFIED 2026-09-18]. "dandadan" is the proven broad hit: two
// fresh-season search hits, both with populated per-episode servers.
func (p *Kickassanime) SmokeQuery() string { return "dandadan" }
