package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/an0nx/anicli-go/internal/storage"
)

// History routes: list, one, patch, episodes, progress GET/PATCH —
// ported from api_server.py history endpoints.

// historyTTL is the episodes-list cache lifetime (python used the redis
// episodes ttl; the Go in-process default is 60s).
const historyTTL = 60 * time.Second

// queryInt parses a bounded integer query parameter; a missing
// parameter yields def; invalid values or out-of-range values become
// the 422 contract (python Query(ge=..., le=...) validation).
func queryInt(r *http.Request, name string, def, min, max int) (int, *apiError) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fieldError(name, "must be an integer between "+strconv.Itoa(min)+" and "+strconv.Itoa(max))
	}
	return v, nil
}

// handleHistoryList renders every history record (python GET
// /api/v1/history).
func (a *App) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.Progress.ListHistory(r.Context(), "", 0, 0)
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to load history"))
		return
	}
	items := make([]map[string]any, 0, len(records))
	for i := range records {
		sources, err := a.store.Sources.ListByAnime(r.Context(), records[i].ID)
		if err != nil {
			writeAPIError(w, r, errInternal("Failed to load history sources"))
			return
		}
		items = append(items, serializeHistoryRecord(&records[i], sources))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// historyRecordWithSources loads one record plus its sources.
func (a *App) historyRecordWithSources(r *http.Request, id int64) (map[string]any, *apiError) {
	rec, err := a.store.Progress.GetByID(r.Context(), id)
	if err != nil {
		return nil, errNotFound("History item not found")
	}
	sources, err := a.store.Sources.ListByAnime(r.Context(), id)
	if err != nil {
		return nil, errInternal("Failed to load history sources")
	}
	return serializeHistoryRecord(rec, sources), nil
}

// pathAnimeID extracts {anime_id} as a positive int64 (invalid paths
// get the 422 contract, matching FastAPI int path coercion).
func pathAnimeID(r *http.Request) (int64, *apiError) {
	raw := chi.URLParam(r, "anime_id")
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, fieldError("anime_id", "must be a positive integer")
	}
	return v, nil
}

// handleHistoryOne renders one record (python GET /history/{anime_id}).
func (a *App) handleHistoryOne(w http.ResponseWriter, r *http.Request) {
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	payload, e := a.historyRecordWithSources(r, id)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	writeJSON(w, 200, payload)
}

// historyPatchRequest is the local status update body (python
// HistoryPatchRequest).
type historyPatchRequest struct {
	Status    *string `json:"status"`
	Score     *int    `json:"score"`
	Rewatches *int    `json:"rewatches"`
	Episodes  *string `json:"episodes"`
}

// handleHistoryPatch updates the local status subset (python PATCH
// /history/{anime_id}).
func (a *App) handleHistoryPatch(w http.ResponseWriter, r *http.Request) {
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	var req historyPatchRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}
	if req.Score != nil && (*req.Score < 0 || *req.Score > 10) {
		writeAPIError(w, r, fieldError("score", "must be between 0 and 10"))
		return
	}
	if req.Rewatches != nil && *req.Rewatches < 0 {
		writeAPIError(w, r, fieldError("rewatches", "must be >= 0"))
		return
	}

	if err := a.store.Progress.UpdateLocalStatus(r.Context(), id, req.Status, req.Score, req.Rewatches, req.Episodes); err != nil {
		writeAPIError(w, r, errInternal("Failed to update history"))
		return
	}
	payload, e := a.historyRecordWithSources(r, id)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	writeJSON(w, 200, payload)
}

// handleHistoryEpisodes lists episode track keys of the bound source
// (python GET /history/{anime_id}/episodes).
func (a *App) handleHistoryEpisodes(w http.ResponseWriter, r *http.Request) {
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	if _, err := a.store.Progress.GetByID(r.Context(), id); err != nil {
		writeAPIError(w, r, errNotFound("History item not found"))
		return
	}
	sources, err := a.store.Sources.ListByAnime(r.Context(), id)
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to load history sources"))
		return
	}

	// Source selection: explicit source_id query param, else the first
	// non-shikimori binding (python behavior).
	targetSourceID := r.URL.Query().Get("source_id")
	targetSourceURL := ""
	if targetSourceID != "" {
		for _, src := range sources {
			if src.SourceID == targetSourceID {
				targetSourceURL = src.SourceURL
				break
			}
		}
	} else {
		for _, src := range sources {
			if src.SourceID != "shikimori" {
				targetSourceID, targetSourceURL = src.SourceID, src.SourceURL
				break
			}
		}
	}
	if targetSourceID == "" || targetSourceURL == "" {
		writeAPIError(w, r, &apiError{Code: "source_not_bound", Message: "No source bound for this history item"})
		return
	}

	cacheToken := "history:episodes:" + strconv.FormatInt(id, 10) + ":" + targetSourceID + ":" + cacheHash(targetSourceURL)
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	items, e := a.fetchEpisodeItems(r, targetSourceID, targetSourceURL)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	payload := map[string]any{
		"anime_id":   id,
		"source_id":  targetSourceID,
		"source_url": targetSourceURL,
		"items":      items,
	}
	a.cache.Set(cacheToken, payload, historyTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// progressPatchRequest is the playback progress body (python
// HistoryProgressPatchRequest).
type progressPatchRequest struct {
	Episode     string  `json:"episode"`
	PositionSec *int64  `json:"position_sec"`
	DurationSec *int64  `json:"duration_sec"`
	VideoKey    *string `json:"video_key"`
	AudioKey    *string `json:"audio_key"`
	Quality     *int64  `json:"quality"`
}

// handleHistoryProgressPatch saves per-episode playback position
// (python PATCH /history/{anime_id}/progress).
func (a *App) handleHistoryProgressPatch(w http.ResponseWriter, r *http.Request) {
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	var req progressPatchRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}
	if req.Episode == "" {
		writeAPIError(w, r, fieldError("episode", "is required"))
		return
	}
	if req.PositionSec == nil || *req.PositionSec < 0 {
		writeAPIError(w, r, fieldError("position_sec", "must be >= 0"))
		return
	}
	if req.DurationSec == nil || *req.DurationSec < 0 {
		writeAPIError(w, r, fieldError("duration_sec", "must be >= 0"))
		return
	}
	if req.Quality != nil && *req.Quality < 1 {
		writeAPIError(w, r, fieldError("quality", "must be >= 1"))
		return
	}

	if _, err := a.store.Progress.GetByID(r.Context(), id); err != nil {
		writeAPIError(w, r, errNotFound("History item not found"))
		return
	}

	row := &storage.EpisodeProgress{
		AnimeID:     id,
		Episode:     req.Episode,
		PositionSec: *req.PositionSec,
		DurationSec: *req.DurationSec,
		VideoKey:    req.VideoKey,
		AudioKey:    req.AudioKey,
		Quality:     req.Quality,
		UpdatedAt:   a.now(),
	}
	if err := a.store.Episodes.Upsert(r.Context(), row); err != nil {
		writeAPIError(w, r, errInternal("Failed to save progress"))
		return
	}
	writeJSON(w, 200, serializeEpisodeProgress(row))
}

// handleHistoryProgressGet reads the playback position (python GET
// /history/{anime_id}/progress): the requested episode or the most
// recently updated one.
func (a *App) handleHistoryProgressGet(w http.ResponseWriter, r *http.Request) {
	id, e := pathAnimeID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	if _, err := a.store.Progress.GetByID(r.Context(), id); err != nil {
		writeAPIError(w, r, errNotFound("History item not found"))
		return
	}

	episode := r.URL.Query().Get("episode")
	var row *storage.EpisodeProgress
	var err error
	if episode != "" {
		row, err = a.store.Episodes.GetByEpisode(r.Context(), id, episode)
	} else {
		row, err = a.store.Episodes.LatestByAnime(r.Context(), id)
	}
	if err != nil || row == nil {
		writeJSON(w, 200, map[string]any{"anime_id": id, "progress": nil})
		return
	}
	writeJSON(w, 200, map[string]any{
		"anime_id": id,
		"progress": serializeEpisodeProgress(row),
	})
}

// serializeEpisodeProgress renders one episode progress row (python
// progress payload).
func serializeEpisodeProgress(row *storage.EpisodeProgress) map[string]any {
	updated := ""
	if !row.UpdatedAt.IsZero() {
		updated = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"episode":      row.Episode,
		"position_sec": row.PositionSec,
		"duration_sec": row.DurationSec,
		"video_key":    row.VideoKey,
		"audio_key":    row.AudioKey,
		"quality":      row.Quality,
		"updated_at":   nilIfEmpty(updated),
	}
}
