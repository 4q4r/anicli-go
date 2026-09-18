package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Anime365Mirrors is the mirror failover list in priority order
// (controller intel 2026-09-18: smotret-anime.app works from RF; the
// official docs also list smotret-anime.online and anime365.ru —
// github.com/thedvxch/anime365wrapper README, DEFAULT_MIRRORS).
var Anime365Mirrors = []string{
	"https://smotret-anime.app",
	"https://smotret-anime.online",
	"https://anime365.ru",
}

// anime365SeriesIDRe extracts the series id from the catalog URL the
// search results carry ("…/catalog/dandadan-35439").
var anime365SeriesIDRe = regexp.MustCompile(`-(\d+)/?$`)

// anime365ContentTypes is the episode-type whitelist (the official
// episodeType enum of the API docs): previews (trailers), openings,
// endings and "other" singles are junk for the dub-watch flow and are
// dropped client-side. isActive is deliberately NOT consulted: the
// live API flags real episodes inactive (Dandadan ep 24, captured
// 2026-09-18).
var anime365ContentTypes = map[string]bool{
	"tv": true, "movie": true, "ova": true, "ona": true, "special": true,
}

// Anime365 is the smotret-anime (anime365) provider. Unlike most of
// the roster it has NO frozen Python original — it is written against
// the live documented JSON API (github.com/thedvxch/anime365wrapper +
// the official OpenAPI spec, probed 2026-09-18), like anidub was
// written against its live site (PR22).
//
// The catalog, episodes and translation metadata are open; the embed
// data (playable links) requires an access token from an account with
// an active subscription, passed as the access_token query parameter.
// The provider therefore joins the unconfigured-provider table like
// kodik (PR24): without a token it is disabled at startup, and a
// tokenless ResolveStream fails loud with a typed error.
type Anime365 struct {
	Base

	// token is the access token from settings (providers.anime365.token
	// or ANICLI_ANIME365_TOKEN); consumed only by the embed resolution.
	token string

	// bases is the mirror failover list: first-found-wins — a mirror
	// that ANSWERS (any HTTP status, any API error body) wins; only
	// transport failures move to the next mirror (the wrapper's
	// ruling: an answering domain is not the problem).
	bases []string
}

// newAnime365 builds the provider against the mirror list (bases[0]
// doubles as the reported BaseURL) with the access token.
func newAnime365(bases []string, token string, http *netclient.Client) *Anime365 {
	return &Anime365{
		Base: Base{
			id:          "anime365",
			name:        "Anime365",
			baseURL:     bases[0],
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		token: token,
		bases: bases,
	}
}

// anime365Envelope is the wire envelope of every endpoint: lists and
// single objects ride "data"; errors ride "error" — possibly with
// HTTP 200 (documented API quirk: "Ошибки приходят с HTTP 200").
type anime365Envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *anime365Error  `json:"error"`
}

// anime365Error is the API error object ({"code","message","fields"?}).
type anime365Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// err maps the API error onto the sentinel taxonomy: 401/403 are
// credential/subscription problems (config-flavored ErrInvalidInput,
// kodik 401 parity), 404 is ErrNotFound, anything else stays a plain
// descriptive provider error.
func (e *anime365Error) err() error {
	switch e.Code {
	case 401, 403:
		return fmt.Errorf("%w: anime365 api error %d: %s", contracts.ErrInvalidInput, e.Code, e.Message)
	case 404:
		return fmt.Errorf("%w: anime365 api error %d: %s", contracts.ErrNotFound, e.Code, e.Message)
	default:
		return fmt.Errorf("anime365 api error %d: %s", e.Code, e.Message)
	}
}

// get GETs path from the first mirror that can serve it. A mirror that
// ANSWERS wins — any HTTP status, including the netclient-mapped 4xx/5xx
// sentinels (the wrapper's ruling: an answering domain is not the
// problem, fallback never triggers) — only transport failures
// (connection refused, DNS, timeouts) move to the next mirror.
func (p *Anime365) get(ctx context.Context, op, path string) (*netclient.Response, error) {
	var lastErr error
	for _, base := range p.bases {
		resp, err := p.http.Do(ctx, netclient.Request{
			Method: "GET",
			URL:    base + path,
			Op:     op,
		})
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// netclient maps non-2xx answers onto typed errors after
		// reading the body (403 → ErrProvider403, 404 → ErrNotFound,
		// the rest → StatusError): an answer, not a transport failure.
		var statusErr *netclient.StatusError
		var providerErr *contracts.ProviderError
		if errors.As(err, &statusErr) || (errors.As(err, &providerErr) && providerErr.StatusCode > 0) {
			return nil, err
		}
	}
	return nil, lastErr
}

// decode unpacks the envelope and maps its error object (the API
// answers errors with HTTP 200 — the body wins over the status) or a
// bare non-2xx status onto the sentinel taxonomy.
func (p *Anime365) decode(op string, status int, body []byte) (json.RawMessage, error) {
	var env anime365Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, status,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, truncErr(body)))
	}
	if env.Error != nil {
		return nil, contracts.WrapProvider(p.ID(), op, status, env.Error.err())
	}
	if status < 200 || status > 299 {
		return nil, contracts.WrapProvider(p.ID(), op, status,
			fmt.Errorf("unexpected status %d", status))
	}
	return env.Data, nil
}

// Search queries the open catalog (GET /api/series?query=…&limit=20)
// and maps each series hit: the display title, the catalog URL (its
// trailing -<id> is the series handle GetEpisodes parses), the poster
// and — surfaced per the controller ruling — the tracker mappings the
// API carries: the Shikimori link from links[] and the MyAnimeList id.
func (p *Anime365) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	resp, err := p.get(ctx, contracts.OpSearch,
		"/api/series?limit=20&query="+pyQuote(query))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0, err)
	}

	data, err := p.decode(contracts.OpSearch, resp.StatusCode, resp.Body)
	if err != nil {
		return nil, err
	}
	var series []anime365Series
	if err := json.Unmarshal(data, &series); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, truncErr(resp.Body)))
	}

	results := make([]contracts.SearchResult, 0, len(series))
	for _, s := range series {
		meta := map[string]any{}
		if s.MyAnimeListID != 0 {
			meta["mal_id"] = s.MyAnimeListID
		}
		if shiki := s.shikimoriLink(); shiki != "" {
			meta["shikimori"] = shiki
		}
		results = append(results, contracts.SearchResult{
			Title:    s.Title,
			URL:      s.URL,
			SourceID: p.ID(),
			Poster:   s.PosterURL,
			Meta:     meta,
		})
	}
	return results, nil
}

// GetEpisodes lists the series' episodes: the series id comes from the
// search URL's trailing -<id>, the open /api/episodes endpoint is
// asked for a large page (live-verified cap: the API honors
// limit=2500), and only content episode types survive — previews,
// openings, endings and "other" singles are junk for the dub-watch
// flow. Dubs are NOT fetched here: the translation list hydrates per
// episode (DubsHydrator, the anilib/animego model).
func (p *Anime365) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	m := anime365SeriesIDRe.FindStringSubmatch(animeURL)
	if m == nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: cannot parse the anime365 series id from %q",
				contracts.ErrInvalidInput, animeURL))
	}

	resp, err := p.get(ctx, contracts.OpGetEpisodes,
		"/api/episodes?seriesId="+m[1]+"&limit=2500")
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0, err)
	}
	data, err := p.decode(contracts.OpGetEpisodes, resp.StatusCode, resp.Body)
	if err != nil {
		return nil, err
	}

	var raw []anime365Episode
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, truncErr(resp.Body)))
	}

	episodes := make([]contracts.Episode, 0, len(raw))
	for _, e := range raw {
		if !anime365ContentTypes[e.EpisodeType] {
			continue
		}
		id := strconv.FormatInt(e.ID, 10)
		episodes = append(episodes, contracts.Episode{
			Num:       strconv.FormatInt(e.EpisodeInt, 10),
			Title:     e.EpisodeTitle,
			RawID:     id,
			RawEmbeds: map[string][]string{},
		})
	}
	return episodes, nil
}

// FetchDubs hydrates one episode's dub list from the open translation
// metadata (GET /api/translations?episodeId=…): voice-kind
// translations become dubs named after their author teams
// (first-found-wins on name collisions — the API orders by priority),
// raw/sub entries are not audio options and never surface, inactive
// translations are dead dubs that stay hidden. The embed entry is the
// API PATH (/api/translations/embed/<id>) — ResolveStream resolves it
// through the mirror list at play time.
func (p *Anime365) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	resp, err := p.get(ctx, contracts.OpGetEpisodes,
		"/api/translations?episodeId="+episode.RawID+"&limit=500")
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0, err)
	}
	data, err := p.decode(contracts.OpGetEpisodes, resp.StatusCode, resp.Body)
	if err != nil {
		return nil, err
	}

	var translations []anime365Translation
	if err := json.Unmarshal(data, &translations); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, truncErr(resp.Body)))
	}

	for _, tr := range translations {
		if tr.TypeKind != "voice" || tr.IsActive != 1 {
			continue
		}
		name := strings.Join(tr.AuthorsList, ", ")
		if name == "" {
			name = "Озвучка"
		}
		if _, taken := episode.RawEmbeds[name]; taken {
			continue // first-found-wins (the API orders by priority)
		}
		episode.RawEmbeds[name] = []string{
			"/api/translations/embed/" + strconv.FormatInt(tr.ID, 10)}
	}
	return episode, nil
}

// ResolveStream resolves the chosen dub's playable links: the embed
// path recorded by FetchDubs is fetched with the access_token query
// parameter (OpenAPI accessTokenQuery; requires an account with an
// active subscription) and the stream[] heights become the quality
// keys, with download[] entries filling qualities the stream list
// lacks.
//
// An empty configured token fails loud BEFORE any request — kodik
// parity (PR24). The provider is registration-gated anyway
// (unconfigured-provider table), so this is the hand-built-instance
// guard.
func (p *Anime365) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	paths := episode.RawEmbeds[dubID]
	if len(paths) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: unknown dub %q", contracts.ErrNotFound, dubID))
	}
	if p.token == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: anime365 token is empty: set providers.anime365.token in settings.toml or ANICLI_ANIME365_TOKEN",
				contracts.ErrInvalidInput))
	}

	resp, err := p.get(ctx, contracts.OpResolveStream,
		paths[0]+"?access_token="+pyQuote(p.token))
	if err != nil {
		// The embed resource is the subscription gate: the API answers
		// 403 for accounts without an active subscription (OpenAPI:
		// "Не выполнен вход или нет активной подписки"). Config-flavored
		// typed error, kodik 401 parity. A plain 404 stays ErrNotFound
		// (the API's own tokenless-embed answer, captured 2026-09-18).
		if errors.Is(err, contracts.ErrProvider403) {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, http.StatusForbidden,
				fmt.Errorf("%w: the anime365 account lacks an active subscription or the token is wrong: check providers.anime365.token",
					contracts.ErrInvalidInput))
		}
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	data, err := p.decode(contracts.OpResolveStream, resp.StatusCode, resp.Body)
	if err != nil {
		return stream, err
	}

	var embed anime365Embed
	if err := json.Unmarshal(data, &embed); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, truncErr(resp.Body)))
	}

	for _, s := range embed.Stream {
		if len(s.URLs) == 0 {
			continue
		}
		quality := strconv.Itoa(s.Height)
		stream.Links[quality] = contracts.VideoSource{
			URL:     s.URLs[0],
			Quality: quality,
			Type:    anime365StreamType(s.URLs[0]),
		}
	}
	for _, d := range embed.Download {
		quality := strconv.Itoa(d.Height)
		if _, ok := stream.Links[quality]; ok {
			continue // stream[] wins; download only fills the gaps
		}
		stream.Links[quality] = contracts.VideoSource{
			URL:     d.URL,
			Quality: quality,
			Type:    anime365StreamType(d.URL),
		}
	}
	return stream, nil
}

// anime365StreamType labels a link by its path extension (.m3u8 →
// "m3u8", .mp4 → "mp4"); empty when the extension is unknown.
func anime365StreamType(u string) string {
	switch {
	case strings.Contains(u, ".m3u8"):
		return "m3u8"
	case strings.Contains(u, ".mp4"):
		return "mp4"
	default:
		return ""
	}
}

// anime365Series is the catalog entry the search endpoint returns
// (live capture 2026-09-18; the API carries far more fields — only
// the consumed ones are typed).
type anime365Series struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	PosterURL     string `json:"posterUrl"`
	MyAnimeListID int64  `json:"myAnimeListId"`
	Links         []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"links"`
}

// shikimoriLink picks the Шикимори entry out of links[] (present on
// live series objects: {"title":"Шикимори","url":"https://shikimori.io/animes/57334"}).
func (s *anime365Series) shikimoriLink() string {
	for _, l := range s.Links {
		if l.Title == "Шикимори" {
			return l.URL
		}
	}
	return ""
}

// anime365Episode is the /api/episodes entry.
type anime365Episode struct {
	ID           int64  `json:"id"`
	EpisodeInt   int64  `json:"episodeInt"`
	EpisodeTitle string `json:"episodeTitle"`
	EpisodeType  string `json:"episodeType"`
	IsActive     int    `json:"isActive"`
}

// anime365Translation is the /api/translations entry: typeKind splits
// voice ("voice") from subtitles ("sub") and raw uploads ("raw").
type anime365Translation struct {
	ID          int64    `json:"id"`
	AuthorsList []string `json:"authorsList"`
	Type        string   `json:"type"`
	TypeKind    string   `json:"typeKind"`
	IsActive    int      `json:"isActive"`
}

// anime365Embed is the /api/translations/embed/{id} payload (the
// wrapper's EmbedTranslation type; requires access_token + active
// subscription).
type anime365Embed struct {
	Download []struct {
		Height int    `json:"height"`
		URL    string `json:"url"`
	} `json:"download"`
	Stream []struct {
		Height int      `json:"height"`
		URLs   []string `json:"urls"`
	} `json:"stream"`
}

// truncErr renders a body prefix for decode-failure messages.
func truncErr(body []byte) string {
	const max = 120
	if len(body) > max {
		body = body[:max]
	}
	s := strings.ToValidUTF8(string(body), "�")
	return strconv.Quote(s)
}
