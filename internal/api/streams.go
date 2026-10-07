package api

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// Stream-orchestrator endpoints (KEEP rulings: normalized variants, no
// media proxying) plus the providers listing and the shikimori anime
// page.

// episodesTTL caches episode listings (in-process default 60s).
const episodesTTL = 60 * time.Second

// shikimoriPageTTL caches the shikimori anime page payload.
const shikimoriPageTTL = 60 * time.Second

// fetchEpisodeItems loads and serializes a provider's episode list.
func (a *App) fetchEpisodeItems(r *http.Request, sourceID, sourceURL string) ([]map[string]any, *apiError) {
	provider, ok := a.registry.Get(sourceID)
	if !ok {
		// Python returned an empty list for unknown providers.
		return []map[string]any{}, nil
	}
	episodes, err := provider.GetEpisodes(r.Context(), sourceURL)
	if err != nil {
		return nil, mapProviderError(err)
	}
	items := make([]map[string]any, 0, len(episodes))
	for _, ep := range episodes {
		items = append(items, serializeEpisode(ep, provider.SourceType()))
	}
	return items, nil
}

// mapProviderError routes provider sentinels onto the unified API
// error contract via _ERROR_STATUS_MAP semantics.
func mapProviderError(err error) *apiError {
	switch {
	case errors.Is(err, contracts.ErrGeoBlocked):
		return &apiError{Code: "geo_blocked", Message: "Provider refused the region"}
	case errors.Is(err, contracts.ErrProvider403):
		return &apiError{Code: "provider_403", Message: "Provider returned forbidden"}
	case errors.Is(err, contracts.ErrProviderTimeout):
		return &apiError{Code: "provider_timeout", Message: "Provider timeout"}
	case errors.Is(err, contracts.ErrExtractFailed):
		return &apiError{Code: "extract_failed", Message: "Stream extraction failed"}
	case errors.Is(err, contracts.ErrAllCandidatesFailed):
		return &apiError{Code: "all_candidates_failed", Message: "All candidates failed"}
	default:
		return errInternal("Provider request failed")
	}
}

// handleProviders lists registered providers (python GET
// /api/v1/providers, cached).
func (a *App) handleProviders(w http.ResponseWriter, r *http.Request) {
	const cacheToken = "providers:list"
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	items := make([]map[string]any, 0)
	for _, p := range a.registry.List() {
		items = append(items, map[string]any{
			"source_id":   p.ID(),
			"name":        p.Name(),
			"base_url":    p.BaseURL(),
			"source_type": string(p.SourceType()),
		})
	}
	payload := map[string]any{"items": items}
	a.cache.Set(cacheToken, payload, historyTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// handleEpisodes lists episodes of an arbitrary source (python GET
// /api/v1/episodes).
func (a *App) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	sourceID := r.URL.Query().Get("source_id")
	sourceURL := r.URL.Query().Get("source_url")
	if sourceID == "" || sourceURL == "" {
		writeAPIError(w, r, fieldError("source_id", "source_id and source_url are required"))
		return
	}

	cacheToken := "episodes:" + sourceID + ":" + cacheHash(sourceURL)
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	items, e := a.fetchEpisodeItems(r, sourceID, sourceURL)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	payload := map[string]any{"items": items}
	a.cache.Set(cacheToken, payload, episodesTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// streamResolveRequest is the multi-track resolve body (python
// StreamResolveRequest).
type streamResolveRequest struct {
	SourceID     string   `json:"source_id"`
	EpisodeNum   string   `json:"episode_num"`
	EpisodeRawID string   `json:"episode_raw_id"`
	VideoKey     string   `json:"video_key"`
	AudioKey     *string  `json:"audio_key"`
	URLsVideo    []string `json:"urls_video"`
	URLsAudio    []string `json:"urls_audio"`
}

// handleStreamsResolve resolves playable stream links for the selected
// tracks (python POST /api/v1/streams/resolve): server-side provider +
// extractor resolution, NO media proxying.
func (a *App) handleStreamsResolve(w http.ResponseWriter, r *http.Request) {
	var req streamResolveRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}

	urlsVideo := nonEmpty(req.URLsVideo)
	urlsAudio := nonEmpty(req.URLsAudio)
	if len(urlsVideo) == 0 {
		writeAPIError(w, r, errValidation("video URLs are required", map[string]any{"field": "urls_video"}))
		return
	}

	rawEmbeds := map[string][]string{req.VideoKey: urlsVideo}
	hasAudio := req.AudioKey != nil && *req.AudioKey != "" && len(urlsAudio) > 0
	if hasAudio {
		rawEmbeds[*req.AudioKey] = urlsAudio
	}
	episode := contracts.Episode{
		Num:       req.EpisodeNum,
		RawID:     req.SourceID + ":" + req.EpisodeRawID,
		RawEmbeds: rawEmbeds,
	}

	provider, ok := a.registry.Get(req.SourceID)
	if !ok {
		writeAPIError(w, r, &apiError{
			Code:    "all_candidates_failed",
			Message: "Unable to resolve video streams",
			Details: map[string]any{"track": "video", "source_id": req.SourceID},
		})
		return
	}

	// The python decomposition (cli/stream_resolver.py
	// extract_best_source): the provider consumes its OWN bare raw id,
	// never the composed "source:rawid" form — the prefixed bytes'
	// first byte is what crashed the animevib json.decode live (#157).
	// The same function's dub half (#158 fix-round 2): the provider
	// consumes its own bare dub name, never the "[prov] dub" track
	// tag.
	videoEpisode := episode
	videoEpisode.RawID = episode.ProviderRawID(req.SourceID)
	videoStream, err := provider.ResolveStream(r.Context(), videoEpisode, dubNameFromTrackKey(req.VideoKey))
	if err != nil || len(videoStream.Links) == 0 {
		writeAPIError(w, r, &apiError{
			Code:    "all_candidates_failed",
			Message: "Unable to resolve video streams",
			Details: map[string]any{"track": "video", "source_id": req.SourceID},
		})
		return
	}

	audioStreams := []map[string]any{}
	muxMode := "single_av"
	if hasAudio {
		if *req.AudioKey != req.VideoKey {
			muxMode = "dual_url"
		}
		audioProviderID := providerIDFromTrackKey(*req.AudioKey, req.SourceID)
		audioProvider := provider
		if audioProviderID != req.SourceID {
			if ap, ok := a.registry.Get(audioProviderID); ok {
				audioProvider = ap
			}
		}
		audioEpisode := episode
		if audioProviderID == req.SourceID {
			audioEpisode.RawID = episode.ProviderRawID(req.SourceID)
		} else {
			// A genuinely foreign audio provider has no raw id in this
			// single-source request: python's decomposition loop-miss
			// yields "" and the provider resolves from the embed link
			// or fails typed — the video source's state must never
			// leak to it (#157 review).
			audioEpisode.RawID = ""
		}
		// The audio key is a "[prov] dub" track key by the same
		// contract — the bare dub name rides the same strip (#158
		// fix-round 2).
		audioStream, err := audioProvider.ResolveStream(r.Context(), audioEpisode, dubNameFromTrackKey(*req.AudioKey))
		if err != nil || len(audioStream.Links) == 0 {
			writeAPIError(w, r, &apiError{
				Code:    "all_candidates_failed",
				Message: "Unable to resolve audio streams",
				Details: map[string]any{"track": "audio", "source_id": req.SourceID},
			})
			return
		}
		sortedAudio := sortedQualities(audioStream.Links)
		for i, q := range sortedAudio {
			src := audioStream.Links[q]
			audioStreams = append(audioStreams, serializeAudioStream(q, src, *req.AudioKey, i == 0))
		}
	}

	sortedVideo := sortedQualities(videoStream.Links)
	videoStreams := make([]map[string]any, 0, len(sortedVideo))
	recommendedOrder := make([]int, 0, len(sortedVideo))
	for _, q := range sortedVideo {
		src := videoStream.Links[q]
		videoStreams = append(videoStreams, serializeVideoStream(q, src))
		if n, err := strconv.Atoi(q); err == nil {
			recommendedOrder = append(recommendedOrder, n)
		}
	}

	expiresValues := []any{}
	for _, item := range append(append([]map[string]any{}, videoStreams...), audioStreams...) {
		if v, ok := item["expires_at"]; ok && v != nil {
			expiresValues = append(expiresValues, v)
		}
	}
	sourceMeta := map[string]any{
		"provider":          req.SourceID,
		"expires_at":        minISO(expiresValues),
		"tokenized":         len(expiresValues) > 0,
		"requires_headers":  anyStreamHasHeaders(videoStreams, audioStreams),
		"recommended_order": recommendedOrder,
	}

	writeJSON(w, 200, map[string]any{
		"source_id":      req.SourceID,
		"episode_num":    req.EpisodeNum,
		"episode_raw_id": req.EpisodeRawID,
		"video_key":      req.VideoKey,
		"audio_key":      req.AudioKey,
		"video_streams":  videoStreams,
		"audio_streams":  audioStreams,
		"mux_mode":       muxMode,
		"captions":       []any{},
		"chapters":       []any{},
		"source_meta":    sourceMeta,
	})
}

// serializeVideoStream renders one video variant (python
// _serialize_video_stream); quality is numeric on the wire.
func serializeVideoStream(quality string, src contracts.VideoSource) map[string]any {
	kind := streamType(src.URL, src.Type)
	expiresAt, ttl := ttlHints(src.URL)
	return map[string]any{
		"quality":     qualityInt(quality),
		"url":         src.URL,
		"type":        kind,
		"headers":     headersOrNil(src.Headers),
		"codec":       nil,
		"bandwidth":   nil,
		"is_hls":      kind == "hls",
		"expires_at":  expiresAt,
		"ttl_seconds": ttl,
	}
}

// serializeAudioStream renders one audio variant (python
// _serialize_audio_stream).
func serializeAudioStream(quality string, src contracts.VideoSource, name string, isDefault bool) map[string]any {
	kind := streamType(src.URL, src.Type)
	expiresAt, ttl := ttlHints(src.URL)
	return map[string]any{
		"lang":        nil,
		"name":        name,
		"quality":     qualityInt(quality),
		"url":         src.URL,
		"type":        kind,
		"headers":     headersOrNil(src.Headers),
		"codec":       nil,
		"default":     isDefault,
		"expires_at":  expiresAt,
		"ttl_seconds": ttl,
	}
}

// sortedQualities orders link keys LEXICOGRAPHICALLY descending,
// exactly reproducing python api_server.py streams/resolve:
//
//	sorted(items, key=lambda item: item[0], reverse=True)
//
// The keys are plain strings, so {"1080","720","480"} emits
// ["720","480","1080"] — string order, NOT numeric order.
func sortedQualities(links map[string]contracts.VideoSource) []string {
	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys
}

// qualityInt coerces a quality label to int (python int(quality)).
func qualityInt(quality string) int {
	v, err := strconv.Atoi(quality)
	if err != nil {
		return 0
	}
	return v
}

// handleShikimoriAnimePage serves the cached shikimori details aggregate
// with normalized artwork (python GET /api/v1/shikimori/anime/{id}/page;
// the referenced python client method is absent from the frozen tree —
// the Go port uses the GetAnimeDetails aggregate).
func (a *App) handleShikimoriAnimePage(w http.ResponseWriter, r *http.Request) {
	if a.shiki == nil {
		writeAPIError(w, r, errNotFound("Shikimori client unavailable"))
		return
	}
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}

	cacheToken := "shikimori:anime_page:" + strconv.FormatInt(id, 10)
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	details, err := a.shiki.GetAnimeDetails(r.Context(), id)
	if err != nil {
		if errors.Is(err, shikimori.ErrDisabled) {
			writeAPIError(w, r, errNotFound("Shikimori client unavailable"))
			return
		}
		writeAPIError(w, r, errNotFound("Anime page unavailable"))
		return
	}

	payload := map[string]any{
		"anime":      details.Anime,
		"characters": details.Characters,
		"staff":      details.Staff,
		"related":    details.Related,
		"similar":    details.Similar,
	}
	payload["artwork"] = buildArtwork(details.Anime.PosterURL(shikimori.DefaultBaseURL), "shikimori:"+strconv.FormatInt(id, 10))

	a.cache.Set(cacheToken, payload, shikimoriPageTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// ttlHints best-effort parses tokenized-URL expiry query params
// (python _ttl_hints): expires/exp/e/expire unix timestamps become an
// ISO expires_at plus remaining ttl_seconds.
func ttlHints(rawURL string) (any, any) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil
	}
	now := ttlClock()
	for _, key := range []string{"expires", "exp", "e", "expire"} {
		value := u.Query().Get(key)
		if value == "" || !isAllDigits(value) {
			continue
		}
		ts, err := strconv.ParseInt(value, 10, 64)
		if err != nil || ts <= 0 {
			continue
		}
		expiresAt := time.Unix(ts, 0).UTC().Format(time.RFC3339)
		ttl := ts - now
		if ttl < 0 {
			ttl = 0
		}
		return expiresAt, int(ttl)
	}
	return nil, nil
}

// ttlClock is swappable for deterministic ttl_hint tests.
var ttlClock = func() int64 { return time.Now().Unix() }

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// nonEmpty strips blank strings (python list comprehension filters).
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// headersOrNil maps empty header sets to nil (python dict | None wire
// shape).
func headersOrNil(h map[string]string) any {
	if len(h) == 0 {
		return nil
	}
	return h
}

// minISO picks the earliest ISO timestamp of the collected expiry
// values (python min(expires_values)); nil when empty.
func minISO(values []any) any {
	var best string
	for _, v := range values {
		s, _ := v.(string)
		if s == "" {
			continue
		}
		if best == "" || s < best {
			best = s
		}
	}
	if best == "" {
		return nil
	}
	return best
}

// anyStreamHasHeaders reports whether any variant carries required
// headers (python requires_headers).
func anyStreamHasHeaders(streams ...[]map[string]any) bool {
	for _, group := range streams {
		for _, s := range group {
			if h, ok := s["headers"].(map[string]string); ok && len(h) > 0 {
				return true
			}
		}
	}
	return false
}
