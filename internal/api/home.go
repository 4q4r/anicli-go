package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/storage"
)

// Home feed (KEEP rulings: personalized continue_watching/new_releases/
// library_recent from shikimori user rates + local DB; hero gallery
// paginated items+next_cursor).

// heroOngoingTTL caches the ongoing hero candidates (python
// _HERO_ONGOING_CACHE_TTL_SECONDS).
const heroOngoingTTL = 3600 * time.Second

// heroBadge / heroLibraryBadge mirror the python constants.
const (
	heroBadge        = "Онгоинг"
	heroLibraryBadge = "Из вашей библиотеки"
)

// homeCardFieldOrder documents the card keys produced by buildHomeCard
// (python _build_home_card).
//
//	shikimori_id, title_ru, title_en, poster_url, score, description,
//	genres, kind, status, year, episodes, episodes_aired, next_episode,
//	next_episode_at, url, bottom_text_left, bottom_text_right

// buildHomeCard assembles one ui-ready card (python _build_home_card).
func buildHomeCard(animeID int64, animeRow map[string]any, localRow *localRowView, baseURL, bottomLeft string, bottomRight any) map[string]any {
	titleRu, titleEn := animeTitles(animeRow, localRow)
	nextEpisodeAt := any(nil)
	if t, ok := parseShikiDatetime(stringOrEmpty(animeRow["next_episode_at"])); ok {
		nextEpisodeAt = t.UTC().Format(time.RFC3339)
	}
	score := any(nil)
	if s, ok := coerceFloat(animeRow["score"]); ok {
		score = s
	}
	return map[string]any{
		"shikimori_id":      animeID,
		"title_ru":          titleRu,
		"title_en":          titleEn,
		"poster_url":        animePoster(animeRow, localRow, baseURL),
		"score":             score,
		"description":       nonEmptyOrNil(stringOrEmpty(animeRow["description"])),
		"genres":            genreNames(animeRow["genres"]),
		"kind":              nonEmptyOrNil(stringOrEmpty(animeRow["kind"])),
		"status":            nonEmptyOrNil(stringOrEmpty(animeRow["status"])),
		"year":              animeYear(animeRow),
		"episodes":          intOrNil(animeRow["episodes"]),
		"episodes_aired":    intOrNil(animeRow["episodes_aired"]),
		"next_episode":      intOrNil(animeRow["next_episode"]),
		"next_episode_at":   nextEpisodeAt,
		"url":               "/anime/" + strconv.FormatInt(animeID, 10),
		"bottom_text_left":  bottomLeft,
		"bottom_text_right": bottomRight,
	}
}

// localRowView is the local anime_progress projection used by card
// builders (title/poster/current_episode/updated_at/history id).
type localRowView struct {
	Title          string
	Poster         string
	CurrentEpisode int
	UpdatedAt      time.Time
	HistoryID      int64
	Has            bool
}

// handleHomeFeed composes the personalized home sections (python
// home_feed).
func (a *App) handleHomeFeed(w http.ResponseWriter, r *http.Request) {
	heroLimit, e := queryInt(r, "hero_limit", 12, 1, 50)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	continueLimit, e := queryInt(r, "continue_limit", 12, 1, 50)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	releasesLimit, e := queryInt(r, "releases_limit", 12, 1, 50)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	recentLimit, e := queryInt(r, "recent_limit", 12, 1, 50)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	releaseDays, e := queryInt(r, "release_days", 14, 1, 30)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	heroCursor := r.URL.Query().Get("hero_cursor")
	continueCursor := r.URL.Query().Get("continue_cursor")
	releasesCursor := r.URL.Query().Get("releases_cursor")
	recentCursor := r.URL.Query().Get("recent_cursor")

	localByID, e := a.localByShikiID(r)
	if e != nil {
		writeAPIError(w, r, e)
		return
	}
	localViews := map[int64]localRowView{}
	for id, row := range localByID {
		localViews[id] = localViewOfRow(row)
	}

	shikiClient := a.requestShiki(r)
	baseURL := a.shikiBaseURL(shikiClient)
	rates, animeByID := a.loadShikimoriUserSnapshot(r, shikiClient, cacheScopeOf(r), true)

	continueSource := buildContinueWatchingItems(rates, animeByID, localViews, baseURL)
	releaseSource := buildNewReleasesItems(rates, animeByID, localViews, baseURL, releaseDays, a.now())
	recentSource := buildLibraryRecentItems(rates, animeByID, localViews, baseURL)

	personalHero := buildPersonalizedHeroCandidates(rates, animeByID, localViews, baseURL)
	ongoingHero := a.loadOngoingHeroItems(r, shikiClient)
	heroSource := mergeHeroCandidates(personalHero, ongoingHero)

	heroItems, heroNext := paginate(heroSource, heroLimit, heroCursor)
	continueItems, continueNext := paginate(continueSource, continueLimit, continueCursor)
	releaseItems, releaseNext := paginate(releaseSource, releasesLimit, releasesCursor)
	recentItems, recentNext := paginate(recentSource, recentLimit, recentCursor)

	visible := append(append(append(heroItems, continueItems...), releaseItems...), recentItems...)
	detailsMap := a.loadAnimeDetailsMap(r, shikiClient, visibleIDs(visible))
	for i := range heroItems {
		heroItems[i] = applyDetailFieldsToCard(heroItems[i], detailsMap, baseURL)
	}
	for i := range continueItems {
		continueItems[i] = applyDetailFieldsToCard(continueItems[i], detailsMap, baseURL)
	}
	for i := range releaseItems {
		releaseItems[i] = applyDetailFieldsToCard(releaseItems[i], detailsMap, baseURL)
	}
	for i := range recentItems {
		recentItems[i] = applyDetailFieldsToCard(recentItems[i], detailsMap, baseURL)
	}

	writeJSON(w, 200, map[string]any{
		"hero": map[string]any{
			"items":       heroItems,
			"next_cursor": heroNext,
		},
		"sections": map[string]any{
			"continue_watching": map[string]any{"items": continueItems, "next_cursor": continueNext},
			"new_releases":      map[string]any{"items": releaseItems, "next_cursor": releaseNext},
			"library_recent":    map[string]any{"items": recentItems, "next_cursor": recentNext},
		},
	})
}

// loadOngoingHeroItems caches the ongoing gallery candidates (python
// home:hero:ongoing:v2).
func (a *App) loadOngoingHeroItems(r *http.Request, client ShikiClient) []map[string]any {
	const key = "home:hero:ongoing:v2"
	if cached := a.cache.Get(key); cached != nil {
		if items, ok := cached["items"].([]any); ok {
			out := make([]map[string]any, 0, len(items))
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
	}

	var items []map[string]any
	if client != nil {
		candidates, err := client.FetchOngoingCandidates(r.Context())
		if err == nil {
			for _, c := range candidates {
				poster := c.PosterURL
				if poster == "" {
					poster = placeholderPoster
				}
				items = append(items, map[string]any{
					"shikimori_id":    c.ShikimoriID,
					"title_ru":        c.TitleRu,
					"title_en":        c.TitleEn,
					"poster_url":      poster,
					"background_url":  poster,
					"badge":           heroBadge,
					"year":            yearOrNil(c.Year),
					"genres":          []any{},
					"score":           nil,
					"description":     nil,
					"kind":            nil,
					"status":          "ongoing",
					"episodes":        nil,
					"episodes_aired":  nil,
					"next_episode":    nil,
					"next_episode_at": nil,
					"url":             "/anime/" + strconv.FormatInt(c.ShikimoriID, 10),
					"source_url":      c.SourceURL,
				})
			}
		}
	}
	if items == nil {
		items = []map[string]any{}
	}
	a.cache.Set(key, map[string]any{"items": items}, heroOngoingTTL)
	return items
}

// mergeHeroCandidates dedups personalized then ongoing candidates
// (python _merge_hero_candidates).
func mergeHeroCandidates(personalized, ongoing []map[string]any) []map[string]any {
	seen := map[int64]bool{}
	merged := make([]map[string]any, 0, len(personalized)+len(ongoing))
	for _, source := range [][]map[string]any{personalized, ongoing} {
		for _, item := range source {
			id, ok := coerceID(item["shikimori_id"])
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, item)
		}
	}
	return merged
}

// buildPersonalizedHeroCandidates picks list entries for the hero
// gallery (python _build_personalized_hero_candidates).
func buildPersonalizedHeroCandidates(rates []shikiRate, animeByID map[int64]map[string]any, localByID map[int64]localRowView, baseURL string) []map[string]any {
	supported := map[string]bool{"watching": true, "rewatching": true, "planned": true, "plan_to_watch": true}
	seen := map[int64]bool{}
	var items []map[string]any
	for _, rate := range rates {
		if !supported[rate.Status] {
			continue
		}
		if seen[rate.TargetID] {
			continue
		}
		animeRow, ok := animeByID[rate.TargetID]
		if !ok {
			continue
		}
		seen[rate.TargetID] = true

		local := localOrNil(localByID, rate.TargetID)
		card := buildHomeCard(rate.TargetID, animeRow, local, baseURL, "", nil)
		poster := stringOrEmpty(card["poster_url"])
		if poster == "" {
			poster = placeholderPoster
		}
		card["background_url"] = poster
		card["badge"] = heroLibraryBadge
		sourceURL := resolveAgainstBase(baseURL, stringOrEmpty(animeRow["url"]))
		if sourceURL == "" {
			sourceURL = baseURL + "/animes/" + strconv.FormatInt(rate.TargetID, 10)
		}
		card["source_url"] = sourceURL
		items = append(items, card)
	}
	return items
}

// buildContinueWatchingItems builds the continue_watching section
// (python _build_continue_watching_items): watching/rewatching rates
// with a local binding, an episode behind the aired count, newest
// local update first.
func buildContinueWatchingItems(rates []shikiRate, animeByID map[int64]map[string]any, localByID map[int64]localRowView, baseURL string) []map[string]any {
	type stamped struct {
		at   time.Time
		card map[string]any
	}
	var items []stamped
	for _, rate := range rates {
		if rate.Status != "watching" && rate.Status != "rewatching" {
			continue
		}
		local, ok := localByID[rate.TargetID]
		if !ok {
			continue
		}
		animeRow := animeByID[rate.TargetID]
		watched := rate.Episodes
		if watched == 0 {
			watched = local.CurrentEpisode
		}
		if watched <= 0 {
			continue
		}
		episodesAired := intOf(animeRow["episodes_aired"])
		nextEpisode := intOf(animeRow["next_episode"])
		maxAvailable := maxInt(episodesAired, nextEpisode-1, 0)
		if maxAvailable <= watched {
			continue
		}

		bottomRight := any(nil)
		if score, ok := coerceFloat(animeRow["score"]); ok {
			bottomRight = "⭐ " + trimScore(score)
		}
		card := buildHomeCard(rate.TargetID, animeRow, &local, baseURL,
			"Остановились на "+strconv.Itoa(watched)+" серии", bottomRight)
		items = append(items, stamped{at: local.UpdatedAt, card: card})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.card)
	}
	return out
}

// buildNewReleasesItems builds the new_releases section (python
// _build_new_releases_items): list entries whose next episode aired
// inside the release window, newest release first.
func buildNewReleasesItems(rates []shikiRate, animeByID map[int64]map[string]any, localByID map[int64]localRowView, baseURL string, releaseDays int, now time.Time) []map[string]any {
	lowerBound := now.UTC().AddDate(0, 0, -maxInt(releaseDays, 1))
	supported := map[string]bool{"watching": true, "rewatching": true, "planned": true, "plan_to_watch": true}

	type stamped struct {
		at   time.Time
		card map[string]any
	}
	var items []stamped
	for _, rate := range rates {
		if !supported[rate.Status] {
			continue
		}
		animeRow, ok := animeByID[rate.TargetID]
		if !ok {
			continue
		}
		releaseAt, ok := parseShikiDatetime(stringOrEmpty(animeRow["next_episode_at"]))
		if !ok {
			releaseAt, ok = parseShikiDatetime(stringOrEmpty(animeRow["updated_at"]))
			if !ok {
				continue
			}
		}
		if releaseAt.After(now) || releaseAt.Before(lowerBound) {
			continue
		}
		nextEpisode := intOf(animeRow["next_episode"])
		bottomLeft := "Новая серия"
		if nextEpisode > 0 {
			bottomLeft = "Вышла " + strconv.Itoa(nextEpisode) + " серия"
		}
		local := localOrNil(localByID, rate.TargetID)
		card := buildHomeCard(rate.TargetID, animeRow, local, baseURL, bottomLeft, "Обновлено "+formatDaysAgo(releaseAt, now))
		items = append(items, stamped{at: releaseAt, card: card})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.card)
	}
	return out
}

// buildLibraryRecentItems builds the library_recent section (python
// _build_library_recent_items) keyed by rate created_at (local
// updated_at fallback), with the semantic badge.
func buildLibraryRecentItems(rates []shikiRate, animeByID map[int64]map[string]any, localByID map[int64]localRowView, baseURL string) []map[string]any {
	type stamped struct {
		at   time.Time
		card map[string]any
	}
	var items []stamped
	seen := map[int64]bool{}
	for _, rate := range rates {
		if seen[rate.TargetID] {
			continue
		}
		seen[rate.TargetID] = true

		addedAt, ok := parseShikiDatetime(rate.CreatedAt)
		local, hasLocal := localByID[rate.TargetID]
		if !ok && hasLocal {
			addedAt, ok = local.UpdatedAt, true
		}
		if !ok {
			continue
		}
		localArg := (*localRowView)(nil)
		if hasLocal {
			l := local
			localArg = &l
		}
		animeRow := animeByID[rate.TargetID]
		card := buildHomeCard(rate.TargetID, animeRow, localArg, baseURL,
			"Добавлено в библиотеку "+formatDaysAgoStatic(addedAt), nil)
		card["badge"] = libraryRecentBadge(rate.Status, animeRow)
		items = append(items, stamped{at: addedAt, card: card})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.card)
	}
	return out
}

// libraryRecentBadge derives the semantic badge (python
// _build_library_recent_badge).
func libraryRecentBadge(rateStatus string, animeRow map[string]any) string {
	switch strings.ToLower(strings.TrimSpace(rateStatus)) {
	case "watching", "rewatching":
		return "Смотрю"
	case "planned", "plan_to_watch":
		return "В планах"
	}
	if score, ok := coerceFloat(animeRow["score"]); ok {
		if score >= 8.8 {
			return "Топ"
		}
		if score >= 8.0 {
			return "Рекомендовано"
		}
	}
	if strings.ToLower(strings.TrimSpace(stringOrEmpty(animeRow["status"]))) == "ongoing" {
		return "Онгоинг"
	}
	return "В библиотеке"
}

// buildRealLibraryItems builds the library section cards (python
// _build_real_library_items).
func buildRealLibraryItems(rates []shikiRate, animeByID map[int64]map[string]any, localByID map[int64]localRowView, baseURL string) []map[string]any {
	type stamped struct {
		at   time.Time
		card map[string]any
	}
	var items []stamped
	seen := map[int64]bool{}
	for _, rate := range rates {
		if rate.TargetType != "" && strings.ToLower(rate.TargetType) != "anime" {
			continue
		}
		if seen[rate.TargetID] {
			continue
		}
		seen[rate.TargetID] = true

		animeRow := animeByID[rate.TargetID]
		local, hasLocal := localByID[rate.TargetID]
		statusRaw := strings.ToLower(strings.TrimSpace(rate.Status))
		statusRu := statusLabelRu(statusRaw)
		watched := rate.Episodes
		total := intOf(animeRow["episodes"])
		progressSuffix := strconv.Itoa(watched)
		if total > 0 {
			progressSuffix = strconv.Itoa(watched) + "/" + strconv.Itoa(total)
		}
		localSuffix := ""
		if hasLocal && local.CurrentEpisode > 0 {
			localSuffix = " · Локально: EP " + strconv.Itoa(local.CurrentEpisode)
		}
		var bottomRight any
		if score, ok := coerceFloat(animeRow["score"]); ok {
			bottomRight = "⭐ " + trimScore(score)
		}

		localArg := (*localRowView)(nil)
		historyID := any(nil)
		if hasLocal {
			l := local
			localArg = &l
			historyID = local.HistoryID
		}
		card := buildHomeCard(rate.TargetID, animeRow, localArg, baseURL,
			statusRu+" · Эпизоды: "+progressSuffix+localSuffix, bottomRight)
		card["badge"] = statusRu
		card["status"] = statusRaw
		card["status_ru"] = statusRu
		card["score_user"] = rate.Score
		card["episodes_watched"] = watched
		card["shikimori_rate_id"] = rate.ID
		card["history_id"] = historyID
		card["has_local_binding"] = hasLocal

		updatedAt, ok := parseShikiDatetime(rate.UpdatedAt)
		if !ok {
			updatedAt, _ = parseShikiDatetime(rate.CreatedAt)
		}
		items = append(items, stamped{at: updatedAt, card: card})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.card)
	}
	return out
}

// buildLocalLibraryFallbackItems builds cards from local rows only
// (python _build_local_library_fallback_items).
func buildLocalLibraryFallbackItems(historyRows []storage.AnimeProgress, baseURL string) []map[string]any {
	type stamped struct {
		at   time.Time
		card map[string]any
	}
	var items []stamped
	seen := map[int64]bool{}
	for i := range historyRows {
		row := &historyRows[i]
		if row.ShikimoriID == nil || seen[*row.ShikimoriID] {
			continue
		}
		seen[*row.ShikimoriID] = true

		statusRaw := strings.ToLower(strings.TrimSpace(row.ShikimoriStatus))
		statusRu := "Локально"
		if statusRaw != "" {
			statusRu = statusLabelRu(statusRaw)
		}
		view := localViewOfRow(row)
		card := buildHomeCard(*row.ShikimoriID, map[string]any{}, &view, baseURL,
			statusRu+" · Локально: EP "+strconv.Itoa(view.CurrentEpisode), nil)
		card["badge"] = statusRu
		card["status"] = statusRaw
		if statusRaw == "" {
			card["status"] = "local"
		}
		card["status_ru"] = statusRu
		card["history_id"] = row.ID
		card["has_local_binding"] = true
		items = append(items, stamped{at: row.UpdatedAt, card: card})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.card)
	}
	return out
}

// statusLabelRu maps a shikimori user status to its russian label
// (python _SHIKIMORI_STATUS_RU + _status_label_ru).
func statusLabelRu(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "watching":
		return "Смотрю"
	case "rewatching":
		return "Пересматриваю"
	case "completed":
		return "Просмотрено"
	case "on_hold":
		return "Отложено"
	case "dropped":
		return "Брошено"
	case "planned", "plan_to_watch":
		return "В планах"
	}
	return "Без статуса"
}

// formatDaysAgo renders the compact relative label (python
// _format_days_ago).
func formatDaysAgo(dt, now time.Time) string {
	days := int(now.Sub(dt).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return daysLabel(days)
}

func formatDaysAgoStatic(dt time.Time) string {
	return formatDaysAgo(dt, time.Now())
}

func daysLabel(days int) string {
	switch days {
	case 0:
		return "сегодня"
	case 1:
		return "1 день назад"
	default:
		return strconv.Itoa(days) + " дн. назад"
	}
}
