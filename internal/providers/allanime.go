package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Domain constants for the post-2026-07-22 rotation (verified
// 2026-09-12; the Python originals api.allanime.day + allmanga.to are
// dead or answer stripped — ani-cli PR #1779).
const (
	// AllAnimeReferer is the Referer/Origin the API is bound to.
	AllAnimeReferer = "https://mkissa.to"
	// AllAnimeAPIBase is the GraphQL endpoint root.
	AllAnimeAPIBase = "https://api.mkissa.net/api"
	// AllAnimeInternalBase is the host internal ("--"-encoded) source
	// URLs resolve against (the /clock.json embeds); unchanged by the
	// rotation.
	AllAnimeInternalBase = "https://allanime.day"
	// aaContentLane is the episode content lane — the k field of the
	// bootstrap and aaReq payloads. [LIVE-VERIFIED 2026-09-13] (live
	// ap() picks k7 for /episode\s*\(/ queries).
	aaContentLane = "k7"
)

// aaSearchQuery is the site's search document (live o7(false) builder
// with the Ri selection resolved), byte-for-byte as POSTed by the real
// player — including englishName/nativeName, which the pre-rotation
// document lacked. [LIVE-VERIFIED 2026-09-13]: this exact text returned
// ROAD OF NARUTO from POST /api.
const aaSearchQuery = `
query(
$search: SearchInput
$limit: Int
$page: Int
$translationType: VaildTranslationTypeEnumType
$countryOrigin: VaildCountryOriginEnumType
) {
shows(
search: $search
limit: $limit
page: $page
translationType: $translationType
countryOrigin: $countryOrigin
) {
pageInfo {
total
}
edges {


_id
name
englishName
nativeName
slugTime
thumbnail

tbObj {
  u
  sm
  md
  ts
}
lastEpisodeInfo
lastEpisodeDate
type
season
score
airedStart
availableEpisodes
episodeDuration
episodeCount
# lastUpdateStart
lastUpdateEnd
characterCount
playlistCount
tierListCount
worldMapCount
caseFileCount

siteRanks {
  entries {
    key
    label
    position
    delta
    sortOrder
    score
    windowDays
  }
  weekly { score windowDays }
  monthly { score windowDays }
  overall { score windowDays }
  bookmarked { score windowDays }
  bookmarkedOverall { score windowDays }
  boostedMonthly { score windowDays }
  boosted { score windowDays }
}


}
}
}`

// aaEpisodesQuery lists available episodes per translation.
// [LIVE-VERIFIED 2026-09-13] via POST.
const aaEpisodesQuery = `
    query ($showId: String!) {
        show(
            _id: $showId
        ) {
            _id,
            availableEpisodesDetail
        }
    }
`

// aaEpisodeQuery is the episode-sources document, byte-identical to
// the one POSTed during the live characterization (its SHA-256 equals
// the live persisted-query hash 2654f89a…, pinned in
// TestAAQueryDocsMatchLive). The show{_id} subselection is REQUIRED:
// without it the live episode resolver crashes server-side ("Cannot
// set properties of undefined (setting 'countryOfOrigin')") — bisected
// live on 2026-09-13. [LIVE-VERIFIED 2026-09-13].
const aaEpisodeQuery = "\nquery( $showId: String!, $translationType: VaildTranslationTypeEnumType!, $episodeString: String! ) { episode( showId: $showId translationType: $translationType episodeString: $episodeString ) { episodeString sourceUrls show { _id } } }"

// aaEpisodeQueryHash is SHA-256 of aaEpisodeQuery — the persisted-query
// hash the site computes client-side over the exact document text
// (live bl() is plain SHA-256; the server accepts any document paired
// with its own hash). [LIVE-VERIFIED 2026-09-13].
var aaEpisodeQueryHash = func() string {
	sum := sha256.Sum256([]byte(aaEpisodeQuery))
	return hex.EncodeToString(sum[:])
}()

// aaSearchLimit is the search page size the live player uses.
// [LIVE-VERIFIED 2026-09-13].
const aaSearchLimit = 40

// aaPreferredProviders is the ani-cli provider priority (dispatch
// ruling): sources are processed in this order first, everything else
// keeps response order. The Python original iterated sourceUrls raw
// (allanime.py:190) — priority is a mandated improvement.
var aaPreferredProviders = []string{"Default", "S-mp4", "Luf-Mp4", "Yt-mp4"}

// aaResolutionRe extracts the height from a RESOLUTION=WxH attribute.
var aaResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)[xX](\d+)`)

// AllAnime is the mkissa.to provider speaking the v3 protocol:
// GraphQL POSTs against api.mkissa.net with the mkissa.to
// Referer/Origin, per-epoch AES material from the client-crypto
// bootstrap (allanime_bootstrap.go), an aaReq proof token on
// episode-source queries and AES-256-GCM tobeparsed decryption
// (allanime_proto.go). Resolution falls back to a live browser-bridge
// (allanime_bridge.go) when the pure-Go crypto fails.
type AllAnime struct {
	Base

	// apiBase is the GraphQL endpoint root.
	apiBase string
	// bootstrapBase is the client-crypto bootstrap endpoint root,
	// derived from apiBase (…/client-crypto/v1/bootstrap).
	bootstrapBase string
	// internalBase absolutizes decoded "--" URLs.
	internalBase string
	// referer is the Referer/Origin value sent on every request.
	referer string
	// refererHost feeds the x-aa-boot key-group folding (gT).
	refererHost string
	// material derives and caches the per-epoch AES key.
	material *aaMaterialManager
	// buildIDs persists bridge-discovered buildIds.
	buildIDs *aaBuildIDCache
	// bridge re-derives crypto material in a real browser when the
	// pure-Go path fails (nil = disabled; typed errors then).
	bridge aaBridgeSource
	// rateLimitBackoff overrides the rate-limit retry wait (tests);
	// 0 keeps the default. Per-instance so tests never mutate shared
	// state (a package var raced under -race).
	rateLimitBackoff time.Duration
}

// newAllAnime builds the provider against the API base, the referer
// page origin and the internal-URL base. The bases are injectable so
// tests run the full protocol against fake servers (production values
// are the package constants). bridge may be nil (no fallback);
// cacheDir persists the bridge-discovered buildId ("" = memory only).
func newAllAnime(apiBase, referer, internalBase string, http *netclient.Client, bridge aaBridgeSource, cacheDir string) *AllAnime {
	p := &AllAnime{
		Base: Base{
			id:          "allanime",
			name:        "AllAnime",
			baseURL:     referer,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja", // primary sub track; the dub track is "en"
			headers: map[string]string{
				"Referer": referer,
				"Origin":  referer,
			},
			http: http,
		},
		apiBase:       apiBase,
		bootstrapBase: strings.TrimSuffix(apiBase, "/api") + "/client-crypto/v1/bootstrap",
		internalBase:  internalBase,
		referer:       referer,
		refererHost:   refererHostOf(referer),
		bridge:        bridge,
		buildIDs:      newAABuildIDCache(cacheDir),
	}
	p.material = newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: p.bootstrapBase,
		Referer:       referer,
		RefererHost:   p.refererHost,
		Lane:          aaContentLane,
		BuildID: func() (string, error) {
			return p.buildIDs.Load(), nil
		},
		HTTP: http,
		Now:  time.Now,
	})
	return p
}

// refererHostOf extracts the host of an origin URL (empty on parse
// failure).
func refererHostOf(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// graphqlPost issues the GraphQL POST with the provider headers.
// [LIVE-VERIFIED 2026-09-13]: the live player POSTs every query
// (GET is refused by the API since the v3 rotation).
func (p *AllAnime) graphqlPost(ctx context.Context, payload any, extra map[string]string) ([]byte, error) {
	hdrs := map[string]string{"Content-Type": "application/json"}
	for k, v := range p.headers {
		hdrs[k] = v
	}
	for k, v := range extra {
		hdrs[k] = v
	}
	resp, err := p.http.PostJSON(ctx, p.apiBase, payload, hdrs)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// aaGraphqlRequest is the POST body {query, variables, extensions?}.
type aaGraphqlRequest struct {
	Query      string         `json:"query"`
	Variables  map[string]any `json:"variables"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// Search runs the shows(search:) query and sorts by difflib similarity
// (port of allanime.py:80-117). Transport and decode failures return an
// empty set (the Python except boundary).
func (p *AllAnime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	body, err := p.graphqlPost(ctx, aaGraphqlRequest{
		Query: aaSearchQuery,
		Variables: map[string]any{
			"search": map[string]any{
				"allowAdult":   false,
				"allowUnknown": false,
				"query":        query,
			},
			"limit":           aaSearchLimit,
			"page":            1,
			"translationType": "sub",
			"countryOrigin":   "ALL",
		},
	}, nil)
	if err != nil {
		return []contracts.SearchResult{}, nil
	}

	var parsed struct {
		Data struct {
			Shows struct {
				Edges []struct {
					ID        string `json:"_id"`
					Name      string `json:"name"`
					Thumbnail string `json:"thumbnail"`
				} `json:"edges"`
			} `json:"shows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return []contracts.SearchResult{}, nil
	}

	results := make([]contracts.SearchResult, 0, len(parsed.Data.Shows.Edges))
	for _, show := range parsed.Data.Shows.Edges {
		results = append(results, contracts.SearchResult{
			Title:    show.Name,
			URL:      show.ID,
			SourceID: p.ID(),
			Poster:   show.Thumbnail,
		})
	}
	sortAllAnimeBySimilarity(query, results)
	return results, nil
}

// GetEpisodes lists the union of sub and dub episode numbers, float
// sorted, with placeholder embeds marking availability (port of
// allanime.py:119-166). Transport and decode failures return an empty
// list (the Python except boundary).
func (p *AllAnime) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	body, err := p.graphqlPost(ctx, aaGraphqlRequest{
		Query:     aaEpisodesQuery,
		Variables: map[string]any{"showId": animeURL},
	}, nil)
	if err != nil {
		return []contracts.Episode{}, nil
	}

	var parsed struct {
		Data struct {
			Show struct {
				AvailableEpisodesDetail map[string][]any `json:"availableEpisodesDetail"`
			} `json:"show"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return []contracts.Episode{}, nil
	}
	details := parsed.Data.Show.AvailableEpisodesDetail
	subEps := aaEpisodeNumSet(details["sub"])
	dubEps := aaEpisodeNumSet(details["dub"])

	merged := make([]string, 0, len(subEps)+len(dubEps))
	for num := range subEps {
		merged = append(merged, num)
	}
	for num := range dubEps {
		if _, dup := subEps[num]; !dup {
			merged = append(merged, num)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return aaParseNum(merged[i]) < aaParseNum(merged[j])
	})

	episodes := make([]contracts.Episode, 0, len(merged))
	for _, num := range merged {
		embeds := map[string][]string{}
		if _, ok := subEps[num]; ok {
			embeds["sub"] = []string{"sub"}
		}
		if _, ok := dubEps[num]; ok {
			embeds["dub"] = []string{"dub"}
		}
		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     "Episode " + num,
			RawID:     animeURL,
			RawEmbeds: embeds,
		})
	}
	return episodes, nil
}

// ResolveStream runs the episode query with the aaReq token, decrypts
// tobeparsed, decodes the "--" source URLs and expands them through the
// clock.json flow (port of allanime.py:168-271 on the v3 transport).
// The crypto ladder: pure-Go derivation → on a typed rotation failure
// one bridge handoff + one retry → loud typed error. Per-source
// expansion failures skip the source (the Python inner try/except).
func (p *AllAnime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	sources, err := p.fetchEpisodeSources(ctx, episode, dubID)
	if err != nil {
		// Crypto failures never collapse to an empty stream — the
		// no-silent-failure policy; search/episodes keep their []
		// boundary, resolve is loud.
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: %w", contracts.ErrExtractFailed, err))
	}

	for _, src := range sources {
		// Decode once [M7]; the classification then only decides where
		// the decoded URL goes.
		raw, ok := decodeAllAnimeSourceURL(src.URL, p.internalBase)
		if !ok {
			continue
		}
		// Direct-media entries: live type=="player" sources (and any
		// URL that names a media file) become links without a clock
		// fetch. [LIVE-VERIFIED 2026-09-13]: the Yt-mp4 source carries
		// type "player" / fallBack "mp4".
		if aaIsDirectMedia(src) {
			stream.Links["1080"] = contracts.VideoSource{URL: raw, Quality: "1080"}
			continue
		}
		p.expandClockLinks(ctx, &stream, raw)
	}
	return stream, nil
}

// aaIsDirectMedia reports whether a decoded source is a playable media
// file rather than a player-page embed: live type "player" entries, or
// URLs containing .mp4/.m3u8 (allanime.py:211-213).
func aaIsDirectMedia(src aaSource) bool {
	if src.Type == "player" || strings.EqualFold(src.FallBack, "mp4") {
		return true
	}
	return strings.Contains(src.URL, ".mp4") || strings.Contains(src.URL, ".m3u8")
}

// expandClockLinks fetches one decoded clock URL and folds its links
// into the stream (port of allanime.py:215-261). Failures skip the
// source silently — the Python inner try/except pass boundary.
func (p *AllAnime) expandClockLinks(ctx context.Context, stream *contracts.MediaStream, clockURL string) {
	body, err := p.fetchText(ctx, clockURL)
	if err != nil || body == "" {
		return
	}
	var clock struct {
		Links []struct {
			Link string `json:"link"`
			HLS  any    `json:"hls"`
		} `json:"links"`
	}
	if err := json.Unmarshal([]byte(body), &clock); err != nil {
		return
	}

	for _, entry := range clock.Links {
		src := entry.Link
		if src == "" {
			continue
		}
		if strings.Contains(src, "m3u8") || pyTruthy(entry.HLS) {
			// The playlist is fetched with the clock URL as its own
			// Referer (Python allanime.py:233-236 parity, F30): the CDN
			// serving it rejects the provider Referer.
			m3u8Headers := map[string]string{"Referer": clockURL}
			resp, err := p.http.Get(ctx, src, m3u8Headers)
			if err != nil {
				continue
			}
			playlist := string(resp.Body)
			if variants, isVariant := aaParseMasterPlaylist(playlist, src); isVariant {
				for _, v := range variants {
					stream.Links[v.height] = contracts.VideoSource{
						URL:     v.uri,
						Quality: v.height,
						Type:    "m3u8",
						Headers: m3u8Headers,
					}
				}
			} else if len(variants) == 0 {
				// Not a variant playlist: the URL itself is the stream
				// (allanime.py:253-259).
				stream.Links["1080"] = contracts.VideoSource{
					URL:     src,
					Quality: "1080",
					Type:    "m3u8",
					Headers: m3u8Headers,
				}
			}
		} else if strings.Contains(src, ".mp4") {
			stream.Links["1080"] = contracts.VideoSource{URL: src, Quality: "1080", Type: "mp4"}
		}
	}
}

// fetchEpisodeSources runs the episode query with protocol retries:
// AA_CRYPTO errors force one material refresh (fresh token on the next
// attempt — live XL retry parity); a still-failing rotation escalates
// to one bridge handoff and one final attempt; GCM failures force one
// refresh too. The loop is bounded by the once-flags.
func (p *AllAnime) fetchEpisodeSources(ctx context.Context, episode contracts.Episode, dubID string) ([]aaSource, error) {
	variables := map[string]any{
		"showId":          episode.RawID,
		"translationType": dubID,
		"episodeString":   episode.Num,
	}

	mat, err := p.material.get(ctx)
	if err != nil {
		if p.bridge != nil && errors.Is(err, errAABuildUnknown) {
			return p.resolveViaBridge(ctx, variables)
		}
		return nil, err
	}

	var (
		refreshed        bool
		bridged          bool
		rateLimitRetried bool
	)
	for range 4 {
		token, err := aaBuildAAReqAt(aaEpisodeQueryHash, mat.Key, mat.Epoch, mat.BuildID, aaContentLane, time.Now().UnixMilli())
		if err != nil {
			return nil, err
		}
		extensions := map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": aaEpisodeQueryHash,
			},
			"k":     aaContentLane,
			"aaReq": token,
		}
		// The x-build-id header is mandatory: without it the server
		// answers AA_CRYPTO_MISSING_BUILD (bisected live).
		body, err := p.graphqlPost(ctx, aaGraphqlRequest{
			Query:      aaEpisodeQuery,
			Variables:  variables,
			Extensions: extensions,
		}, map[string]string{"x-build-id": mat.BuildID})
		if err != nil {
			return nil, fmt.Errorf("allanime: episode query transport: %w", err)
		}

		if aaHasAACryptoError(body) {
			if refreshed {
				if !bridged && p.bridge != nil {
					bridged = true
					return p.resolveViaBridge(ctx, variables)
				}
				return nil, errAACryptoRotated
			}
			refreshed = true
			if mat, err = p.material.refresh(ctx); err != nil {
				if errors.Is(err, errAABuildUnknown) && p.bridge != nil {
					return p.resolveViaBridge(ctx, variables)
				}
				return nil, err
			}
			continue
		}

		// The live limiter throttles the episode query ("Too many
		// requests, please try again in 5 seconds." — observed live
		// 2026-09-17 on back-to-back resolves). Back off once and retry
		// the same query; a persisting limit falls through to the
		// decode path and fails loud. NEED_CAPTCHA is a different
		// verdict entirely — a site-side wall (PR67, 2026-09-19) — and
		// is never retried here; it flows to the decode path and
		// surfaces as errAACaptcha on the first attempt.
		if aaIsAARateLimited(body) {
			if !rateLimitRetried {
				rateLimitRetried = true
				timer := time.NewTimer(aaRateLimitWait(p.rateLimitBackoff))
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}

		if blob := aaExtractToBeParsedBlob(body); blob != "" {
			sources, derr := aaDecryptToBeParsed(blob, mat.Key)
			if errors.Is(derr, errAAGCMAuth) && !refreshed {
				refreshed = true
				if mat, err = p.material.refresh(ctx); err != nil {
					if errors.Is(err, errAABuildUnknown) && p.bridge != nil {
						return p.resolveViaBridge(ctx, variables)
					}
					return nil, err
				}
				sources, derr = aaDecryptToBeParsed(blob, mat.Key)
			}
			if derr != nil {
				if errors.Is(derr, errAAGCMAuth) {
					return nil, fmt.Errorf("%w: %w", errAADecryptFailed, derr)
				}
				return nil, derr
			}
			return aaPrioritizeSources(sources), nil
		}

		// Unencrypted response: accept sourceUrls directly.
		return aaDecodePlainSources(body)
	}
	return nil, errAACryptoRotated
}

// resolveViaBridge runs the browser-bridge handoff and one final
// attempt with the live-derived material.
func (p *AllAnime) resolveViaBridge(ctx context.Context, variables map[string]any) ([]aaSource, error) {
	bm, err := p.bridge.ExtractCrypto(ctx)
	if err != nil {
		return nil, fmt.Errorf("allanime: bridge: %w", err)
	}
	if err := p.buildIDs.Store(bm.BuildID); err == nil {
		p.material.setBuildID(bm.BuildID)
	}
	if err := p.material.adoptBridge(ctx, bm); err != nil {
		return nil, fmt.Errorf("allanime: bridge adopt: %w", err)
	}
	mat, err := p.material.get(ctx)
	if err != nil {
		return nil, err
	}
	token, err := aaBuildAAReqAt(aaEpisodeQueryHash, mat.Key, mat.Epoch, mat.BuildID, aaContentLane, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	body, err := p.graphqlPost(ctx, aaGraphqlRequest{
		Query:     aaEpisodeQuery,
		Variables: variables,
		Extensions: map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": aaEpisodeQueryHash,
			},
			"k":     aaContentLane,
			"aaReq": token,
		},
	}, map[string]string{"x-build-id": mat.BuildID})
	if err != nil {
		return nil, fmt.Errorf("allanime: episode query transport: %w", err)
	}
	if aaHasAACryptoError(body) {
		return nil, errAACryptoRotated
	}
	if blob := aaExtractToBeParsedBlob(body); blob != "" {
		sources, derr := aaDecryptToBeParsed(blob, mat.Key)
		if derr != nil {
			if errors.Is(derr, errAAGCMAuth) {
				return nil, fmt.Errorf("%w: %w", errAADecryptFailed, derr)
			}
			return nil, derr
		}
		return aaPrioritizeSources(sources), nil
	}
	return aaDecodePlainSources(body)
}

// aaGraphQLErrorMessages collects the GraphQL errors[] messages and
// extension codes of a response body (deduped, response order) — ""
// entries skipped. AA_CRYPTO signals are intentionally included; the
// caller has already ruled them out by the time it asks.
func aaGraphQLErrorMessages(body []byte) []string {
	var parsed struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	var msgs []string
	seen := map[string]struct{}{}
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		msgs = append(msgs, s)
	}
	for _, e := range parsed.Errors {
		add(e.Message)
		add(e.Extensions.Code)
	}
	return msgs
}

// aaDecodePlainSources decodes an unencrypted episode response [I3].
// A body carrying episode data (even with zero sources) is the
// deliberate empty passthrough — null episode WITHOUT errors stays
// (nil, nil). A body WITHOUT episode data but WITH GraphQL errors[]
// never collapses to a silent empty stream: NEED_CAPTCHA surfaces the
// typed errAACaptcha, any other error surfaces a descriptive error
// (wrapped into the contracts family by ResolveStream).
func aaDecodePlainSources(body []byte) ([]aaSource, error) {
	var parsed struct {
		Data struct {
			Episode *struct {
				SourceUrls []aaSourceEntry `json:"sourceUrls"`
			} `json:"episode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Data.Episode != nil {
		entries := parsed.Data.Episode.SourceUrls
		if len(entries) == 0 {
			return nil, nil
		}
		sources := make([]aaSource, 0, len(entries))
		for _, su := range entries {
			sources = append(sources, aaSource{
				Name:     su.SourceName,
				URL:      strings.TrimPrefix(su.SourceURL, "--"),
				Type:     su.Type,
				FallBack: su.FallBack,
			})
		}
		return aaPrioritizeSources(sources), nil
	}
	msgs := aaGraphQLErrorMessages(body)
	for _, msg := range msgs {
		if strings.HasPrefix(msg, "NEED_CAPTCHA") {
			return nil, fmt.Errorf("%w: %s", errAACaptcha, msg)
		}
	}
	if len(msgs) > 0 {
		return nil, fmt.Errorf("allanime: graphql error response: %s", strings.Join(msgs, "; "))
	}
	return nil, nil
}

// fetchText GETs url with the provider headers and returns the body.
func (p *AllAnime) fetchText(ctx context.Context, url string) (string, error) {
	resp, err := p.http.Get(ctx, url, p.headers)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// aaRateLimitBackoff is the default wait before the single rate-limit
// retry on the episode query (the live limiter's own "try again in 5
// seconds" contract). Instances override via rateLimitBackoff.
const aaRateLimitBackoff = 5 * time.Second

// aaRateLimitWait renders the effective wait: the base plus a small
// jitter that de-synchronizes concurrent resolves — without it all
// throttled retries land in the same instant and form a second burst.
func aaRateLimitWait(base time.Duration) time.Duration {
	if base <= 0 {
		base = aaRateLimitBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(2 * time.Second))) //nolint:gosec // non-crypto jitter to de-synchronize retry bursts — not a security boundary
	return base + jitter
}

// aaIsAARateLimited reports the live episode-query throttle signal:
// the explicit "Too many requests, please try again in 5 seconds."
// message. NEED_CAPTCHA is deliberately NOT classified here: the
// PR67 root-cause probe (2026-09-19) proved it is a site-side wall on
// the episode-sources query — it survives fresh crypto material,
// buildId rotation, both egresses, polite pacing, and an in-page
// browser fetch — so it must surface as the typed errAACaptcha
// immediately instead of riding the backoff-retry path.
// [LIVE-VERIFIED 2026-09-17] for "Too many requests".
func aaIsAARateLimited(body []byte) bool {
	for _, msg := range aaGraphQLErrorMessages(body) {
		if strings.HasPrefix(msg, "Too many requests") {
			return true
		}
	}
	return false
}

// aaHasAACryptoError reports the server-side crypto error signals:
// AA_CRYPTO_MISSING / _LANE / _BUILD / _MISMATCH / _EXPIRED / _STALE
// (live kT error set; ani-cli #1823 traces).
func aaHasAACryptoError(body []byte) bool {
	var parsed struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, e := range parsed.Errors {
		if strings.HasPrefix(e.Message, "AA_CRYPTO") ||
			strings.HasPrefix(e.Extensions.Code, "AA_CRYPTO") {
			return true
		}
	}
	return false
}

// aaPrioritizeSources stable-sorts sources by the ani-cli provider
// priority; unknown providers keep their response order after the
// preferred block.
func aaPrioritizeSources(sources []aaSource) []aaSource {
	rank := func(name string) int {
		for i, want := range aaPreferredProviders {
			if name == want {
				return i
			}
		}
		return len(aaPreferredProviders)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		return rank(sources[i].Name) < rank(sources[j].Name)
	})
	return sources
}

// aaVariant is one resolved master-playlist entry.
type aaVariant struct {
	uri    string
	height string
}

// aaParseMasterPlaylist extracts variant entries from an m3u8 body the
// way the Python m3u8 lib did (allanime.py:239-252): #EXT-X-STREAM-INF
// lines carry RESOLUTION=WxH, the following non-comment line is the
// (possibly relative) URI resolved against the playlist URL; a missing
// RESOLUTION guesses 1080 (Python's height fallback).
func aaParseMasterPlaylist(body, playlistURL string) (variants []aaVariant, isVariant bool) {
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		return nil, false
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, true
	}

	lines := strings.Split(body, "\n")
	pendingHeight := ""
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pendingHeight = "1080"
			if m := aaResolutionRe.FindStringSubmatch(line); m != nil {
				pendingHeight = m[2]
			}
		case line == "" || strings.HasPrefix(line, "#"):
			// attributes may continue on the same line only; skip
		default:
			if pendingHeight == "" {
				continue
			}
			ref, err := url.Parse(line)
			if err != nil {
				continue
			}
			variants = append(variants, aaVariant{
				uri:    base.ResolveReference(ref).String(),
				height: pendingHeight,
			})
			pendingHeight = ""
		}
	}
	return variants, true
}

// aaEpisodeNumSet renders the availableEpisodesDetail entries as
// strings (Python str(num); JSON strings pass through, numbers format
// without trailing zeros).
func aaEpisodeNumSet(entries []any) map[string]struct{} {
	set := make(map[string]struct{}, len(entries))
	for _, v := range entries {
		switch n := v.(type) {
		case string:
			set[n] = struct{}{}
		case float64:
			set[strconv.FormatFloat(n, 'f', -1, 64)] = struct{}{}
		}
	}
	return set
}

// aaParseNum ports Python's sort key: float(x) with ValueError -> 0.0.
func aaParseNum(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// pyTruthy ports Python truthiness for decoded JSON scalars: nil and
// false are falsy, everything else follows ("false" the string is
// truthy — bug-compatible).
func pyTruthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case string:
		return n != ""
	case float64:
		return n != 0
	default:
		return true
	}
}
