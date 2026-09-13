package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/an0nx/anicli-go/internal/storage"
)

// mustJSON marshals v; empty string on failure (cache payloads are
// rebuilt on the next refresh).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Anime-details enrichment (python _load_anime_details_map +
// _apply_detail_fields_to_card): cards are backfilled from the
// shikimori_anime_details_cache, with stale/missing entries refreshed
// in small batches.

// detailsCacheTTL mirrors _HOME_ANIME_DETAILS_CACHE_TTL_SECONDS.
const detailsCacheTTL = 21600 * time.Second

// detailsMutableRefresh mirrors _ANIME_DETAILS_MUTABLE_REFRESH_SECONDS.
const detailsMutableRefresh = 21600 * time.Second

// detailsRefreshBatchLimit mirrors _ANIME_DETAILS_REFRESH_BATCH_LIMIT.
const detailsRefreshBatchLimit = 8

// loadAnimeDetailsMap loads detail payloads for the visible ids
// (python _load_anime_details_map): db cache first, then a bounded
// refresh of missing/stale rows via GetAnimesInfo.
func (a *App) loadAnimeDetailsMap(r *http.Request, client ShikiClient, animeIDs []int64) map[int64]map[string]any {
	detailsMap := map[int64]map[string]any{}
	if client == nil || len(animeIDs) == 0 {
		return detailsMap
	}

	ids := sortedUniqueIDs(animeIDs)

	var missing []int64
	for _, id := range ids {
		cached, err := a.store.Details.Get(r.Context(), id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		payload, ok := decodeDetailPayload(cached.PayloadJSON)
		if !ok {
			missing = append(missing, id)
			continue
		}
		detailsMap[id] = payload
		if isMutableStale(cached.MutableUpdatedAt, a.now()) {
			missing = append(missing, id)
		}
	}

	if len(missing) > detailsRefreshBatchLimit {
		missing = missing[:detailsRefreshBatchLimit]
	}
	if len(missing) == 0 {
		return detailsMap
	}

	animes, err := client.GetAnimesInfo(r.Context(), missing)
	if err != nil {
		return detailsMap
	}
	now := a.now().UTC()
	for i := range animes {
		row := animeRowMap(&animes[i])
		detailsMap[animes[i].ID] = row

		// The Go repo keeps immutable_cached_at on conflict (first
		// cache fill wins), so a plain upsert reproduces the python
		// update_immutable semantics.
		if err := a.store.Details.Upsert(r.Context(), &storage.AnimeDetails{
			AnimeID:           animes[i].ID,
			PayloadJSON:       encodeDetailPayload(row),
			ImmutableCachedAt: now,
			MutableUpdatedAt:  now,
			UpdatedAt:         now,
		}); err != nil {
			continue
		}
		a.cache.Set("home:anime:details:v1:"+int64ToString(animes[i].ID), row, detailsCacheTTL)
	}
	return detailsMap
}

// applyDetailFieldsToCard backfills empty card fields from the details
// map (python _apply_detail_fields_to_card).
func applyDetailFieldsToCard(card map[string]any, detailsMap map[int64]map[string]any, baseURL string) map[string]any {
	id, ok := coerceID(card["shikimori_id"])
	if !ok {
		return card
	}
	details, ok := detailsMap[id]
	if !ok {
		return card
	}

	updated := cloneCard(card)
	setIfAbsent(updated, "title_en", nonEmptyOrNil(stringOrEmpty(details["name"])))
	setIfAbsent(updated, "title_ru", nonEmptyOrNil(stringOrEmpty(details["russian"])))
	if poster := animePoster(details, nil, baseURL); poster != "" && poster != placeholderPoster {
		setIfAbsent(updated, "poster_url", poster)
	}
	if score, ok := coerceFloat(details["score"]); ok && score > 0 {
		setIfAbsent(updated, "score", score)
	}
	setIfAbsent(updated, "description", nonEmptyOrNil(stringOrEmpty(details["description"])))
	if genres := genreNames(details["genres"]); len(genres) > 0 {
		setIfAbsent(updated, "genres", genres)
	}
	setIfAbsent(updated, "kind", nonEmptyOrNil(stringOrEmpty(details["kind"])))
	setIfAbsent(updated, "status", nonEmptyOrNil(stringOrEmpty(details["status"])))
	setIfAbsent(updated, "year", animeYear(details))
	setIfAbsent(updated, "episodes", intOrNil(details["episodes"]))
	setIfAbsent(updated, "episodes_aired", intOrNil(details["episodes_aired"]))
	setIfAbsent(updated, "next_episode", intOrNil(details["next_episode"]))
	if _, exists := updated["next_episode_at"]; !exists || updated["next_episode_at"] == nil {
		if t, ok := parseShikiDatetime(stringOrEmpty(details["next_episode_at"])); ok {
			updated["next_episode_at"] = t.UTC().Format(time.RFC3339)
		}
	}
	return updated
}

// setIfAbsent assigns value when the key is missing, nil or empty.
func setIfAbsent(card map[string]any, key string, value any) {
	if value == nil {
		return
	}
	if s, ok := value.(string); ok && s == "" {
		return
	}
	if existing, exists := card[key]; !exists || existing == nil || existing == "" {
		card[key] = value
	}
}

// cloneCard shallow-copies a card before mutation.
func cloneCard(card map[string]any) map[string]any {
	out := make(map[string]any, len(card))
	for k, v := range card {
		out[k] = v
	}
	return out
}

// isMutableStale reports whether the mutable freshness stamp crossed
// the refresh threshold (python _is_mutable_details_stale).
func isMutableStale(mutableUpdatedAt, now time.Time) bool {
	if mutableUpdatedAt.IsZero() {
		return true
	}
	return now.Sub(mutableUpdatedAt) >= detailsMutableRefresh
}

// encodeDetailPayload renders a details row as its JSON payload.
func encodeDetailPayload(row map[string]any) string {
	return mustJSON(row)
}

// decodeDetailPayload parses a stored payload; false when unreadable.
func decodeDetailPayload(payload string) (map[string]any, bool) {
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}
