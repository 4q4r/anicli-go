package api

import (
	"net/http"
	"time"

	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Request-scoped Shikimori credentials (KEEP ruling): the session's
// stored shiki fields override the app-default client per request
// (python _request_shiki_client). Aliases keep the builder signatures
// readable.

type (
	shikiRate             = shikimori.UserRate
	shikiAutocompleteItem = shikimori.AutocompleteItem
)

// requestShiki builds the request-scoped shikimori client: nil when no
// base client exists; the base client when the session carries no
// credentials; otherwise a derived client with the session's cookie or
// token credentials riding the shared transport.
func (a *App) requestShiki(r *http.Request) ShikiClient {
	base, isConcrete := a.shiki.(*shikimori.Client)
	if !isConcrete || a.shikiNet == nil {
		if a.shiki == nil {
			return nil
		}
		// Injected fake (tests): no per-user derivation.
		if authSessionOf(r) == nil {
			return nil
		}
		return a.shiki
	}
	sess := authSessionOf(r)
	if sess == nil {
		return nil
	}

	cfg := a.cfg.Shikimori
	scoped := false
	if sess.ShikiCookieSession != nil && *sess.ShikiCookieSession != "" {
		cfg.Session = *sess.ShikiCookieSession
		scoped = true
	}
	if sess.ShikiAccessToken != nil && *sess.ShikiAccessToken != "" {
		cfg.AccessToken = *sess.ShikiAccessToken
		scoped = true
	}
	if !scoped {
		return base
	}
	cfg.Enabled = true
	return shikimori.New(cfg, a.shikiNet, a.log)
}

// shikiBaseURL renders the effective shikimori root for URL
// normalization.
func (a *App) shikiBaseURL(ShikiClient) string {
	return shikimori.DefaultBaseURL
}

// cacheScopeOf derives the per-user cache scope (python cache_scope:
// shiki username, else login, else "anon").
func cacheScopeOf(r *http.Request) string {
	sess := authSessionOf(r)
	if sess == nil {
		return "anon"
	}
	if sess.ShikiUsername != nil && *sess.ShikiUsername != "" {
		return *sess.ShikiUsername
	}
	return sess.UserLogin
}

// snapshot is the cached user rates + anime rows pair (python
// shiki:snapshot:v2 payload).
type snapshot struct {
	rates     []shikiRate
	animeByID map[int64]map[string]any
}

// snapshotFullTTL caches the full snapshot (rates + anime rows);
// python used 43200s.
const snapshotFullTTL = 43200 * time.Second

// loadShikimoriUserSnapshot loads (and caches) the user's rates and
// anime rows (python _load_shikimori_user_snapshot). A client failure
// falls back to the cached snapshot, then to empty.
func (a *App) loadShikimoriUserSnapshot(r *http.Request, client ShikiClient, scope string, includeAnimeDetails bool) ([]shikiRate, map[int64]map[string]any) {
	mode := "rates"
	if includeAnimeDetails {
		mode = "full"
	}
	key := "shiki:snapshot:v2:" + cacheHash(scope) + ":" + mode

	if cached := a.cache.Get(key); cached != nil {
		snap := decodeSnapshot(r, cached)
		if client == nil || len(snap.rates) > 0 {
			return snap.rates, snap.animeByID
		}
	}

	if client == nil {
		return nil, map[int64]map[string]any{}
	}

	rates, err := client.GetUserRates(r.Context())
	if err != nil {
		// python parity: warn, fall back to cache (already checked
		// above), else empty.
		return nil, map[int64]map[string]any{}
	}

	animeByID := map[int64]map[string]any{}
	if includeAnimeDetails && len(rates) > 0 {
		ids := make([]int64, 0, len(rates))
		for _, rate := range rates {
			ids = append(ids, rate.TargetID)
		}
		if animes, err := client.GetAnimesInfo(r.Context(), sortedUniqueIDs(ids)); err == nil {
			for i := range animes {
				animeByID[animes[i].ID] = animeRowMap(&animes[i])
			}
		}
	}

	payload := encodeSnapshot(rates, animeByID, a.now())
	ttl := snapshotRatesTTL
	if includeAnimeDetails {
		ttl = snapshotFullTTL
	}
	a.cache.Set(key, payload, ttl)
	return rates, animeByID
}

// encodeSnapshot renders the cacheable snapshot payload.
func encodeSnapshot(rates []shikiRate, animeByID map[int64]map[string]any, now time.Time) map[string]any {
	animeRows := map[string]any{}
	for id, row := range animeByID {
		animeRows[int64ToString(id)] = row
	}
	rateRows := make([]any, 0, len(rates))
	for _, rate := range rates {
		rateRows = append(rateRows, map[string]any{
			"id": rate.ID, "user_id": rate.UserID, "target_id": rate.TargetID,
			"target_type": rate.TargetType, "score": rate.Score, "status": rate.Status,
			"episodes": rate.Episodes, "rewatches": rate.Rewatches,
			"created_at": rate.CreatedAt, "updated_at": rate.UpdatedAt,
		})
	}
	return map[string]any{
		"rates":       rateRows,
		"anime_by_id": animeRows,
		"updated_at":  now.UTC().Format(time.RFC3339),
	}
}

// decodeSnapshot rebuilds the snapshot from the cache payload.
func decodeSnapshot(_ *http.Request, cached map[string]any) snapshot {
	snap := snapshot{animeByID: map[int64]map[string]any{}}
	rawRates, _ := cached["rates"].([]any)
	for _, rr := range rawRates {
		m, ok := rr.(map[string]any)
		if !ok {
			continue
		}
		rate := shikiRate{}
		if v, ok := coerceID(m["id"]); ok {
			rate.ID = v
		}
		if v, ok := coerceID(m["user_id"]); ok {
			rate.UserID = v
		}
		if v, ok := coerceID(m["target_id"]); ok {
			rate.TargetID = v
		}
		rate.TargetType = stringOrEmpty(m["target_type"])
		if v, ok := coerceID(m["score"]); ok {
			rate.Score = int(v)
		}
		rate.Status = stringOrEmpty(m["status"])
		if v, ok := coerceID(m["episodes"]); ok {
			rate.Episodes = int(v)
		}
		if v, ok := coerceID(m["rewatches"]); ok {
			rate.Rewatches = int(v)
		}
		rate.CreatedAt = stringOrEmpty(m["created_at"])
		rate.UpdatedAt = stringOrEmpty(m["updated_at"])
		snap.rates = append(snap.rates, rate)
	}
	if rows, ok := cached["anime_by_id"].(map[string]any); ok {
		for key, value := range rows {
			if id, err := parseInt64(key); err == nil {
				if row, ok := value.(map[string]any); ok {
					snap.animeByID[id] = row
				}
			}
		}
	}
	return snap
}

// localByShikiID indexes history rows by shikimori id.
func (a *App) localByShikiID(r *http.Request) (map[int64]*storage.AnimeProgress, *apiError) {
	rows, err := a.store.Progress.ListHistory(r.Context(), "", 0, 0)
	if err != nil {
		return nil, errInternal("Failed to load history")
	}
	out := map[int64]*storage.AnimeProgress{}
	for i := range rows {
		if rows[i].ShikimoriID != nil {
			out[*rows[i].ShikimoriID] = &rows[i]
		}
	}
	return out, nil
}
