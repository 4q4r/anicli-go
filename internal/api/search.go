package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Search + releases calendar (KEEP rulings: GET /api/v1/search is
// Shikimori-only autocomplete; the calendar projects user rates with
// upcoming next-episode timestamps).

// searchCacheTTL caches autocomplete results (python
// _SEARCH_CACHE_TTL_SECONDS).
const searchCacheTTL = 3600 * time.Second

// searchAutocompleteLimit caps the result count (python
// _SEARCH_AUTOCOMPLETE_LIMIT).
const searchAutocompleteLimit = 7

// releaseCalendarTTL caches the calendar payload.
const releaseCalendarTTL = 60 * time.Second

// handleSearch runs the Shikimori-only autocomplete (python
// _api_search_shikimori): failures degrade to empty results, never a 5xx.
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	rawQuery := r.URL.Query().Get("q")
	if rawQuery == "" {
		writeAPIError(w, r, fieldError("q", "is required"))
		return
	}
	if _, e := queryInt(r, "timeout", 30, 1, 180); e != nil {
		writeAPIError(w, r, e)
		return
	}
	normalized := strings.TrimSpace(rawQuery)
	if normalized == "" {
		writeJSON(w, 200, map[string]any{"query": rawQuery, "results": []any{}, "cached": false})
		return
	}

	cacheToken := "search:v2:" + cacheHash(strings.ToLower(normalized))
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	var results []shikiAutocompleteItem
	if client := a.requestShiki(r); client != nil {
		items, err := client.Autocomplete(r.Context(), normalized, searchAutocompleteLimit)
		if err == nil {
			results = items
		}
	}
	if results == nil {
		results = []shikiAutocompleteItem{}
	}

	payload := map[string]any{"query": rawQuery, "results": results}
	a.cache.Set(cacheToken, payload, searchCacheTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// handleReleasesCalendar projects the user's list into upcoming release
// events (python fetch_release_calendar_for_client + _serialize_release_event).
func (a *App) handleReleasesCalendar(w http.ResponseWriter, r *http.Request) {
	days, e := queryInt(r, "days", 7, 1, 30)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}

	cacheToken := "release_calendar:" + strconv.Itoa(days)
	if cached := a.cache.Get(cacheToken); cached != nil {
		cached["cached"] = true
		writeJSON(w, 200, cached)
		return
	}

	items := []map[string]any{}
	client := a.requestShiki(r)
	if client != nil && client.Authenticated() {
		if events := a.buildReleaseEvents(r, client, days); len(events) > 0 {
			items = events
		}
	}

	payload := map[string]any{
		"days":  days,
		"items": items,
	}
	a.cache.Set(cacheToken, payload, releaseCalendarTTL)
	payload["cached"] = false
	writeJSON(w, 200, payload)
}

// buildReleaseEvents is the calendar service skeleton (python
// build_release_events): rates of watching/rewatching/planned statuses
// whose anime has a next_episode_at inside the window.
func (a *App) buildReleaseEvents(r *http.Request, client ShikiClient, days int) []map[string]any {
	rates, err := client.GetUserRates(r.Context())
	if err != nil || len(rates) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rates))
	for _, rate := range rates {
		ids = append(ids, rate.TargetID)
	}
	animes, err := client.GetAnimesInfo(r.Context(), sortedUniqueIDs(ids))
	if err != nil {
		animes = nil
	}
	animeByID := map[int64]map[string]any{}
	for i := range animes {
		animeByID[animes[i].ID] = animeRowMap(&animes[i])
	}

	now := a.now().UTC()
	dayFrom := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayTo := dayFrom.AddDate(0, 0, maxInt(days, 1)-1)

	var events []map[string]any
	// python sorts events by release_at ascending before returning
	// (release_calendar_service.py events.sort(key=lambda item:
	// item.release_at)); the parsed timestamp is carried alongside each
	// row so the sort compares instants, not formatted strings.
	type keyedEvent struct {
		at  time.Time
		row map[string]any
	}
	var keyed []keyedEvent
	for _, rate := range rates {
		userStatus := strings.ToLower(strings.TrimSpace(rate.Status))
		switch userStatus {
		case "watching", "rewatching", "planned", "plan_to_watch":
		default:
			continue
		}
		anime, ok := animeByID[rate.TargetID]
		if !ok {
			continue
		}
		releaseAt, ok := parseShikiDatetime(stringOrEmpty(anime["next_episode_at"]))
		if !ok {
			continue
		}
		releaseDate := time.Date(releaseAt.Year(), releaseAt.Month(), releaseAt.Day(), 0, 0, 0, 0, time.UTC)
		if releaseDate.Before(dayFrom) || releaseDate.After(dayTo) {
			continue
		}

		title := stringOrEmpty(anime["russian"])
		if title == "" {
			title = stringOrEmpty(anime["name"])
		}
		if title == "" {
			title = strconv.FormatInt(rate.TargetID, 10)
		}

		nextEpisode := any(nil)
		if v, ok := coerceID(anime["next_episode"]); ok {
			nextEpisode = v
		}
		animeStatus := stringOrEmpty(anime["status"])
		if animeStatus == "" {
			animeStatus = "unknown"
		}

		poster := ""
		if img, ok := anime["image"].(map[string]any); ok {
			poster = posterFromImage(img)
		}

		keyed = append(keyed, keyedEvent{at: releaseAt, row: map[string]any{
			"shikimori_id": rate.TargetID,
			"title":        title,
			"artwork":      buildArtwork(poster, "release:"+strconv.FormatInt(rate.TargetID, 10)+":"+releaseAt.Format(time.RFC3339)),
			"user_status":  userStatus,
			"anime_status": animeStatus,
			"next_episode": nextEpisode,
			"release_at":   releaseAt.UTC().Format(time.RFC3339),
		}})
	}
	sort.SliceStable(keyed, func(i, j int) bool { return keyed[i].at.Before(keyed[j].at) })
	events = make([]map[string]any, 0, len(keyed))
	for _, ev := range keyed {
		events = append(events, ev.row)
	}
	return events
}

// posterFromImage resolves the image size ladder (python _anime_poster
// original > main > preview > x96 > x48 against the base URL).
func posterFromImage(img map[string]any) string {
	for _, key := range []string{"original", "main", "preview", "x96", "x48"} {
		if v := stringOrEmpty(img[key]); v != "" {
			return v
		}
	}
	return ""
}
