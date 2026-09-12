package shikimori

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestGetAnimeParsesBaseEntry pins /api/animes/{id} decoding and the
// API-only poster construction from the image object.
func TestGetAnimeParsesBaseEntry(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/animes/21":
			writeJSON(w, map[string]any{
				"id": 21, "name": "Cowboy Bebop", "russian": "Ковбой Бибоп",
				"image": map[string]any{
					"original": "/animes/original/21.jpg",
					"preview":  "/animes/preview/21.jpg",
				},
				"episodes": 26, "episodes_aired": 26,
				"status": "released", "kind": "TV", "score": 8.9, "rating": "R+",
				"description": "space western",
				"genres":      []map[string]any{{"id": 1, "name": "Action"}},
			})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	anime, err := c.GetAnime(context.Background(), 21)
	if err != nil {
		t.Fatalf("GetAnime: %v", err)
	}
	if anime.Name != "Cowboy Bebop" || anime.Russian != "Ковбой Бибоп" {
		t.Errorf("names = %q / %q", anime.Name, anime.Russian)
	}
	if anime.Episodes != 26 || anime.Status != "released" || anime.Kind != "TV" {
		t.Errorf("core fields = %+v", anime)
	}
	if len(anime.Genres) != 1 || anime.Genres[0].Name != "Action" {
		t.Errorf("genres = %+v", anime.Genres)
	}

	poster := c.PosterURL(anime)
	want := c.baseURL + "/animes/original/21.jpg"
	if poster != want {
		t.Errorf("poster = %q, want %q", poster, want)
	}
}

// TestGetAnimePosterURLEmptyImage pins: no image object -> no poster URL.
func TestGetAnimePosterURLEmptyImage(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 1, "name": "NoArt"})
	})

	anime, err := c.GetAnime(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetAnime: %v", err)
	}
	if got := c.PosterURL(anime); got != "" {
		t.Errorf("poster = %q, want empty", got)
	}
}

// TestPosterURLTrimsBaseSlash pins base-URL normalization.
func TestPosterURLTrimsBaseSlash(t *testing.T) {
	t.Parallel()

	a := &Anime{Image: Image{Original: "/animes/original/1.jpg"}}
	if got := a.PosterURL("https://shikimori.one/"); got != "https://shikimori.one/animes/original/1.jpg" {
		t.Errorf("poster = %q", got)
	}
	if got := a.PosterURL("https://shikimori.one"); got != "https://shikimori.one/animes/original/1.jpg" {
		t.Errorf("poster = %q", got)
	}
}

// TestGetAnimeDetailsAggregatesRelations pins the details aggregate:
// /roles split into characters/staff, /related, /similar — mirrored both
// inside the anime payload and at the top level.
func TestGetAnimeDetailsAggregatesRelations(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/animes/21":
			writeJSON(w, map[string]any{"id": 21, "name": "Bebop",
				"image": map[string]any{"original": "/animes/original/21.jpg"}})
		case "/api/animes/21/roles":
			writeJSON(w, []map[string]any{
				{
					"character": map[string]any{"id": 1, "name": "Spike Spiegel",
						"image": map[string]any{"original": "/characters/original/1.jpg"}},
					"roles": []string{"Main"},
				},
				{
					"person": map[string]any{"id": 2, "name": "Shinichiro Watanabe"},
					"roles":  []string{"Director"},
				},
				{
					"person":    map[string]any{"id": 3, "name": "Megumi Hayashibara"},
					"character": nil,
					"roles":     []string{"Japanese"},
				},
			})
		case "/api/animes/21/related":
			writeJSON(w, []map[string]any{
				{
					"relation_en":      "sequel",
					"relation_russian": "продолжение",
					"anime":            map[string]any{"id": 22, "name": "Bebop Movie", "kind": "Movie"},
				},
				{
					"relation_en": "side_story",
					"manga":       map[string]any{"id": 31, "name": "Bebop Manga", "kind": "manga"},
				},
			})
		case "/api/animes/21/similar":
			writeJSON(w, []map[string]any{
				{"id": 40, "name": "Samurai Champloo", "score": 8.5},
			})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	details, err := c.GetAnimeDetails(context.Background(), 21)
	if err != nil {
		t.Fatalf("GetAnimeDetails: %v", err)
	}

	if len(details.Characters) != 1 || details.Characters[0].Name != "Spike Spiegel" {
		t.Errorf("characters = %+v", details.Characters)
	}
	if len(details.Characters) == 1 {
		roles := details.Characters[0].Roles
		if len(roles) != 1 || roles[0] != "Main" {
			t.Errorf("character roles = %+v, want [Main]", roles)
		}
	}
	if len(details.Staff) != 2 {
		t.Fatalf("staff = %+v, want 2 entries", details.Staff)
	}
	if details.Staff[0].Name != "Shinichiro Watanabe" || details.Staff[0].Roles[0] != "Director" {
		t.Errorf("staff[0] = %+v", details.Staff[0])
	}
	if len(details.Related) != 2 {
		t.Fatalf("related = %+v, want 2", details.Related)
	}
	if details.Related[0].Relation != "sequel" || details.Related[0].Anime == nil ||
		details.Related[0].Anime.Name != "Bebop Movie" {
		t.Errorf("related[0] = %+v", details.Related[0])
	}
	if details.Related[1].Manga == nil || details.Related[1].Manga.Name != "Bebop Manga" {
		t.Errorf("related[1] = %+v", details.Related[1])
	}
	if len(details.Similar) != 1 || details.Similar[0].Name != "Samurai Champloo" {
		t.Errorf("similar = %+v", details.Similar)
	}

	// Inventory ruling: relations mirrored inside the anime payload.
	if len(details.Anime.Characters) != 1 || len(details.Anime.Staff) != 2 ||
		len(details.Anime.Related) != 2 || len(details.Anime.Similar) != 1 {
		t.Errorf("anime-payload mirrors missing: %+v", details.Anime)
	}

	// All four endpoints consulted exactly once.
	for _, path := range []string{
		"/api/animes/21", "/api/animes/21/roles",
		"/api/animes/21/related", "/api/animes/21/similar",
	} {
		if got := log.count(path); got != 1 {
			t.Errorf("%s hits = %d, want 1", path, got)
		}
	}
}

// TestGetAnimeDetailsSerializesForCache pins the details payload round-trip
// through the storage cache JSON contract.
func TestGetAnimeDetailsSerializesForCache(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/animes/1":
			writeJSON(w, map[string]any{"id": 1, "name": "A"})
		case "/api/animes/1/roles":
			writeJSON(w, []map[string]any{})
		case "/api/animes/1/related":
			writeJSON(w, []map[string]any{})
		case "/api/animes/1/similar":
			writeJSON(w, []map[string]any{})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	details, err := c.GetAnimeDetails(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetAnimeDetails: %v", err)
	}
	if err := detailsRoundTrip(details); err != nil {
		t.Errorf("payload round-trip: %v", err)
	}
}

// detailsRoundTrip marshals the aggregate through the storage-cache JSON
// contract and back, asserting identity of the scalar core.
func detailsRoundTrip(details *AnimeDetails) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	var back AnimeDetails
	if err := json.Unmarshal(raw, &back); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if back.Anime.ID != details.Anime.ID || back.Anime.Name != details.Anime.Name {
		return fmt.Errorf("anime core changed: %+v vs %+v", back.Anime, details.Anime)
	}
	if len(back.Characters) != len(details.Characters) || len(back.Staff) != len(details.Staff) ||
		len(back.Related) != len(details.Related) || len(back.Similar) != len(details.Similar) {
		return fmt.Errorf("relations changed: %+v", back)
	}
	return nil
}
