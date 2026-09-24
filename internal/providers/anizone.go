package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniZoneBase is the site root.
const AniZoneBase = "https://anizone.to"

// anizoneDub is the fixed single dub name of the sub-only catalog:
// every episode serves the Japanese-audio HLS (the master playlist
// carries selectable ja/en audio groups, ja default) with subtitle
// sidecars — no dubs exist on the site (PR59).
const anizoneDub = "Original (AniZone)"

// azMaxSeriesPages bounds the Livewire episode walk (page size 24, the
// largest live catalog — One Piece — needs 50). Hitting the bound is a
// typed error, never a silent truncation.
const azMaxSeriesPages = 200

// AniZone is anizone.to (PR59): a foreign sub-only stream source. This
// provider is NOT a Python port — the frozen anicli-py has no AniZone.
// The extraction recipe comes from the Anivexa aggregator's AniZone
// provider (github.com/walterwhite-69/Anivexa-API providers/anizone.js,
// read 2026-09-18) and was re-verified live the same day:
//
//	search    GET /anime?search={q}      — Livewire page whose inline
//	            script carries items: JSON.parse('…') with slug,
//	            main_title, title_list, cover, type, start_year,
//	            episode_count.
//	series    GET /anime/{slug}          — the same items payload with
//	            EPISODE rows (slug = number, title_list, duration,
//	            air_date, videos_count), the entity-encoded
//	            wire:snapshot attribute (pages.anime-detail), the
//	            nextCursor / hasMore pagination state and the csrf meta.
//	          POST /livewire/update       — Livewire continuation: the
//	            snapshot + loadPage(cursor) call in, the next
//	            items/snapshot/nextCursor/hasMore out (items-loaded
//	            dispatch). Session cookies rotate every response; the
//	            client jar replays them (live-verified across three
//	            pages of One Piece).
//	watch     GET /anime/{slug}/{number} — vidstackPlayer(JSON.parse('…'))
//	            payload with src (HLS master playlist, 360/720/1080
//	            variants over ja/en audio groups), subtitle sidecars and
//	            storyboard/chapter tracks.
//
// Recipe behaviors kept: hash slugs (not name slugs), pickTitle order
// title_list["1"] → "5" → "8" → first value, numeric-slug episodes only
// (specials s1…s4 dropped — the watch URL and numbering assume numbers),
// videos_count > 0 as the has-sub gate, single-page search. Recipe
// failure semantics upgraded to the house rule: a missing payload or a
// broken continuation is a typed provider error, not a silent null.
type AniZone struct {
	Base
}

// newAniZone builds the provider against baseURL.
func newAniZone(baseURL string, http *netclient.Client) *AniZone {
	return &AniZone{Base: Base{
		id:          "anizone",
		name:        "AniZone",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ja",
		// The recipe's page headers: document accept over the site
		// root referer; the netclient carries the browser UA.
		headers: map[string]string{
			"Referer":         baseURL + "/",
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Accept-Language": "en-US,en;q=0.9",
		},
		http: http,
	}}
}

// NamePreference reports the latin-only search index (PR42): anizone.to
// indexes romaji/english titles, a Cyrillic query is guaranteed-zero.
func (p *AniZone) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// azSearchItem mirrors the fields the search payload carries (verbatim
// capture 2026-09-18): slug, cover, main_title, title_list, type,
// start_year. episode_count/is_ongoing/tags ride along in the payload
// and are not consumed.
type azSearchItem struct {
	Slug      string            `json:"slug"`
	Cover     string            `json:"cover"`
	MainTitle string            `json:"main_title"`
	TitleList map[string]string `json:"title_list"`
	Type      string            `json:"type"`
	StartYear int               `json:"start_year"`
}

// azEpisodeItem mirrors the series payload's episode rows: slug IS the
// episode number ("1"…, or "s1" for specials), videos_count gates the
// has-sub property.
type azEpisodeItem struct {
	Slug        string            `json:"slug"`
	URL         string            `json:"url"`
	TitleList   map[string]string `json:"title_list"`
	VideosCount int               `json:"videos_count"`
}

// azLivewireState is one page of the series walk: the raw episode rows
// plus everything the next continuation call needs.
type azLivewireState struct {
	items    []azEpisodeItem
	snapshot string
	cursor   string
	hasMore  bool
	csrf     string
}

// azLivewireRequest is the /livewire/update body (the recipe's exact
// wire format: one component, its snapshot, the loadPage(cursor) call).
type azLivewireRequest struct {
	Components []azLivewireComponent `json:"components"`
}

type azLivewireComponent struct {
	Snapshot string           `json:"snapshot"`
	Updates  map[string]any   `json:"updates"`
	Calls    []azLivewireCall `json:"calls"`
}

type azLivewireCall struct {
	Path   string   `json:"path"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

// azLivewireResponse is the continuation payload: the NEXT component
// snapshot and the items-loaded dispatch carrying the next page.
type azLivewireResponse struct {
	Components []struct {
		Snapshot string `json:"snapshot"`
		Effects  struct {
			Dispatches []struct {
				Name   string `json:"name"`
				Params struct {
					Items      []azEpisodeItem `json:"items"`
					NextCursor *string         `json:"nextCursor"`
					HasMore    bool            `json:"hasMore"`
				} `json:"params"`
			} `json:"dispatches"`
		} `json:"effects"`
	} `json:"components"`
}

// azPlayer mirrors the consumed vidstackPlayer payload fields: the HLS
// master playlist src. The payload also carries subtitle sidecars and
// storyboard/chapter tracks — they have no slot in the MediaStream
// contract and are deliberately not surfaced.
type azPlayer struct {
	Src string `json:"src"`
}

// Inline-payload extraction patterns (see azJSONArg for the shape).
var (
	azItemsRe  = azJSONArg("items")
	azPlayerRe = regexp.MustCompile(`(?i)vidstackPlayer\s*\(\s*JSON\.parse\('((?:[^'\\]|\\.)*)'\)\s*\)`)
	// wire:snapshot="…" — HTML-entity-encoded JSON; the series page
	// carries several, the detail component is the one naming
	// pages.anime-detail (recipe snapshot()).
	azSnapshotRe = regexp.MustCompile(`wire:snapshot="([^"]*)"`)
	azCursorRe   = regexp.MustCompile(`nextCursor:\s*'([^']+)'`)
	// The recipe only probes for hasMore: true (absence = false).
	azHasMoreRe = regexp.MustCompile(`(?i)hasMore:\s*true`)
	azCSRFRe    = regexp.MustCompile(`csrf-token"\s+content="([^"]+)"`)
	// normalizeUrl: one-or-more backslashes followed by a slash
	// collapse onto the slash (the recipe's /\\+\//g guard for the
	// double-escaped CDN URLs).
	azEscapedSlashRe = regexp.MustCompile(`\\+/`)
	// The double-escaped "\uXXXX" (JS-escaped backslash + JSON escape)
	// must survive the decode pass as the literal JSON escape.
	azDoubleUnicodeRe = regexp.MustCompile(`\\\\u([0-9a-fA-F]{4})`)
	// The recipe's search-slug shape guard.
	azSlugRe = regexp.MustCompile(`(?i)^[a-z0-9-]+$`)
	// azSearchInputRe witnesses the rendered Anime Index page: the
	// Livewire search input's binding (`wire:model.live.debounce.500=
	// "search"` verbatim in the 2026-09-20 captures, with and without
	// results). Its presence with NO items payload is the site's legit
	// no-results answer, not a shape drift.
	azSearchInputRe = regexp.MustCompile(`wire:model[\w.-]*="search"`)
)

// azJSONArg builds the extraction pattern for `name: JSON.parse('…')`
// inline payloads (recipe jsonArgument): the argument is a single-quoted
// JS string literal — any char that is not a quote or backslash, or an
// escaped whatever.
func azJSONArg(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + regexp.QuoteMeta(name) + `\s*:\s*JSON\.parse\('((?:[^'\\]|\\.)*)'\)`)
}

// azDecodeJSONArgument ports decodeJsonArgument (providers/anizone.js
// 24-35): the payload rides inside a JS string literal, so JS escapes
// must come off before JSON parsing. The server double-escapes: "\u0022"
// is a JS escape for a quote (structure quotes in the captured payload),
// while "\\u041F" is a JS-escaped backslash followed by u041F — a
// literal JSON "\u041F" escape JSON.parse must see verbatim. Pass one
// protects the double-escaped form, pass two decodes the remaining JS
// unicode escapes (astral chars arrive as surrogate PAIRS — combined
// like String.fromCharCode), pass three restores the protected escapes.
// The JS original returns null on any failure; the port fails loud.
func azDecodeJSONArgument(raw string) ([]byte, error) {
	const marker = "\x01U\x01"
	protected := azDoubleUnicodeRe.ReplaceAllString(raw, marker+"${1}")

	var b strings.Builder
	b.Grow(len(protected))
	for i := 0; i < len(protected); {
		c := protected[i]
		if c == '\\' && i+6 <= len(protected) &&
			protected[i+1] == 'u' && azHex4(protected[i+2:i+6]) {
			// bitSize 16: exactly four hex digits, so cp <= 0xFFFF.
			cp, err := strconv.ParseUint(protected[i+2:i+6], 16, 16)
			if err != nil {
				b.WriteByte(c)
				i++
				continue
			}
			r := rune(cp) //nolint:gosec // cp <= 0xFFFF (bitSize 16) fits rune
			if utf16.IsSurrogate(r) && i+12 <= len(protected) &&
				protected[i+6] == '\\' && protected[i+7] == 'u' && azHex4(protected[i+8:i+12]) {
				low, err := strconv.ParseUint(protected[i+8:i+12], 16, 16)
				if err == nil {
					if combined := utf16.DecodeRune(r, rune(low)); combined != unicode.ReplacementChar { //nolint:gosec // cp, low <= 0xFFFF fit rune
						b.WriteRune(combined)
						i += 12
						continue
					}
				}
			}
			if utf16.IsSurrogate(r) {
				// An unpaired surrogate has no Go representation; the
				// replacement char keeps the rest of the payload.
				b.WriteRune(unicode.ReplacementChar)
			} else {
				b.WriteRune(r)
			}
			i += 6
			continue
		}
		b.WriteByte(c)
		i++
	}
	out := strings.ReplaceAll(b.String(), marker, `\u`)
	if !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("decoded payload is not valid JSON")
	}
	return []byte(out), nil
}

// azHex4 reports whether s is exactly four hex digits.
func azHex4(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) == 4
}

// azPickTitle ports pickTitle (providers/anizone.js:92-94): the "1"
// (romanized) title, then "5", then "8", then the first value. The JS
// first-value tail follows insertion order; Go maps have none, so the
// fallback walks the keys sorted — deterministic, same first-value
// spirit (the tail only fires for rows missing all three keys).
func azPickTitle(titles map[string]string) string {
	for _, key := range []string{"1", "5", "8"} {
		if v := titles[key]; v != "" {
			return v
		}
	}
	keys := make([]string, 0, len(titles))
	for k := range titles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := titles[k]; v != "" {
			return v
		}
	}
	return ""
}

// azNormalizeURL ports normalizeUrl: collapsed escaped slashes.
func azNormalizeURL(s string) string { return azEscapedSlashRe.ReplaceAllString(s, "/") }

// azDecodeItems extracts and decodes the `items: JSON.parse('…')`
// payload of a Livewire page.
func azDecodeItems(body []byte) ([]azEpisodeItem, error) {
	m := azItemsRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("items payload not found")
	}
	decoded, err := azDecodeJSONArgument(string(m[1]))
	if err != nil {
		return nil, fmt.Errorf("decode items payload: %w", err)
	}
	var items []azEpisodeItem
	if err := json.Unmarshal(decoded, &items); err != nil {
		return nil, fmt.Errorf("decode items payload: %w", err)
	}
	return items, nil
}

// azDetailSnapshot extracts the entity-encoded pages.anime-detail
// wire:snapshot attribute and HTML-decodes it (recipe snapshot()).
func azDetailSnapshot(body []byte) string {
	for _, m := range azSnapshotRe.FindAllSubmatch(body, -1) {
		if strings.Contains(string(m[1]), "pages.anime-detail") {
			return html.UnescapeString(string(m[1]))
		}
	}
	return ""
}

// Search queries the Livewire index (recipe search + parseSearchItems):
// one page, slug-shaped rows with a title only.
func (p *AniZone) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("search", query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  http.MethodGet,
		URL:     p.baseURL + "/anime?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	m := azItemsRe.FindSubmatch(resp.Body)
	if m == nil {
		if azSearchInputRe.Match(resp.Body) {
			// The legit no-results page: the full Anime Index page
			// rendered with an empty result block and NO items
			// payload script (verbatim capture 2026-09-20, cyrillic
			// query — testdata/anizone_search_empty.html). A clean
			// miss, not a shape drift: the fan-out legitimately
			// reaches this latin-only index with cyrillic-only
			// variant sets (enrichment off), and a miss must settle
			// the row as zero results, never an error (PR78).
			return []contracts.SearchResult{}, nil
		}
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("search payload not found: %w", contracts.ErrExtractFailed))
	}
	decoded, err := azDecodeJSONArgument(string(m[1]))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search payload: %w", err))
	}
	var items []azSearchItem
	if err := json.Unmarshal(decoded, &items); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search payload: %w", err))
	}

	// parseSearchItems: slug-shaped rows with a title only.
	results := make([]contracts.SearchResult, 0, len(items))
	for _, item := range items {
		if !azSlugRe.MatchString(item.Slug) {
			continue
		}
		title := azPickTitle(item.TitleList)
		if title == "" {
			title = item.MainTitle
		}
		if title == "" {
			continue
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      item.Slug, // the hash slug doubles as the anime id
			SourceID: p.ID(),
			Poster:   azNormalizeURL(item.Cover),
			Meta: map[string]any{
				"year": item.StartYear,
				"type": item.Type,
			},
		})
	}
	return results, nil
}

// GetEpisodes walks the series listing (recipe scrapeSeries): the
// initial page's rows, then /livewire/update continuations while
// hasMore + nextCursor hold, bounded by azMaxSeriesPages. animeURL is
// the search slug.
func (p *AniZone) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	state, err := p.fetchSeriesPage(ctx, animeURL)
	if err != nil {
		return nil, err
	}
	items := state.items
	pages := 1
	for state.hasMore && state.cursor != "" {
		if pages >= azMaxSeriesPages {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
				fmt.Errorf("series listing exceeded %d pages", azMaxSeriesPages))
		}
		if state, err = p.loadPage(ctx, animeURL, state); err != nil {
			return nil, err
		}
		items = append(items, state.items...)
		pages++
	}

	episodes := azParseEpisodes(p.baseURL, animeURL, items)
	// The recipe throws on an empty list; the port keeps the typed
	// class (a series page that decodes but carries no resolvable
	// episode is a not-found, never a silent empty set).
	if len(episodes) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("no resolvable episodes for %s: %w", animeURL, contracts.ErrNotFound))
	}
	return episodes, nil
}

// fetchSeriesPage loads the initial series page and its Livewire state
// (recipe initialPage): items, the detail snapshot, the csrf — all
// three are required for the walk, their absence means a challenge page
// or a shape drift.
func (p *AniZone) fetchSeriesPage(ctx context.Context, slug string) (azLivewireState, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  http.MethodGet,
		URL:     p.baseURL + "/anime/" + url.PathEscape(slug),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return azLivewireState{}, err
	}

	state := azLivewireState{
		snapshot: azDetailSnapshot(resp.Body),
		cursor:   azPageCursor(resp.Body),
		hasMore:  azHasMoreRe.Match(resp.Body),
		csrf:     azPageCSRF(resp.Body),
	}
	if state.items, err = azDecodeItems(resp.Body); err != nil || len(state.items) == 0 ||
		state.snapshot == "" || state.csrf == "" {
		return azLivewireState{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("series page payload not found: %w", contracts.ErrExtractFailed))
	}
	return state, nil
}

// azPageCursor/azPageCSRF peel the pagination state off a series page.
func azPageCursor(body []byte) string {
	if m := azCursorRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

func azPageCSRF(body []byte) string {
	if m := azCSRFRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// loadPage performs one /livewire/update continuation (recipe loadPage):
// the current snapshot + loadPage(cursor) in, the next page's rows and
// walk state out via the items-loaded dispatch. Session cookies rotate
// every response — the netclient jar replays them.
func (p *AniZone) loadPage(ctx context.Context, slug string, state azLivewireState) (azLivewireState, error) {
	headers := map[string]string{
		"Referer":          p.baseURL + "/anime/" + slug,
		"Accept":           "application/json, text/plain, */*",
		"Content-Type":     "application/json",
		"X-Livewire":       "",
		"X-CSRF-TOKEN":     state.csrf,
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           p.baseURL,
		"Accept-Language":  "en-US,en;q=0.9",
	}
	body, err := json.Marshal(azLivewireRequest{
		Components: []azLivewireComponent{{
			Snapshot: state.snapshot,
			Updates:  map[string]any{},
			Calls: []azLivewireCall{{
				Path:   "",
				Method: "loadPage",
				Params: []string{state.cursor},
			}},
		}},
	})
	if err != nil {
		return azLivewireState{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("encode livewire request: %w", err))
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/livewire/update",
		Headers: headers,
		Body:    bytes.NewReader(body),
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return azLivewireState{}, err
	}

	var payload azLivewireResponse
	if jsonErr := json.Unmarshal(resp.Body, &payload); jsonErr != nil {
		return azLivewireState{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode livewire response: %w", jsonErr))
	}
	if len(payload.Components) != 1 || payload.Components[0].Snapshot == "" {
		return azLivewireState{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("livewire continuation payload not found: %w", contracts.ErrExtractFailed))
	}
	component := payload.Components[0]
	for _, dispatch := range component.Effects.Dispatches {
		if dispatch.Name != "items-loaded" {
			continue
		}
		if dispatch.Params.Items == nil {
			break
		}
		next := azLivewireState{
			items:    dispatch.Params.Items,
			snapshot: component.Snapshot,
			hasMore:  dispatch.Params.HasMore,
			csrf:     state.csrf, // the page token stays valid across pages (live-verified)
		}
		if dispatch.Params.NextCursor != nil {
			next.cursor = *dispatch.Params.NextCursor
		}
		return next, nil
	}
	return azLivewireState{}, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
		fmt.Errorf("livewire items-loaded dispatch not found: %w", contracts.ErrExtractFailed))
}

// azEpisodeNumber ports episodeNumber: the numeric slug, else the
// trailing URL digits. Specials ("s1") carry neither — dropped.
func azEpisodeNumber(item azEpisodeItem) (int, bool) {
	if n, err := strconv.Atoi(item.Slug); err == nil && n > 0 {
		return n, true
	}
	if m := azTrailingNumberRe.FindStringSubmatch(item.URL); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

var azTrailingNumberRe = regexp.MustCompile(`/(\d+)/?$`)

// azParseEpisodes ports parseEpisodes: numeric episodes only, deduped
// by number (FIRST occurrence wins — the recipe adds to the seen set
// and drops later dupes), sorted ascending, gated on videos_count (the
// has-sub property; hasDub is always false on this site). RawEmbeds
// carries the watch URL — ResolveStream's only input.
func azParseEpisodes(baseURL, slug string, items []azEpisodeItem) []contracts.Episode {
	type entry struct {
		number int
		title  string
		hasSub bool
	}
	seen := make(map[int]struct{}, len(items))
	entries := make([]entry, 0, len(items))
	for _, item := range items {
		number, ok := azEpisodeNumber(item)
		if !ok {
			continue // specials ("s1") carry neither a numeric slug nor URL tail
		}
		if _, dup := seen[number]; dup {
			continue
		}
		seen[number] = struct{}{}
		entries = append(entries, entry{
			number: number,
			title:  azEpisodeTitle(item, number),
			hasSub: item.VideosCount > 0,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].number < entries[j].number })

	episodes := make([]contracts.Episode, 0, len(entries))
	for _, e := range entries {
		if !e.hasSub {
			continue // videos_count == 0: no video behind the row
		}
		num := strconv.Itoa(e.number)
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: e.title,
			RawID: num,
			RawEmbeds: map[string][]string{
				anizoneDub: {fmt.Sprintf("%s/anime/%s/%s", baseURL, slug, num)},
			},
		})
	}
	return episodes
}

// azEpisodeTitle ports the parseEpisodes title fallback.
func azEpisodeTitle(item azEpisodeItem, number int) string {
	if title := azPickTitle(item.TitleList); title != "" {
		return title
	}
	return fmt.Sprintf("Episode %d", number)
}

// ResolveStream resolves the watch page's vidstackPlayer payload into
// the HLS master playlist and splits it into its variants (the
// allanime house pattern — aaParseMasterPlaylist; live capture
// 2026-09-18: 360/720/1080 h264 variants over ja/en audio groups).
func (p *AniZone) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: anizoneDub,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 || embeds[0] == "" {
		return stream, nil
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  http.MethodGet,
		URL:     embeds[0],
		Headers: p.headers,
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return stream, err
	}

	m := azPlayerRe.FindSubmatch(resp.Body)
	if m == nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("player payload not found for %s: %w", embeds[0], contracts.ErrExtractFailed))
	}
	decoded, err := azDecodeJSONArgument(string(m[1]))
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("decode player payload: %w", err))
	}
	var player azPlayer
	if err := json.Unmarshal(decoded, &player); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("decode player payload: %w", err))
	}
	src := azNormalizeURL(player.Src)
	if src == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("player payload carries no src: %w", contracts.ErrExtractFailed))
	}

	// The master playlist is fetched with the site root as its Referer
	// (the original PR51 convention; the live CDN also answers
	// referer-less).
	playlistHeaders := map[string]string{"Referer": p.baseURL}
	playlist, err := p.http.Do(ctx, netclient.Request{
		Method:  http.MethodGet,
		URL:     src,
		Headers: playlistHeaders,
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}

	// A master playlist splits into its variant entries; a media
	// playlist means the src IS the stream (allanime parity).
	if variants, isVariant := aaParseMasterPlaylist(string(playlist.Body), src); isVariant {
		for _, v := range variants {
			stream.Links[v.height] = contracts.VideoSource{
				URL:     v.uri,
				Quality: v.height,
				Type:    "m3u8",
				Headers: playlistHeaders,
			}
		}
	} else if len(variants) == 0 {
		stream.Links["1080"] = contracts.VideoSource{
			URL:     src,
			Quality: "1080",
			Type:    "m3u8",
			Headers: playlistHeaders,
		}
	}
	return stream, nil
}
