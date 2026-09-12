package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// GraphQL documents ported verbatim from anicli-py
// anicli/providers/allanime.py:16-63.
const (
	aaSearchQuery = `
    query( $search: SearchInput
           $limit: Int
           $page: Int
           $translationType: VaildTranslationTypeEnumType
           $countryOrigin: VaildCountryOriginEnumType )
    {
        shows( search: $search
                limit: $limit
                page: $page
                translationType: $translationType
                countryOrigin: $countryOrigin )
        {
            edges
            {
                _id,
                name,
                thumbnail,
                availableEpisodes
            }
        }
    }
`

	aaEpisodesQuery = `
    query ($showId: String!) {
        show(
            _id: $showId
        ) {
            _id,
            availableEpisodesDetail
        }
    }
`

	aaStreamQuery = `
    query ($showId: String!, $translationType: VaildTranslationTypeEnumType!, $episodeString: String!) {
        episode(
            showId: $showId
            translationType: $translationType
            episodeString: $episodeString
        ) {
            episodeString,
            sourceUrls
        }
    }
`
)

// aaEpisodeQueryHash is the persisted-query hash for the episode-sources
// document, captured from a live ani-cli request trace (ani-cli issue
// #1823, 2026-07). [UNVERIFIED-live]: if the server has re-registered
// the document, the PersistedQueryNotFound fallback re-sends the full
// query with the aaReq extension kept.
const aaEpisodeQueryHash = "f4662f4b7510b26795dd53ef824a0bf1740fbbc5d1273fab18222ac831bca8d0"

// aaPreferredProviders is the ani-cli provider priority (dispatch
// ruling): sources are processed in this order first, everything else
// keeps response order. The Python original iterated sourceUrls raw
// (allanime.py:190) — priority is a mandated improvement.
var aaPreferredProviders = []string{"Default", "S-mp4", "Luf-Mp4", "Yt-mp4"}

// aaResolutionRe extracts the height from a RESOLUTION=WxH attribute.
var aaResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)[xX](\d+)`)

// AllAnime is the port of anicli-py anicli/providers/allanime.py with
// the new-protocol transport: GraphQL GETs against api.mkissa.net with
// the mkissa.to Referer/Origin, a per-epoch AES key (mask XOR partB),
// an aaReq token on episode-source queries and AES-256-GCM tobeparsed
// decryption (see allanime_key.go).
type AllAnime struct {
	Base

	// apiBase is the GraphQL endpoint root.
	apiBase string
	// internalBase absolutizes decoded "--" URLs.
	internalBase string
	// referer is the Referer/Origin value sent on every request.
	referer string
	// keys derives and caches the per-epoch AES key material.
	keys *allAnimeKeyManager
}

// newAllAnime builds the provider against the API base, the referer
// page origin and the internal-URL base. The bases are injectable so
// tests run the full protocol against fake servers (Python hardcoded
// them; production values are the package constants).
func newAllAnime(apiBase, referer, internalBase string, http *netclient.Client) *AllAnime {
	p := &AllAnime{
		Base: Base{
			id:         "allanime",
			name:       "AllAnime",
			baseURL:    referer,
			sourceType: contracts.SourceTypeVideo,
			headers: map[string]string{
				"Referer": referer,
				"Origin":  referer,
			},
			http: http,
		},
		apiBase:      apiBase,
		internalBase: internalBase,
		referer:      referer,
	}
	p.keys = newAllAnimeKeyManager(func(ctx context.Context) (*aaKeys, error) {
		return aaFetchKeys(ctx, p.referer, p.fetchText)
	}, time.Now)
	return p
}

// fetchText GETs url with the provider headers and returns the body
// (the derivation transport; Python had none of this — new protocol).
func (p *AllAnime) fetchText(ctx context.Context, url string) (string, error) {
	resp, err := p.http.Get(ctx, url, p.headers)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// Search runs the shows(search:) query and sorts by difflib similarity
// (port of allanime.py:80-117). Transport and decode failures return an
// empty set (the Python except boundary).
func (p *AllAnime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	variables := map[string]any{
		"search":        map[string]any{"query": query, "countryOrigin": "ALL"},
		"limit":         26,
		"page":          1,
		"countryOrigin": "ALL",
	}
	body, err := p.graphqlGet(ctx, map[string]string{
		"variables": mustJSON(variables),
		"query":     aaSearchQuery,
	})
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
	body, err := p.graphqlGet(ctx, map[string]string{
		"variables": mustJSON(map[string]any{"showId": animeURL}),
		"query":     aaEpisodesQuery,
	})
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

// ResolveStream runs the episode(showId:) query with the aaReq token,
// decrypts tobeparsed, decodes the "--" source URLs and expands them
// through the clock.json flow (port of allanime.py:168-271 with the
// new-protocol transport). Per-source failures skip the source (the
// Python inner try/except); key-derivation and decrypt failures are
// loud typed errors (repo no-silent-failure policy).
func (p *AllAnime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	sources, err := p.fetchEpisodeSources(ctx, episode, dubID)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: %w", contracts.ErrExtractFailed, err))
	}

	for _, src := range sources {
		raw, ok := decodeAllAnimeSourceURL(src.URL, p.internalBase)
		if !ok {
			continue
		}
		// Direct media (allanime.py:211-213).
		if strings.Contains(raw, ".mp4") || strings.Contains(raw, ".m3u8") {
			stream.Links["1080"] = contracts.VideoSource{URL: raw, Quality: "1080"}
			continue
		}
		p.expandClockLinks(ctx, &stream, raw)
	}
	return stream, nil
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
			m3u8Headers := map[string]string{"Referer": clockURL}
			playlist, err := p.fetchText(ctx, src)
			if err != nil {
				continue
			}
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

// fetchEpisodeSources runs the episode GraphQL query with protocol
// retries: PersistedQueryNotFound falls back to the full query;
// AA_CRYPTO_MISSING and tobeparsed GCM failures force one key refresh
// (fresh token on the next attempt). The loop is bounded by the
// once-flags — at most one extra round per condition.
func (p *AllAnime) fetchEpisodeSources(ctx context.Context, episode contracts.Episode, dubID string) ([]aaSource, error) {
	variables := mustJSON(map[string]any{
		"showId":          episode.RawID,
		"translationType": dubID,
		"episodeString":   episode.Num,
	})

	keys, err := p.keys.get(ctx)
	if err != nil {
		return nil, err
	}

	var (
		usedFullQuery bool
		refreshed     bool
	)
	for range 4 {
		token, err := aaBuildAAReqAt(aaEpisodeQueryHash, keys.key, keys.epoch, time.Now().UnixMilli())
		if err != nil {
			return nil, err
		}

		ext := map[string]any{"aaReq": token}
		if !usedFullQuery {
			ext["persistedQuery"] = map[string]any{
				"version":    1,
				"sha256Hash": aaEpisodeQueryHash,
			}
		}
		params := map[string]string{
			"variables":  variables,
			"extensions": mustJSON(ext),
		}
		if usedFullQuery {
			params["query"] = aaStreamQuery
		}

		body, err := p.graphqlGet(ctx, params)
		if err != nil {
			// Transport failure: the Python resolve boundary swallowed
			// it into an empty stream.
			return nil, nil
		}

		if aaHasErrorMessage(body, "PersistedQueryNotFound") && !usedFullQuery {
			usedFullQuery = true
			continue
		}
		if aaHasAACryptoError(body) && !refreshed {
			refreshed = true
			if keys, err = p.keys.refresh(ctx); err != nil {
				return nil, err
			}
			continue
		}

		if blob := aaExtractToBeParsedBlob(body); blob != "" {
			sources, derr := decodeToBeParsed(blob, keys.key)
			if errors.Is(derr, errAAGCMAuth) && !refreshed {
				refreshed = true
				if keys, err = p.keys.refresh(ctx); err != nil {
					return nil, err
				}
				sources, derr = decodeToBeParsed(blob, keys.key)
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
		var parsed struct {
			Data struct {
				Episode struct {
					SourceUrls []struct {
						SourceURL  string `json:"sourceUrl"`
						SourceName string `json:"sourceName"`
					} `json:"sourceUrls"`
				} `json:"episode"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			sources := make([]aaSource, 0, len(parsed.Data.Episode.SourceUrls))
			for _, su := range parsed.Data.Episode.SourceUrls {
				sources = append(sources, aaSource{
					Name: su.SourceName,
					URL:  strings.TrimPrefix(su.SourceURL, "--"),
				})
			}
			return aaPrioritizeSources(sources), nil
		}
		return nil, nil
	}
	return nil, nil
}

// graphqlGet issues the GraphQL GET with the provider headers.
func (p *AllAnime) graphqlGet(ctx context.Context, params map[string]string) ([]byte, error) {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	resp, err := p.http.Get(ctx, p.apiBase+"?"+q.Encode(), p.headers)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// aaHasErrorMessage reports whether the GraphQL error array carries a
// message containing needle.
func aaHasErrorMessage(body []byte, needle string) bool {
	var parsed struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, e := range parsed.Errors {
		if strings.Contains(e.Message, needle) {
			return true
		}
	}
	return false
}

// aaHasAACryptoError reports the server-side stale-key signals
// (AA_CRYPTO_MISSING, AA_CRYPTO_MISSING_BUILD — ani-cli #1823 traces).
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

// mustJSON marshals v or panics — used on internally-built payloads
// that cannot fail to encode.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("allanime: marshal internal payload: %v", err))
	}
	return string(b)
}
