package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnilibAPIBase is the JSON API root (anicli-py anicli/providers/
// anilib.py:19). Requests must carry the browser-mimicking header set
// below or the CDN answers 403.
const AnilibAPIBase = "https://api.cdnlibs.org/api"

// anilibReferer is the Referer required on resolved AnimeLib CDN streams
// (anilib.py:157).
const anilibReferer = "https://v3.animelib.org"

// anilibCDNBase is the literal (including the odd percent-encoded
// segment) CDN prefix for internal AnimeLib video paths (anilib.py:153).
const anilibCDNBase = "https://video1.cdnlibs.org/.%D0%B0s/"

// Anilib is the port of anicli-py anicli/providers/anilib.py.
// Source type BOTH; dubs are fetched lazily per episode via FetchDubs
// (Python fetch_dubs_for_episode) and cached in Episode.RawEmbeds.
type Anilib struct {
	Base
}

// newAnilib builds the provider against baseURL.
func newAnilib(baseURL string, http *netclient.Client) *Anilib {
	// Header set verbatim from anilib.py:27-41. The netclient applies
	// its own User-Agent and Accept-Language first; these overrides win
	// because per-request headers are applied last.
	headers := map[string]string{
		"Authority":          "api.cdnlibs.org",
		"Accept":             "application/json, text/plain, */*",
		"Accept-Language":    "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
		"Origin":             "https://animelib.me",
		"Referer":            "https://animelib.me/",
		"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		"Sec-Ch-Ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "cross-site",
	}
	return &Anilib{Base: Base{
		id:          "anilib",
		name:        "AnimeLib",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		headers:     headers,
		http:        http,
	}}
}

// anilibSearch mirrors the fields consumed by anilib.py:73-83.
type anilibSearch struct {
	Data []struct {
		RusName *string `json:"rus_name"`
		Name    *string `json:"name"`
		EngName *string `json:"eng_name"`
		SlugURL string  `json:"slug_url"`
		Cover   struct {
			Default string `json:"default"`
		} `json:"cover"`
	} `json:"data"`
}

// anilibEpisodes mirrors the fields consumed by anilib.py:102-112.
type anilibEpisodes struct {
	Data []struct {
		ID     json.Number  `json:"id"`
		Name   *string      `json:"name"`
		Number *json.Number `json:"number"`
	} `json:"data"`
}

// anilibEpisode mirrors the fields consumed by anilib.py:124-136.
type anilibEpisode struct {
	Data struct {
		Players []struct {
			Team struct {
				Name string `json:"name"`
			} `json:"team"`
			Player string `json:"player"`
			Src    string `json:"src"`
			Video  struct {
				Quality []struct {
					Href    string       `json:"href"`
					Quality *json.Number `json:"quality"`
				} `json:"quality"`
			} `json:"video"`
		} `json:"players"`
	} `json:"data"`
}

// Search queries /anime [LIVE-VERIFIED 2026-09-13: q, limit=20, site_id=5].
// The API now REJECTS the legacy browser parameter list — site_id[]=1 and
// the fields[] selectors — with HTTP 422 ("The selected value for fields.N
// is incorrect"); the flat site_id=5 form answers with the plain data
// array. The Python original wraps the whole call in
// `except Exception: return []`: HTTP and decode failures surface as an
// empty result set here, verbatim (documented quirk, see also the test).
//
// The Python `("q", unquote(query))` parameter is preserved: the query is
// percent-DECODED before being re-encoded by the request layer. Python's
// unquote leaves a literal "+" untouched (and never fails on invalid
// escapes); Go's behavioral twin is url.PathUnescape — QueryUnescape
// would decode "+" to a space, diverging from the Python request shape.
// Invalid escapes fall back to the raw query, matching unquote's
// pass-through.
func (p *Anilib) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	decoded, err := url.PathUnescape(query)
	if err != nil {
		decoded = query
	}

	params := url.Values{}
	params.Set("q", decoded)
	params.Set("limit", "20")
	params.Set("site_id", "5") // [LIVE-VERIFIED 2026-09-13] was site_id[]=1

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/anime?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return []contracts.SearchResult{}, nil
	}

	var data anilibSearch
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return []contracts.SearchResult{}, nil
	}

	results := make([]contracts.SearchResult, 0, len(data.Data))
	for _, item := range data.Data {
		// Python or-chain: first non-empty of rus_name, name, eng_name.
		title := firstNonEmpty(deref(item.RusName), deref(item.Name), deref(item.EngName))
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      item.SlugURL,
			SourceID: p.ID(),
			Poster:   item.Cover.Default,
		})
	}
	return p.filterContentless(ctx, results), nil
}

// anilibProbeConcurrency bounds the contentless-release preflight
// fan-out (the same bounded-pool shape the smoke suite uses).
const anilibProbeConcurrency = 8

// filterContentless drops contentless catalog entries from the search
// surface (PR53 owner ruling): releases whose episode/dub data is
// empty per the API shape — the same data the PR44 release-dub model
// hydrates. The API exposes no distinguishing field on the search
// payload itself (the CM/«Реклама» entries ride ordinary catalog
// rows; type.id=0 also covers real entries), so the preflight rides
// the exact endpoints GetEpisodes uses: the release's episode list,
// then the sorted-first episode's detail. A release is contentless
// when it has no episodes at all or its first episode's players list
// is empty.
//
// Fail-open: a failed probe (transport, non-200, decode) keeps the
// result — absence of evidence is not evidence of contentlessness, and
// a flaky API must not empty the search. Only a positively-empty
// players list drops a release; drops are logged with the reason.
//
// Latency budget: two small JSON requests per result, ≤8 concurrent —
// a 20-result search pays ~5 rounds ≈ 2-3s live, documented trade-off
// for surfacing only playable results.
func (p *Anilib) filterContentless(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	if len(results) == 0 {
		return results
	}

	keep := make([]bool, len(results))
	for i := range keep {
		keep[i] = true // fail-open default
	}
	type indexed struct {
		i int
		r contracts.SearchResult
	}
	items := make([]indexed, len(results))
	for i, r := range results {
		items[i] = indexed{i: i, r: r}
	}
	_ = netclient.Parallel(ctx, items, anilibProbeConcurrency, func(ctx context.Context, it indexed) error {
		contentless, err := p.probeContentless(ctx, it.r.URL)
		switch {
		case err != nil:
			p.loggerOrDiscard().Info("anilib: preflight failed, keeping release", "release", it.r.URL, "reason", err)
		case contentless:
			keep[it.i] = false
			p.loggerOrDiscard().Info("anilib: dropped contentless release", "release", it.r.URL, "title", it.r.Title)
		}
		return nil // never aborts the group; per-result outcomes recorded above
	})

	kept := make([]contracts.SearchResult, 0, len(results))
	for i, r := range results {
		if keep[i] {
			kept = append(kept, r)
		}
	}
	return kept
}

// probeContentless reports whether the release behind a search-result
// slug ("25322--black-lagoon-cm-anime") carries no dub content: the
// episode list is empty, or the sorted-first episode's players list is
// empty. The sort mirrors GetEpisodes so the probed episode is exactly
// the one the release-dub model hydrates. Any transport/decode failure
// surfaces as an error (fail-open upstream).
func (p *Anilib) probeContentless(ctx context.Context, slugURL string) (bool, error) {
	animeID := slugURL
	if before, _, found := strings.Cut(slugURL, "--"); found {
		animeID = before
	}

	params := url.Values{}
	params.Set("anime_id", animeID)
	listResp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/episodes?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return false, err
	}

	var list anilibEpisodes
	if err := json.Unmarshal(listResp.Body, &list); err != nil {
		return false, fmt.Errorf("decode episodes of %s: %w", animeID, err)
	}
	if len(list.Data) == 0 {
		return true, nil
	}

	// Sorted-first episode, pythonFloatKey parity with GetEpisodes.
	first := list.Data[0]
	for _, item := range list.Data[1:] {
		if pythonFloatKey(pythonStr(derefNum(item.Number))) < pythonFloatKey(pythonStr(derefNum(first.Number))) {
			first = item
		}
	}

	detailResp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/episodes/" + first.ID.String(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return false, err
	}
	var detail anilibEpisode
	if err := json.Unmarshal(detailResp.Body, &detail); err != nil {
		return false, fmt.Errorf("decode episode %s: %w", first.ID.String(), err)
	}
	return len(detail.Data.Players) == 0, nil
}

// GetEpisodes lists episodes of the anime identified by a "ID--slug"
// URL: the numeric id is split off the slug prefix (anicli-py
// anilib.py:86-114). Episodes come back numerically sorted with the
// Python quirk that a JSON-null number renders as "None" and sorts with
// key 0.
func (p *Anilib) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	// Slug format is "ID--slug-name"; the API wants the numeric id
	// (anilib.py:91).
	animeID := animeURL
	if before, _, found := strings.Cut(animeURL, "--"); found {
		animeID = before
	}

	params := url.Values{}
	params.Set("anime_id", animeID)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/episodes?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return []contracts.Episode{}, nil
	}

	var data anilibEpisodes
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return []contracts.Episode{}, nil
	}

	episodes := make([]contracts.Episode, 0, len(data.Data))
	for _, item := range data.Data {
		name := "Episode"
		if item.Name != nil && *item.Name != "" {
			name = *item.Name
		}
		episodes = append(episodes, contracts.Episode{
			Num:       pythonStr(derefNum(item.Number)),
			Title:     name,
			RawID:     item.ID.String(),
			RawEmbeds: map[string][]string{},
		})
	}

	sort.SliceStable(episodes, func(i, j int) bool {
		return pythonFloatKey(episodes[i].Num) < pythonFloatKey(episodes[j].Num)
	})

	// PR44 owner model: one request per RELEASE covers the dub-
	// provider list — the first episode's players name every team
	// voicing the release. Apply the key set to all episodes (episode
	// one keeps its real links, the rest carry EMPTY lists). Fail-soft:
	// a failed tier-1 fetch only means the dub lists stay unknown
	// until an episode is opened.
	if len(episodes) > 0 {
		if _, err := p.FetchDubs(ctx, &episodes[0]); err == nil {
			applyReleaseDubKeys(episodes)
		}
	}
	return episodes, nil
}

// FetchDubs hydrates episode.RawEmbeds from the per-episode players list
// (port of anilib.py:116-139 fetch_dubs_for_episode). Kodik players are
// stashed as their embed src; AnimeLib players as an internal: JSON
// payload. The Python original swallows transport/decode failures and
// returns the episode unchanged; this port FAILS LOUD instead — the
// caller (the on-demand resolve step, PR44) logs the failure to the
// diagnostics file and continues with no dubs, which is the same UX
// without the silent path. Callers that prefer the Python semantics
// ignore the error and keep the unchanged episode.
func (p *Anilib) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/episodes/" + episode.RawID,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return episode, fmt.Errorf("fetch dubs for episode %s: %w", episode.RawID, err)
	}

	var data anilibEpisode
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return episode, fmt.Errorf("decode dubs of episode %s: %w", episode.RawID, jsonErr)
	}

	embeds := map[string][]string{}
	for _, player := range data.Data.Players {
		team := player.Team.Name
		if team == "" {
			team = "Unknown" // Python .get("team", {}).get("name", "Unknown")
		}
		key := fmt.Sprintf("%s (%s)", team, player.Player)

		switch player.Player {
		case "Kodik":
			embeds[key] = []string{player.Src}
		case "AnimeLib":
			payload, err := json.Marshal(player.Video)
			if err != nil {
				// struct is always marshalable; kept for honesty.
				return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
					fmt.Errorf("encode internal video payload: %w", err))
			}
			embeds[key] = []string{"internal:" + string(payload)}
		}
	}

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream turns the raw embeds of the chosen dub into quality-
// keyed VideoSources (port of anilib.py:141-163): internal: payloads
// resolve to the video1.cdnlibs.org CDN with the v3.animelib.org
// Referer; embed URLs go through the extractor factory.
//
// PR44 owner model: the release's dub list rides every episode as
// keys with EMPTY lists (streams are temporary — the srcs are not
// fetched until needed). A known-but-empty dub self-hydrates that ONE
// episode here (a single /episodes/{id} request), so resolving never
// runs bulk and never pre-fetches unopened episodes. An unknown dub
// key hydrates nothing.
func (p *Anilib) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	if links, ok := episode.RawEmbeds[dubID]; ok && len(links) == 0 {
		if _, err := p.FetchDubs(ctx, &episode); err != nil {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
		}
	}

	for _, link := range episode.RawEmbeds[dubID] {
		if payload, ok := strings.CutPrefix(link, "internal:"); ok {
			p.resolveInternal(stream.Links, payload)
			continue
		}
		if strings.HasPrefix(link, "//") {
			link = "https:" + link
		}
		// Python merges extractor output via dict.update and never fails;
		// a link whose extraction fails surfaces its error only while no
		// source has resolved yet (task ruling), never shadowing sources
		// already resolved.
		sources, err := resolveEmbeds(ctx, p.http, []string{link})
		if err != nil {
			if len(stream.Links) == 0 {
				return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
			}
			continue
		}
		for quality, src := range sources {
			stream.Links[quality] = src
		}
	}
	return stream, nil
}

// resolveInternal decodes an internal: payload into CDN links
// (anilib.py:146-159). Malformed payloads are skipped, matching the
// Python bare `except: pass`.
func (p *Anilib) resolveInternal(links map[string]contracts.VideoSource, payload string) {
	var video struct {
		Quality []struct {
			Href    string       `json:"href"`
			Quality *json.Number `json:"quality"`
		} `json:"quality"`
	}
	if err := json.Unmarshal([]byte(payload), &video); err != nil {
		return
	}
	for _, q := range video.Quality {
		if q.Href == "" {
			continue
		}
		quality := "1080" // Python q.get("quality", 1080)
		if q.Quality != nil {
			quality = q.Quality.String()
		}
		links[quality] = contracts.VideoSource{
			URL:     anilibCDNBase + q.Href,
			Quality: quality,
			Headers: map[string]string{"Referer": anilibReferer},
		}
	}
}

// deref returns the pointed-to string or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefNum returns the pointed-to json.Number or the zero value for nil.
func derefNum(n *json.Number) json.Number {
	if n == nil {
		return ""
	}
	return *n
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// applyReleaseDubKeys distributes the FIRST episode's dub keys onto
// every other episode of the release as keys with EMPTY link lists
// (PR44 owner model: the dub-provider list is release-scoped — one
// request covers it — while the streams are per-episode and resolve
// on demand). Episodes already carrying a key keep it untouched.
func applyReleaseDubKeys(episodes []contracts.Episode) {
	if len(episodes) == 0 {
		return
	}
	first := episodes[0].RawEmbeds
	for i := 1; i < len(episodes); i++ {
		if episodes[i].RawEmbeds == nil {
			episodes[i].RawEmbeds = map[string][]string{}
		}
		for dub := range first {
			if _, ok := episodes[i].RawEmbeds[dub]; !ok {
				episodes[i].RawEmbeds[dub] = []string{}
			}
		}
	}
}
