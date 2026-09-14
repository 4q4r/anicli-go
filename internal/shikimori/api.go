package shikimori

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalStatus maps user-facing rate statuses onto the API-v2
// canonical form. The UI vocabulary says "planned"; the user_rates API
// accepts only "plan_to_watch" (FEATURE_INVENTORY F: Shikimori 422 +
// planned->plan_to_watch normalization).
func CanonicalStatus(status string) string {
	if strings.ToLower(strings.TrimSpace(status)) == "planned" {
		return "plan_to_watch"
	}
	return status
}

// GetUserID resolves the Shikimori user id via /api/users/whoami and
// caches it for the client lifetime.
func (c *Client) GetUserID(ctx context.Context) (int64, error) {
	id, _, err := c.WhoAmI(ctx)
	return id, err
}

// WhoAmI resolves the user id and nickname via /api/users/whoami and
// caches both for the client lifetime (PR26: the setup screens greet
// the user by nickname). An absent nickname in the reply decodes as
// "" — callers render the id fallback.
func (c *Client) WhoAmI(ctx context.Context) (int64, string, error) {
	if err := c.requireMode(true); err != nil {
		return 0, "", err
	}

	c.mu.Lock()
	cached := c.user
	c.mu.Unlock()
	if cached != nil {
		return cached.id, cached.nickname, nil
	}

	resp, err := c.get(ctx, "/api/users/whoami", nil)
	if err != nil {
		if authFailure(statusCodeOf(err)) {
			return 0, "", fmt.Errorf("%w: whoami: %w", ErrAuthRequired, err)
		}
		return 0, "", fmt.Errorf("shikimori whoami: %w", err)
	}
	var reply whoamiResponse
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		return 0, "", fmt.Errorf("shikimori whoami: decode response: %w", err)
	}
	if reply.ID == 0 {
		// Anonymous whoami: credentials absent or rejected.
		return 0, "", fmt.Errorf("%w: whoami returned no user", ErrAuthRequired)
	}

	c.mu.Lock()
	c.user = &userIdentity{id: reply.ID, nickname: reply.Nickname}
	c.mu.Unlock()
	return reply.ID, reply.Nickname, nil
}

// GetAnime fetches the base anime entry from /api/animes/{id}. Public:
// works without credentials.
func (c *Client) GetAnime(ctx context.Context, shikimoriID int64) (*Anime, error) {
	if err := c.requireMode(false); err != nil {
		return nil, err
	}

	resp, err := c.get(ctx, "/api/animes/"+strconv.FormatInt(shikimoriID, 10), nil)
	if err != nil {
		return nil, fmt.Errorf("shikimori get_anime %d: %w", shikimoriID, err)
	}
	var anime Anime
	if err := json.Unmarshal(resp.Body, &anime); err != nil {
		return nil, fmt.Errorf("shikimori get_anime %d: decode response: %w", shikimoriID, err)
	}
	return &anime, nil
}

// GetAnimeDetails fetches the anime aggregate for the details cache:
// the base entry plus structured relations from the /roles, /related and
// /similar endpoints (API-only ruling: no HTML fallback). Relations are
// exposed both inside the anime payload and at the top level
// (FEATURE_INVENTORY F). Public.
func (c *Client) GetAnimeDetails(ctx context.Context, shikimoriID int64) (*AnimeDetails, error) {
	anime, err := c.GetAnime(ctx, shikimoriID)
	if err != nil {
		return nil, err
	}
	idPath := "/api/animes/" + strconv.FormatInt(shikimoriID, 10)

	// Roles -> characters + staff.
	rolesResp, err := c.get(ctx, idPath+"/roles", nil)
	if err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d roles: %w", shikimoriID, err)
	}
	var roles []rolesEntry
	if err := json.Unmarshal(rolesResp.Body, &roles); err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d roles: decode: %w", shikimoriID, err)
	}
	var characters []Character
	var staff []StaffMember
	for _, entry := range roles {
		if entry.Character != nil {
			entry.Character.Roles = entry.Roles
			characters = append(characters, *entry.Character)
		}
		if entry.Person != nil {
			entry.Person.Roles = entry.Roles
			staff = append(staff, *entry.Person)
		}
	}

	// Related anime/manga.
	relatedResp, err := c.get(ctx, idPath+"/related", nil)
	if err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d related: %w", shikimoriID, err)
	}
	var related []RelatedEntry
	if err := json.Unmarshal(relatedResp.Body, &related); err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d related: decode: %w", shikimoriID, err)
	}

	// Similar anime.
	similarResp, err := c.get(ctx, idPath+"/similar", nil)
	if err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d similar: %w", shikimoriID, err)
	}
	var similar []Anime
	if err := json.Unmarshal(similarResp.Body, &similar); err != nil {
		return nil, fmt.Errorf("shikimori get_anime_details %d similar: decode: %w", shikimoriID, err)
	}

	// Mirror the relations into the anime payload (inventory ruling).
	anime.Characters = characters
	anime.Staff = staff
	anime.Related = related
	anime.Similar = similar

	return &AnimeDetails{
		Anime:      *anime,
		Characters: characters,
		Staff:      staff,
		Related:    related,
		Similar:    similar,
	}, nil
}

// PosterURL renders the absolute poster URL for an anime id's image path
// against this client's base URL.
func (c *Client) PosterURL(anime *Anime) string {
	return anime.PosterURL(c.baseURL)
}

// GetUserRates fetches the user's anime list entries
// (python get_user_rates: v2 endpoint, target_type Anime, high limit).
func (c *Client) GetUserRates(ctx context.Context) ([]UserRate, error) {
	uid, err := c.GetUserID(ctx)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("user_id", strconv.FormatInt(uid, 10))
	query.Set("target_type", "Anime")
	query.Set("limit", "1000")

	resp, err := c.get(ctx, "/api/v2/user_rates", query)
	if err != nil {
		return nil, fmt.Errorf("shikimori user_rates: %w", err)
	}
	var rates []UserRate
	if err := json.Unmarshal(resp.Body, &rates); err != nil {
		return nil, fmt.Errorf("shikimori user_rates: decode response: %w", err)
	}
	return rates, nil
}

// CreateRate adds an anime to the user's list (POST /api/v2/user_rates)
// and returns the new rate id. Payload status is sent verbatim; the 422
// handler retries once with the canonical form when it was "planned".
func (c *Client) CreateRate(ctx context.Context, shikimoriID int64, input RateInput) (int64, error) {
	uid, err := c.GetUserID(ctx)
	if err != nil {
		return 0, err
	}

	build := func(in RateInput) any {
		return rateWrapper[createRateInput]{
			UserRate: createRateInput{
				RateInput:  in,
				UserID:     uid,
				TargetID:   shikimoriID,
				TargetType: "Anime",
			},
		}
	}
	rateID, err := c.mutate(ctx, http.MethodPost, "/api/v2/user_rates", "create_rate", input, build)
	if err != nil {
		return 0, err
	}
	if rateID == 0 {
		return 0, fmt.Errorf("shikimori create_rate: response carried no rate id")
	}
	return rateID, nil
}

// UpdateRate patches an existing list entry (PATCH /api/v2/user_rates/{id})
// and returns the rate id.
func (c *Client) UpdateRate(ctx context.Context, rateID int64, input RateInput) (int64, error) {
	build := func(in RateInput) any {
		return rateWrapper[RateInput]{UserRate: in}
	}
	id, err := c.mutate(ctx, http.MethodPatch,
		"/api/v2/user_rates/"+strconv.FormatInt(rateID, 10), "update_rate", input, build)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		id = rateID // 204-style replies keep the caller's id
	}
	return id, nil
}

// DeleteRate removes a list entry (DELETE /api/v2/user_rates/{id}).
func (c *Client) DeleteRate(ctx context.Context, rateID int64) error {
	build := func(in RateInput) any {
		return rateWrapper[RateInput]{UserRate: in}
	}
	_, err := c.mutate(ctx, http.MethodDelete,
		"/api/v2/user_rates/"+strconv.FormatInt(rateID, 10), "delete_rate", RateInput{}, build)
	return err
}

// AddAnimeToList is the sugar path of the Python sync flows: create a
// list entry with a status. The status is canonicalized before the wire,
// so "planned" never triggers the 422 round-trip.
func (c *Client) AddAnimeToList(ctx context.Context, shikimoriID int64, status string) (int64, error) {
	input := RateInput{Status: CanonicalStatus(status)}
	return c.CreateRate(ctx, shikimoriID, input)
}
