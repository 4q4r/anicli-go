package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniFilmBase is the site root [LIVE-VERIFIED 2026-09-23: plain
// browser-UA curl answers HTTP 200 on every probed path — the site
// fronts no anti-bot wall].
const AniFilmBase = "https://anifilm.pro"

// AniFilm is the anifilm.pro provider (Russian dubs, RU streaming +
// download/torrent catalog). Not a port and not DLE (the sameband /
// animedia DLE search-form recipe does NOT apply): the site runs a
// custom "AniFilm System" engine (WebyTech; Yii CSRF on the account
// forms, Vue components for the player). Written from the live site,
// characterized 2026-09-23:
//
//   - Search: GET /releases?title=<query> — the site's own header
//     form is method=get (the engine also exposes a fast-search
//     /releases/api:search fragment, but the full page carries the
//     complete result set). Results render server-side as
//     .releases__item cards; a miss is HTTP 200 with zero cards.
//   - Episodes: the release page hydrates <player-component> from
//     its :releases_id and :services_props attributes; the episode
//     playlist is GET /releases/api:online:<releases_id>:<service> —
//     a JSON array of {id, rid, episode, title, iframe, from} rows.
//     Services are tried kodik-first (the catalog is kodik-dominant
//     and kodik embeds resolve through the shared extractor); the
//     trailer "service" is a promo source and never an episode
//     surface. Rows decode to episodes whose raw embed is the
//     release's /releases/api:video:<row id> page — the site's own
//     iframe source. The playlist's raw iframe field points at a
//     kodik mirror host (anivod.com) that the extractor gate does
//     not cover and that fails TLS on some networks; the api:video
//     page wraps the canonical kodikplayer.com URL instead.
//   - Streams: the api:video page carries one .iframe-player iframe
//     pointing at kodikplayer.com; it resolves through the shared
//     extractor factory (kodik: the 2026-09 vInfo player shape with
//     the /ftor default API path — live-probed 2026-09-23,
//     360/480/720 mp4 for a Devil May Cry episode).
//
// Known walls, typed per the no-silent-failure policy:
//
//   - The search index lists releases whose pages answer 404
//     (deletion lag; observed live on 2026-09-23 for three RSS
//     entries) — GetEpisodes surfaces the netclient's typed
//     ErrNotFound.
//   - A release page without a player-component (or without a
//     parsable releases_id), a release whose active services are all
//     unservable, and an api:video page without the player iframe
//     all surface contracts.ErrExtractFailed naming the serving
//     shape.
//
// Torrents: the release page lists per-release .torrent downloads
// (GET /releases/download-torrent/<tid> answers a real torrent file
// anonymously, live-verified 2026-09-23) — a TorrentBase extension
// candidate, deliberately out of scope for the stream provider.
type AniFilm struct {
	Base
}

// afServiceDub is the dub name for titles whose release page credits
// zero or several voices while the playlist carries exactly one
// unnamed translation per episode (the amdServiceDub precedent).
const afServiceDub = "AniFilm"

// afSmokeQuery is the declared smoke probe (PR51 mechanism): Black
// Lagoon — the shared RU probe — is not on the catalog [LIVE-VERIFIED
// 2026-09-23: 0 cards]. «дьявол» surfaces four releases including
// Devil May Cry, live-verified through the full chain.
const afSmokeQuery = "дьявол"

// newAniFilm builds the provider against baseURL. anifilm.pro answers
// plain client requests (verified via curl and the netclient
// fingerprint through the live probe), so the shared netclient is
// kept: per-provider cookie jar, status mapping and the CF ladder
// wiring all apply as for every standard provider.
func newAniFilm(baseURL string, http *netclient.Client) *AniFilm {
	return &AniFilm{Base: Base{
		id:          "anifilm",
		name:        "AniFilm",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// SmokeQuery reports the provider-specific live smoke probe.
func (p *AniFilm) SmokeQuery() string { return afSmokeQuery }

// afAbsolutize turns the site's relative URLs into absolute ones
// against the provider base.
func (p *AniFilm) afAbsolutize(src string) string {
	switch {
	case src == "":
		return ""
	case strings.HasPrefix(src, "http"):
		return src
	case strings.HasPrefix(src, "//"):
		return "https:" + src
	default:
		return p.baseURL + src
	}
}

// afService is one entry of the player-component's :services_props
// JSON: which embedding service the release serves and whether it is
// the active default.
type afService struct {
	From   string `json:"from"`
	Active bool   `json:"active"`
}

// afPlaylistRow is one api:online playlist entry [LIVE-VERIFIED
// 2026-09-23: all fields arrive as JSON strings].
type afPlaylistRow struct {
	ID       string `json:"id"`
	RID      string `json:"rid"`
	Episode  string `json:"episode"`
	Title    string `json:"title"`
	Iframe   string `json:"iframe"`
	ImageURL string `json:"imageUrl"`
	From     string `json:"from"`
}

// Search GETs the site's release filter and scrapes the result cards
// [LIVE-VERIFIED 2026-09-23: GET /releases?title=дьявол → HTTP 200
// with 4 rendered cards; junk query → 200 with zero cards; the query
// rides the form's own `title` field]. The url.Values encoding
// (percent-encoded UTF-8) is what the site's own GET form submits.
func (p *AniFilm) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{"title": {query}}
	resp, err := p.http.Get(ctx, p.baseURL+"/releases?"+form.Encode(), nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	results := make([]contracts.SearchResult, 0, 8)
	doc.Find(".releases__item").Each(func(_ int, card *goquery.Selection) {
		titleNode := card.Find("a.releases__title-russian[href]").First()
		if titleNode.Length() == 0 {
			return
		}
		title := strings.TrimSpace(titleNode.Text())
		href, _ := titleNode.Attr("href")
		if title == "" || href == "" {
			return
		}

		poster := ""
		if img := card.Find("img.releases__image[src]").First(); img.Length() > 0 {
			poster = p.afAbsolutize(mustAttr(img, "src"))
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      p.afAbsolutize(href),
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes fetches the release page, reads the player-component's
// releases_id, walks the active services kodik-first and expands the
// winning playlist into (episode, dub) rows [LIVE-VERIFIED 2026-09-23
// on Devil May Cry: 12 rows × one credited voice «Боллектив Media»;
// Дорохедоро: Спешл (5 credited voices, single-translation embeds)
// pins the afServiceDub fallback rationale].
func (p *AniFilm) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err // the netclient maps 404 → typed ErrNotFound
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse release page: %w", err))
	}

	releasesID := afReleasesID(string(resp.Body))
	if releasesID == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: release page carries no player-component releases_id; "+
				"no anonymous episode surface", contracts.ErrExtractFailed))
	}

	// The services props parse off the RAW page: the Vue render emits
	// the attribute unquoted, and a goquery re-serialization would
	// escape the JSON's double quotes into &quot; entities.
	services, err := afServices(string(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: release page player props unparsable: %w", contracts.ErrExtractFailed, err))
	}

	// Dub name: the release page's credited voices. Exactly one credit
	// names the single translation inside the embeds; zero or several
	// credits stay ambiguous — the service dub carries the rows.
	dub := afDubName(doc)

	rows, served, servErr := p.afPlaylist(ctx, releasesID, services)
	if servErr != nil {
		return nil, servErr
	}
	if len(rows) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: release %s serves no episode rows from any active service (tried %s)",
				contracts.ErrExtractFailed, releasesID, strings.Join(served, ", ")))
	}

	episodes := make([]contracts.Episode, 0, len(rows))
	for _, row := range rows {
		num := row.Episode
		if num == "" {
			num = row.ID // the row id still orders deterministically
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: row.Title,
			RawID: row.ID,
			RawEmbeds: map[string][]string{
				dub: {p.afAbsolutize("/releases/api:video:" + row.ID)},
			},
		})
	}

	nums := make([]string, len(episodes))
	for i, ep := range episodes {
		nums[i] = ep.Num
	}
	// amdAllDigits/amdSortNumeric are the package's tested numeric
	// episode-order helpers (animedia) — same shape, reused.
	if amdAllDigits(nums) {
		amdSortNumeric(nums)
		afReorderEpisodes(episodes, nums)
	}
	return episodes, nil
} // afPlaylist tries the active services in kodik-first order and
// returns the first playlist that decodes to ≥1 row, the services it
// polled, and the typed wall when nothing serves episodes. The
// trailer service is never polled (a promo source, not episodes).
func (p *AniFilm) afPlaylist(ctx context.Context, releasesID string, services []afService) ([]afPlaylistRow, []string, error) {
	var tried []string
	for _, name := range servableServices(services) {
		tried = append(tried, name)

		resp, err := p.http.Get(ctx, p.baseURL+"/releases/api:online:"+releasesID+":"+name, nil)
		if err != nil {
			continue // a dead service is not yet a wall — try the next
		}

		var rows []afPlaylistRow
		if json.Unmarshal(resp.Body, &rows) != nil || len(rows) == 0 {
			continue // 200-with-empty (or junk) serves no episodes either
		}
		return rows, tried, nil
	}

	return nil, tried, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
		fmt.Errorf("%w: release %s serves episodes through an unsupported service set "+
			"(active: %s; polled: %s)", contracts.ErrExtractFailed, releasesID,
			afServiceNames(services), strings.Join(tried, ", ")))
}

// ResolveStream fetches the episode's api:video page, lifts the
// player iframe and resolves it through the shared extractor factory:
// anifilm's embeds are kodik URLs, so the kodik extractor yields
// quality-keyed mp4 links. A dub the episode does not carry is a
// caller bug (typed ErrInvalidInput); an iframe-less video page or a
// resolve that yields nothing is the typed extract wall.
func (p *AniFilm) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	links, ok := episode.RawEmbeds[dubID]
	if !ok || len(links) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrInvalidInput, episode.Num, dubID))
	}

	resp, err := p.http.Get(ctx, links[0], nil)
	if err != nil {
		return stream, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("parse video page: %w", err))
	}

	frame := doc.Find("iframe.iframe-player[src]").First()
	if frame.Length() == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: video page for episode %s carries no iframe-player embed",
				contracts.ErrExtractFailed, episode.Num))
	}

	sources, err := resolveEmbeds(ctx, p.http, []string{mustAttr(frame, "src")})
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

// afReleasesID scrapes the player-component's :releases_id attribute
// off the raw page (the attribute is rendered unquoted:
// `:releases_id=1200`).
func afReleasesID(page string) string {
	idx := strings.Index(page, ":releases_id=")
	if idx < 0 {
		return ""
	}
	rest := page[idx+len(":releases_id="):]
	end := 0
	for end < len(rest) {
		c := rest[end]
		if c < '0' || c > '9' {
			break
		}
		end++
	}
	return rest[:end]
}

// afServices reads the player-component's :services_props attribute
// off the raw page: the Vue render outputs it unquoted, so the JSON
// object is located textually and parsed with a balanced-brace scan
// (the value nests one level).
func afServices(page string) ([]afService, error) {
	idx := strings.Index(page, ":services_props=")
	if idx < 0 {
		return nil, fmt.Errorf("no services_props attribute")
	}
	rest := page[idx+len(":services_props="):]
	start := strings.IndexByte(rest, '{')
	if start < 0 {
		return nil, fmt.Errorf("services_props carries no JSON object")
	}
	raw, err := afBalancedJSON(rest[start:])
	if err != nil {
		return nil, err
	}
	return parseServicesProps(raw)
}

// afBalancedJSON returns the first balanced {...} block of s.
func afBalancedJSON(s string) (string, error) {
	depth := 0
	inString := false
	escaped := false
	for i := range len(s) {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[:i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON object")
}

// parseServicesProps decodes the services_props JSON preserving the
// document order of the service keys (Go maps would scramble the
// kodik-first preference).
func parseServicesProps(raw string) ([]afService, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("services_props: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("services_props is not a JSON object")
	}

	var services []afService
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("services_props key: %w", err)
		}
		key, _ := keyTok.(string)
		var service afService
		if err := dec.Decode(&service); err != nil {
			return nil, fmt.Errorf("services_props[%s]: %w", key, err)
		}
		if service.From == "" {
			service.From = key // the prop value carries the same name
		}
		services = append(services, service)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("services_props is empty")
	}
	return services, nil
}

// servableServices orders the services for the playlist walk: ACTIVE
// services only (the site's own default surface), kodik first when
// present (the extractor-covered default), every other service in
// document order. The trailer service is never servable.
func servableServices(services []afService) []string {
	var active []afService
	for _, s := range services {
		if s.From == "trailer" || !s.Active {
			continue
		}
		active = append(active, s)
	}
	out := make([]string, 0, len(active))
	for _, s := range active {
		if s.From == "kodik" {
			out = append(out, s.From)
		}
	}
	for _, s := range active {
		if s.From != "kodik" {
			out = append(out, s.From)
		}
	}
	return out
}

// afServiceNames renders the service list for wall messages.
func afServiceNames(services []afService) string {
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.From)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// afDubName derives the dub label from the release page's credited
// voices: exactly one credited voice names it, anything else falls
// back to the service dub. The voice block also carries the voice-
// TYPE link («дублированное» on /releases/voice-type/…) — only
// anchors into /releases/voice/ credit a voice.
func afDubName(doc *goquery.Document) string {
	var voices []string
	seen := map[string]bool{}
	doc.Find(`.release__work-item--voice a[href*="/releases/voice/"]`).Each(func(_ int, sel *goquery.Selection) {
		name := strings.TrimSpace(sel.Text())
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		voices = append(voices, name)
	})
	if len(voices) == 1 {
		return voices[0]
	}
	return afServiceDub
}

// afReorderEpisodes reorders the episode slice to match the sorted
// nums order.
func afReorderEpisodes(episodes []contracts.Episode, nums []string) {
	pos := make(map[string]int, len(nums))
	for i, num := range nums {
		pos[num] = i
	}
	ordered := make([]contracts.Episode, len(episodes))
	for _, ep := range episodes {
		ordered[pos[ep.Num]] = ep
	}
	copy(episodes, ordered)
}

// mustAttr reads an attribute known to exist (the selection was
// matched on its presence).
func mustAttr(sel *goquery.Selection, name string) string {
	v, _ := sel.Attr(name)
	return v
}
