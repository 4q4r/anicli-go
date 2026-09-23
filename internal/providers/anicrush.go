package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniCrushBase is the site root [RECONSTRUCTED]. The whole anicrush.to
// family (site + api.anicrush.to) went origin-dead behind Cloudflare
// (HTTP 521 served by the CF edge for every client network — curl
// direct, curl via the 10809 proxy, webfetch infra, and a stealth
// browser all saw the same edge-rendered error) somewhere around
// 2026-08-07 (multi-country uptime history) and stayed dead through
// 2026-09-23. The provider is written against the wire contract
// documented by three independent wrapper implementations of the
// anonymous playback flow (see the provenance block below), NOT against
// a live capture — no live capture was possible.
const AniCrushBase = "https://anicrush.to"

// AniCrushAPIBase is the JSON API host the site's player front-end
// calls [RECONSTRUCTED — see AniCrushBase]. The wrapper headers (the
// x-site tag in particular) ride on every request below.
const AniCrushAPIBase = "https://api.anicrush.to"

// anicrushPosterBase fronts the poster_path values: the reference
// wrappers build poster URLs as {base}/{poster_path} [RECONSTRUCTED].
const anicrushPosterBase = "https://static.gniyonna.com/media/poster"

// Wire contract provenance (all [RECONSTRUCTED] in the sense above):
//
//   - github.com/DrBrainlessLol/anicrush-api (fork of shafat-96's):
//     the endpoints this provider implements — /shared/v2/movie/list
//     (keyword/page/limit), /shared/v2/episode/list (_movieId),
//     /shared/v2/episode/servers (_movieId, ep) and
//     /shared/v2/episode/sources (_movieId, ep, sv, sc sub|dub) —
//     plus the common headers (x-site: anicrush, site Referer/Origin)
//     and the response status/result envelope (index.js, mapper.js);
//   - github.com/shimizudev/anicrush-api: the exact wire shapes as zod
//     schemas — search result {status, result: {movies: [...]}}
//     (search/types.ts), episode list {status, result:
//     record<string, {id, name, name_english, number, is_filler}[]>}
//     — a record of ARRAYS keyed by an undocumented group id
//     (info/types.ts + info/episodes.ts flattening), servers
//     {status, result: {sub|dub: [{server, type, hard_sub,
//     multiple_audio, streamServer: {name, url, type}}]}}
//     (sources/types.ts; server numbers Megacloud=4, Southcloud=1),
//     sources {status, result: {type, link, server}}
//     (sources/sources.ts);
//   - github.com/ghoshRitesh12/gojo (Go): the movie row fields
//     (src/providers/anicrush/home/types.go CommonAnimeResult).
//
// The final embed→HLS hop: the sources endpoint answers an EMBED
// player link (megacloud.tv/embed-2/e-1/<id> per the reference
// sources' documented examples). The wrappers resolve it with the
// consumet rabbit extractor, which needs a WASM blob served from the
// player host plus canvas-pixel key material baked into the extractor
// itself (shimizudev sources/extractors/megacloud) — not portable to a
// faithful Go port without either artifact, and unverifiable while the
// origin is dead. This provider therefore stops at the documented
// embed link and hands it to the shared extractor factory; a factory
// miss fails typed with contracts.ErrExtractFailed (no silent
// swallowing), and a future rabbit/megacloud extractor slots in
// without provider changes. Subtitle tracks the player exposes are a
// property of that unported hop: the provider contracts carry no
// subtitle field, so none surface.

// AniCrush serves the anicrush.to catalog: keyword search, a grouped
// episode list, per-episode sub/dub server rows and per-server embed
// links. Shapes reconstructed per the provenance block above.
type AniCrush struct {
	Base
	// apiBase is the JSON host (AniCrushAPIBase in production); every
	// endpoint below keys on it, the site root only headers playback.
	apiBase string
}

// newAniCrush builds the provider against apiBase (the JSON host). The
// netclient already sends a browser-fingerprint user agent; the API
// wants the site tag and CORS-style headers on every call (the
// wrappers' common header block).
func newAniCrush(apiBase string, http *netclient.Client) *AniCrush {
	return &AniCrush{
		Base: Base{
			id:          "anicrush",
			name:        "AniCrush",
			baseURL:     AniCrushBase,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			headers: map[string]string{
				"Accept":           "application/json, text/plain, */*",
				"Accept-Language":  "en-US,en;q=0.9",
				"x-site":           "anicrush",
				"Referer":          AniCrushBase + "/",
				"Origin":           AniCrushBase,
				"X-Requested-With": "XMLHttpRequest",
			},
			http: http,
		},
		apiBase: apiBase,
	}
}

// acEnvelope is the common API wrapper: {status, message?, result}.
// result rides as raw JSON — every endpoint decodes its own shape.
type acEnvelope struct {
	Status  bool            `json:"status"`
	Message string          `json:"message,omitempty"`
	Result  json.RawMessage `json:"result"`
}

// acMovie is one search row (the fields this provider consumes; the
// reference shapes carry more — schedule, backdrop, producers — which
// are not needed here).
type acMovie struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	NameEnglish  string `json:"name_english"`
	Slug         string `json:"slug"`
	PosterPath   string `json:"poster_path"`
	TotalEpisode int    `json:"total_episodes"`
	AiredFrom    string `json:"aired_from"`
}

// acSearchResult mirrors the movie/list result object.
type acSearchResult struct {
	Movies []acMovie `json:"movies"`
}

// acEpisodeEntry is one wire episode of the episode/list record.
// is_filler is not consumed (the dub pipeline has no filler facet).
type acEpisodeEntry struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	NameEnglish string  `json:"name_english"`
	Number      float64 `json:"number"`
}

// acStreamServer is the player descriptor of one server row.
// url is nullable on the wire (the zod schema says so) — the embed
// link actually used for playback comes from the sources endpoint.
type acStreamServer struct {
	Name string  `json:"name"`
	URL  *string `json:"url"`
	Type string  `json:"type"`
}

// acServer is one row of the episode/servers sub|dub arrays. server is
// the numeric id the sources endpoint keys on (sv=; the reference
// wrappers document Megacloud=4, Southcloud=1).
type acServer struct {
	Server        int            `json:"server"`
	Type          int            `json:"type"`
	HardSub       int            `json:"hard_sub"`
	MultipleAudio int            `json:"multiple_audio"`
	StreamServer  acStreamServer `json:"streamServer"`
}

// acServersResult mirrors the episode/servers result object: sub and
// dub server lists.
type acServersResult struct {
	Sub []acServer `json:"sub"`
	Dub []acServer `json:"dub"`
}

// acSourcesResult mirrors the episode/sources result object: the embed
// player link for one (movie, episode, server, audio) tuple.
type acSourcesResult struct {
	Type   string `json:"type"`
	Link   string `json:"link"`
	Server int    `json:"server"`
}

// acGet decodes one API GET into the envelope, mapping the API-level
// {status: false, message} refusal onto a typed provider error (the
// wrappers throw on it). The raw result is returned for the caller's
// own shape decode.
func (p *AniCrush) acGet(ctx context.Context, op, rawURL string) (json.RawMessage, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     rawURL,
		Headers: p.headers,
		Op:      op,
	})
	if err != nil {
		return nil, err
	}

	var env acEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("decode response: %w", err))
	}
	if !env.Status {
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("api refusal: %s: %w", env.Message, contracts.ErrNotFound))
	}
	return env.Result, nil
}

// Search runs the keyword search: GET
// {api}/shared/v2/movie/list?keyword=<q>&page=1&limit=24 (the
// wrappers' defaults). Result URLs are the bare movie ids — the
// episode endpoints key on them directly. The title prefers the
// English name and falls back to the romaji name; both are latin.
func (p *AniCrush) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	q := url.Values{}
	q.Set("keyword", query)
	q.Set("page", "1")
	q.Set("limit", "24")

	raw, err := p.acGet(ctx, contracts.OpSearch, p.apiBase+"/shared/v2/movie/list?"+q.Encode())
	if err != nil {
		return nil, err
	}

	var data acSearchResult
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("decode search result: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(data.Movies))
	for _, m := range data.Movies {
		if m.ID == "" {
			continue
		}
		title := m.NameEnglish
		if title == "" {
			title = m.Name
		}
		res := contracts.SearchResult{
			Title:    title,
			URL:      m.ID,
			SourceID: p.ID(),
		}
		if m.PosterPath != "" {
			res.Poster = anicrushPosterBase + m.PosterPath
		}
		results = append(results, res)
	}
	return results, nil
}

// GetEpisodes lists a show's episodes. animeURL is the movie id from
// Search. The wire result is a record of ARRAYS keyed by an
// undocumented group id — the wrappers flatten the values — so the
// groups are flattened and sorted by episode number. RawID is the
// movie id (the server endpoints key on movie+number; the number rides
// in Episode.Num) and RawEmbeds stay empty: the sub/dub server lists
// are one request PER EPISODE, hydrated eagerly through the
// DubsHydrator seam like the kickassanime port.
func (p *AniCrush) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	movieID := strings.Trim(animeURL, "/")
	q := url.Values{}
	q.Set("_movieId", movieID)

	raw, err := p.acGet(ctx, contracts.OpGetEpisodes,
		p.apiBase+"/shared/v2/episode/list?"+q.Encode())
	if err != nil {
		return nil, err
	}

	var groups map[string][]acEpisodeEntry
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("decode episode list of %q: %w", movieID, err))
	}

	// Flatten the wire groups; the wire numbers are JSON numbers
	// (possibly fractional), formatted like the kaa port (FormatFloat
	// -1) so "1" stays "1".
	entries := make([]acEpisodeEntry, 0, len(groups)*4)
	for _, group := range groups {
		entries = append(entries, group...)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Number < entries[j].Number })

	episodes := make([]contracts.Episode, 0, len(entries))
	for _, e := range entries {
		title := e.NameEnglish
		if title == "" {
			title = e.Name
		}
		episodes = append(episodes, contracts.Episode{
			Num:       strconv.FormatFloat(e.Number, 'f', -1, 64),
			Title:     title,
			RawID:     movieID,
			RawEmbeds: map[string][]string{},
		})
	}
	return episodes, nil
}

// acDubKey builds the dub name the TUI shows for one server row: the
// audio group plus the stream server's name (a bare "Megacloud" would
// collide across sub and dub).
func acDubKey(audio string, row acServer) string {
	name := row.StreamServer.Name
	if name == "" {
		name = "Server #" + strconv.Itoa(row.Server)
	}
	return audio + " · " + name
}

// sourcesQueryURL builds the episode/sources request URL one server
// row needs. It is stored verbatim as the dub's RawEmbed entry: a real
// fetchable URL whose answer carries the embed link ResolveStream
// needs — the same "constructed URL as embed" trick the kaa port uses
// for its master manifests.
func (p *AniCrush) sourcesQueryURL(movieID string, ep float64, audio string, row acServer) string {
	q := url.Values{}
	q.Set("_movieId", movieID)
	q.Set("ep", strconv.FormatFloat(ep, 'f', -1, 64))
	q.Set("sv", strconv.Itoa(row.Server))
	q.Set("sc", audio)
	return p.apiBase + "/shared/v2/episode/sources?" + q.Encode()
}

// FetchDubs implements contracts.DubsHydrator (the PR44 eager
// hydration): GET {api}/shared/v2/episode/servers?_movieId=X&ep=N and
// expand every sub/dub server row into one RawEmbeds entry named
// "<sub|dub> · <server>" whose payload is the sources-query URL for
// that row (see sourcesQueryURL). The episode's RawID is the movie id
// and Num the episode number (both set by GetEpisodes).
func (p *AniCrush) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	movieID := strings.Trim(episode.RawID, "/")
	if movieID == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode RawID %q is not a movie id", episode.RawID))
	}
	ep, err := strconv.ParseFloat(episode.Num, 64)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode number %q: %w", episode.Num, err))
	}

	q := url.Values{}
	q.Set("_movieId", movieID)
	q.Set("ep", strconv.FormatFloat(ep, 'f', -1, 64))

	raw, err := p.acGet(ctx, contracts.OpGetEpisodes,
		p.apiBase+"/shared/v2/episode/servers?"+q.Encode())
	if err != nil {
		return nil, err
	}

	var data acServersResult
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("decode servers of %s ep %s: %w", movieID, episode.Num, err))
	}

	embeds := map[string][]string{}
	for _, row := range data.Sub {
		embeds[acDubKey("sub", row)] = append(embeds[acDubKey("sub", row)],
			p.sourcesQueryURL(movieID, ep, "sub", row))
	}
	for _, row := range data.Dub {
		embeds[acDubKey("dub", row)] = append(embeds[acDubKey("dub", row)],
			p.sourcesQueryURL(movieID, ep, "dub", row))
	}

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream resolves the chosen dub's embed link: the RawEmbed
// entry is the episode/sources query URL, whose result.link is the
// player embed. A direct media link (.m3u8/.mp4) resolves through the
// shared factory's direct fallback; an embed runs through the
// registered extractors — the megacloud/rabbit player anicrush uses is
// NOT among them (see the provenance block), so a factory miss fails
// typed instead of pretending success. Like the kaa port, the dub list
// is fetched lazily when the episode carries none, and an unknown dub
// or an unresolved embed fails typed.
func (p *AniCrush) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if len(episode.RawEmbeds) == 0 {
		hydrated, err := p.FetchDubs(ctx, &episode)
		if err != nil {
			return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
		}
		episode = *hydrated
	}

	links := episode.RawEmbeds[dubID]
	if len(links) == 0 || links[0] == "" {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("dub %q carries no mirrors to resolve", dubID))
	}

	raw, err := p.acGet(ctx, contracts.OpResolveStream, links[0])
	if err != nil {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
	}

	var data acSourcesResult
	if err := json.Unmarshal(raw, &data); err != nil {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("decode sources of %s ep %s: %w", episode.RawID, episode.Num, err))
	}
	if data.Link == "" {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("sources for dub %q carry no embed link: %w", dubID, contracts.ErrExtractFailed))
	}

	sources, err := resolveEmbeds(ctx, p.http, []string{data.Link})
	if err != nil {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	if len(sources) == 0 {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("no extractor resolves the %s embed %q: %w", dubID, data.Link, contracts.ErrExtractFailed))
	}
	return contracts.MediaStream{DubName: dubID, Links: sources}, nil
}

// NamePreference implements contracts.NamePreferenceProvider: the
// catalog is latin-indexed (romaji/english titles), a Cyrillic query
// is guaranteed-zero.
func (p *AniCrush) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// SmokeQuery implements contracts.SmokeQueryProvider. Unverifiable
// while the origin is dead (see AniCrushBase): "dandadan" follows the
// kaa port's broad-hit precedent over the wrappers' "one piece"
// example, whose 1000+ episode list would dominate the smoke budget.
func (p *AniCrush) SmokeQuery() string { return "dandadan" }
