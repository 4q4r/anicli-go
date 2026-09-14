package shikimori

import "encoding/json"

// Wire types of the Shikimori API (v1 animes endpoints + v2 user_rates),
// ported from the frozen Python original's dict shapes plus the
// structured-relations ruling (FEATURE_INVENTORY F: characters/staff/
// related/similar arrays live both inside the anime payload and at the
// top level of the details aggregate).

// Image is Shikimori's image object; fields are site-root-relative paths.
// The full size ladder mirrors the API payload (python _anime_poster
// prefers original > main > preview > x96 > x48).
type Image struct {
	// Original is the full-size artwork path, e.g. "/animes/original/1.jpg".
	Original string `json:"original,omitempty"`
	// Main is the mid-size artwork path.
	Main string `json:"main,omitempty"`
	// Preview is the thumbnail path.
	Preview string `json:"preview,omitempty"`
	// X96 is the tiny 96px artwork path.
	X96 string `json:"x96,omitempty"`
	// X48 is the tiny 48px artwork path.
	X48 string `json:"x48,omitempty"`
}

// Genre is a brief anime genre entry.
type Genre struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Russian string `json:"russian,omitempty"`
}

// Anime is one entry of /api/animes/{id} (and the brief shape nested in
// related/similar listings). Relations arrays are filled by
// GetAnimeDetails, not by the wire format of the base endpoint.
type Anime struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Russian       string `json:"russian,omitempty"`
	Image         Image  `json:"image"`
	Episodes      int    `json:"episodes"`
	EpisodesAired int    `json:"episodes_aired,omitempty"`
	Status        string `json:"status,omitempty"` // ongoing, released, anounced...
	Kind          string `json:"kind,omitempty"`   // TV, Movie, OVA...
	// Score arrives as a JSON string ("8.61") on /api/animes list
	// responses; json.Number tolerates both wire shapes.
	Score       json.Number `json:"score,omitempty"`
	Rating      string      `json:"rating,omitempty"`
	Description string      `json:"description,omitempty"`
	Genres      []Genre     `json:"genres,omitempty"`
	AiredOn     string      `json:"aired_on,omitempty"`
	ReleasedOn  string      `json:"released_on,omitempty"`
	// NextEpisode and NextEpisodeAt feed the home feed and release
	// calendar projections (python anime rows carry them on /api/animes
	// list responses).
	NextEpisode   int    `json:"next_episode,omitempty"`
	NextEpisodeAt string `json:"next_episode_at,omitempty"`
	URL           string `json:"url,omitempty"`
	// Characters/Staff/Related/Similar implement the inventory ruling:
	// structured relations inside the anime payload. Empty when the anime
	// was fetched without relations.
	Characters []Character    `json:"characters,omitempty"`
	Staff      []StaffMember  `json:"staff,omitempty"`
	Related    []RelatedEntry `json:"related,omitempty"`
	Similar    []Anime        `json:"similar,omitempty"`
}

// PosterURL renders the absolute poster URL for the anime from the
// site-root-relative original image path (API-only ruling: no HTML
// fallback). Empty when the API carried no image.
func (a *Anime) PosterURL(baseURL string) string {
	if a.Image.Original == "" {
		return ""
	}
	return trimTrailingSlash(baseURL) + a.Image.Original
}

// Character is one /roles character entry.
type Character struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Russian string `json:"russian,omitempty"`
	Image   Image  `json:"image"`
	// Roles are the /roles role tags (e.g. "Main").
	Roles []string `json:"roles,omitempty"`
}

// StaffMember is one /roles person entry (seiyuu, authors, directors).
type StaffMember struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Russian string `json:"russian,omitempty"`
	Image   Image  `json:"image"`
	// Roles are the /roles role tags (e.g. "Japanese", "Author").
	Roles []string `json:"roles,omitempty"`
}

// RelatedManga is the brief manga half of a /related entry.
type RelatedManga struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Russian string `json:"russian,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// RelatedEntry is one /related row: an anime and/or manga plus the
// relation label.
type RelatedEntry struct {
	// Relation is the English relation key, e.g. "sequel", "side_story".
	Relation   string        `json:"relation_en,omitempty"`
	RelationRu string        `json:"relation_russian,omitempty"`
	Anime      *Anime        `json:"anime,omitempty"`
	Manga      *RelatedManga `json:"manga,omitempty"`
}

// AnimeDetails is the aggregate stored in shikimori_anime_details_cache:
// the base anime plus the structured relations, exposed both inside the
// anime payload and at the top level (inventory F ruling).
type AnimeDetails struct {
	Anime      Anime          `json:"anime"`
	Characters []Character    `json:"characters"`
	Staff      []StaffMember  `json:"staff"`
	Related    []RelatedEntry `json:"related"`
	Similar    []Anime        `json:"similar"`
}

// UserRate is one row of /api/v2/user_rates.
type UserRate struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	TargetID   int64  `json:"target_id"`
	TargetType string `json:"target_type"`
	Score      int    `json:"score"`
	Status     string `json:"status"`
	Episodes   int    `json:"episodes"`
	Rewatches  int    `json:"rewatches"`
	Text       string `json:"text,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// RateInput is the caller-controlled user_rate payload. Pointer fields
// implement the conditional-omission ruling: zero-valued score and
// rewatches never leave the client (FEATURE_INVENTORY F).
type RateInput struct {
	Episodes  *int   `json:"episodes,omitempty"`
	Status    string `json:"status,omitempty"`
	Score     *int   `json:"score,omitempty"`
	Rewatches *int   `json:"rewatches,omitempty"`
}

// createRateInput is the POST body shape: RateInput flattened plus the
// create-only identity fields.
type createRateInput struct {
	RateInput
	UserID     int64  `json:"user_id"`
	TargetID   int64  `json:"target_id"`
	TargetType string `json:"target_type"`
}

// rateWrapper is the {"user_rate": {...}} envelope.
type rateWrapper[T any] struct {
	UserRate T `json:"user_rate"`
}

// rateResponse is the minimal create/update reply.
type rateResponse struct {
	ID int64 `json:"id"`
}

// whoamiResponse is the minimal whoami reply.
type whoamiResponse struct {
	ID int64 `json:"id"`
	// Nickname greets the user by name in the setup flows (PR26);
	// absent in degraded replies — callers fall back to the id.
	Nickname string `json:"nickname"`
}

// rolesEntry is one raw /roles row before splitting into character/staff:
// the roles tags sit at the entry top level and apply to whichever of
// character/person the row carries.
type rolesEntry struct {
	Character *Character   `json:"character"`
	Person    *StaffMember `json:"person"`
	Roles     []string     `json:"roles"`
}

// trimTrailingSlash drops one trailing "/" from a base URL.
func trimTrailingSlash(u string) string {
	if len(u) > 0 && u[len(u)-1] == '/' {
		return u[:len(u)-1]
	}
	return u
}
