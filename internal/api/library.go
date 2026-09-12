package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/storage"
)

// Library routes: the personalized list (python GET /api/v1/library)
// and the bind endpoint (python POST /api/v1/library/bind).

// bindSourceItem is one source binding of the bind request.
type bindSourceItem struct {
	SourceID  string `json:"source_id"`
	SourceURL string `json:"source_url"`
}

// artworkPayload is the normalized artwork body.
type artworkPayload struct {
	Poster string `json:"poster"`
}

// libraryBindRequest is the bind body (python LibraryBindRequest).
type libraryBindRequest struct {
	Title           string           `json:"title"`
	Artwork         *artworkPayload  `json:"artwork"`
	Sources         []bindSourceItem `json:"sources"`
	ShikimoriID     *int64           `json:"shikimori_id"`
	ShikimoriTitle  *string          `json:"shikimori_title"`
	ShikimoriStatus *string          `json:"shikimori_status"`
}

// handleLibraryBind binds sources into one library entry (python
// library_bind): upsert the anime_progress row via the primary source,
// replace the source list, return the refreshed record.
func (a *App) handleLibraryBind(w http.ResponseWriter, r *http.Request) {
	var req libraryBindRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}
	if len(req.Sources) == 0 {
		writeAPIError(w, r, errValidation("Sources must not be empty", map[string]any{"field": "sources"}))
		return
	}
	if req.Title == "" {
		writeAPIError(w, r, fieldError("title", "is required"))
		return
	}

	poster := placeholderPoster
	if req.Artwork != nil && req.Artwork.Poster != "" {
		poster = req.Artwork.Poster
	}

	primary := req.Sources[0]
	row := &storage.AnimeProgress{
		Title:           req.Title,
		Poster:          &poster,
		SourceID:        primary.SourceID,
		SourceURL:       primary.SourceURL,
		CurrentEpisode:  "1",
		ShikimoriTitle:  req.ShikimoriTitle,
		ShikimoriID:     req.ShikimoriID,
		ShikimoriStatus: statusOr(req.ShikimoriStatus, "watching"),
		UpdatedAt:       a.now(),
	}
	if err := a.store.Progress.Upsert(r.Context(), row); err != nil {
		writeAPIError(w, r, errInternal("Failed to bind library item"))
		return
	}

	sources := make([]storage.AnimeSource, 0, len(req.Sources))
	for _, item := range req.Sources {
		sources = append(sources, storage.AnimeSource{
			AnimeProgressID: row.ID,
			SourceID:        item.SourceID,
			SourceURL:       item.SourceURL,
		})
	}
	if err := a.store.Sources.ReplaceForAnime(r.Context(), row.ID, sources); err != nil {
		writeAPIError(w, r, errInternal("Failed to bind library item"))
		return
	}

	payload, e := a.historyRecordWithSources(r, row.ID)
	if e != nil {
		writeAPIError(w, r, errInternal("Failed to bind library item"))
		return
	}
	writeJSON(w, 200, payload)
}

// handleLibraryList renders the personalized library (python GET
// /api/v1/library): shikimori rates first, local DB fallback, paginated
// with offset cursors and enriched with detail fields.
func (a *App) handleLibraryList(w http.ResponseWriter, r *http.Request) {
	limit, e := queryInt(r, "limit", 24, 1, 100)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	statusFilter := r.URL.Query().Get("status")

	historyRows, err := a.store.Progress.ListHistory(r.Context(), "", 0, 0)
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to load history"))
		return
	}
	localByShikiID := map[int64]*storage.AnimeProgress{}
	for i := range historyRows {
		if historyRows[i].ShikimoriID != nil {
			localByShikiID[*historyRows[i].ShikimoriID] = &historyRows[i]
		}
	}
	localViews := map[int64]localRowView{}
	for id, row := range localByShikiID {
		localViews[id] = localViewOfRow(row)
	}

	shikiClient := a.requestShiki(r)
	baseURL := a.shikiBaseURL(shikiClient)
	rates, animeByID := a.loadShikimoriUserSnapshot(r, shikiClient, cacheScopeOf(r), false)

	statuses := parseStatusFilter(statusFilter)
	sourceRates := filterRatesByStatus(rates, statuses)

	itemsSource := buildRealLibraryItems(sourceRates, animeByID, localViews, baseURL)
	if len(itemsSource) == 0 && len(historyRows) > 0 {
		// Local DB fallback when the shikimori snapshot is empty.
		fallback := buildLocalLibraryFallbackItems(historyRows, baseURL)
		if len(statuses) > 0 {
			filtered := make([]map[string]any, 0, len(fallback))
			for _, item := range fallback {
				if statuses[statusString(item["status"])] {
					filtered = append(filtered, item)
				}
			}
			itemsSource = filtered
		} else {
			itemsSource = fallback
		}
	}

	items, nextCursor := paginate(itemsSource, limit, cursor)
	detailsMap := a.loadAnimeDetailsMap(r, shikiClient, visibleIDs(items))
	for i, item := range items {
		items[i] = applyDetailFieldsToCard(item, detailsMap, baseURL)
	}

	writeJSON(w, 200, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
		"total":       len(itemsSource),
		"filters":     map[string]any{"status": sortedStatuses(statuses)},
	})
}

// parseStatusFilter splits the csv status filter and applies the
// planned <-> plan_to_watch aliasing (python library route).
func parseStatusFilter(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		switch p {
		case "plan_to_watch":
			out[p] = true
			out["planned"] = true
		case "planned":
			out[p] = true
			out["plan_to_watch"] = true
		default:
			out[p] = true
		}
	}
	return out
}

// filterRatesByStatus keeps rates whose status matches the filter set
// (empty set keeps everything).
func filterRatesByStatus(rates []shikiRate, statuses map[string]bool) []shikiRate {
	if len(statuses) == 0 {
		return rates
	}
	out := make([]shikiRate, 0, len(rates))
	for _, rate := range rates {
		if statuses[strings.ToLower(strings.TrimSpace(rate.Status))] {
			out = append(out, rate)
		}
	}
	return out
}

// sortedStatuses renders the active filter set sorted (python
// sorted(statuses)).
func sortedStatuses(statuses map[string]bool) []string {
	out := make([]string, 0, len(statuses))
	for s := range statuses {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// visibleIDs extracts the shikimori ids of a card page.
func visibleIDs(items []map[string]any) []int64 {
	out := make([]int64, 0, len(items))
	for _, item := range items {
		if id, ok := coerceID(item["shikimori_id"]); ok {
			out = append(out, id)
		}
	}
	return out
}

// coerceID converts float64/int/int64 JSON-ish ids to int64.
func coerceID(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// statusString renders a status-ish value ("" when nil).
func statusString(v any) string {
	if s, ok := v.(string); ok {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return ""
}

// statusOr falls back to def for a nil status.
func statusOr(s *string, def string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return def
	}
	return *s
}

// libraryCacheScopeTTL anchors the snapshot cache lifetime for the
// rates-only variant (python: 43200s).
const snapshotRatesTTL = 43200 * time.Second
