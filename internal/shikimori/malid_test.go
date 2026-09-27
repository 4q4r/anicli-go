package shikimori

import (
	"encoding/json"
	"testing"
)

// malIDFixtures covers every documented carrier of the MAL id on the
// Shikimori anime card: the direct fields (mal_id / myanimelist_id —
// different API generations expose different names) and the external
// links array (the myanimelist.net/anime/<id>/<slug> URL form).
func malIDFixtures() map[string]struct {
	payload string
	want    int64
} {
	return map[string]struct {
		payload string
		want    int64
	}{
		"mal_id field": {
			`{"id":1,"mal_id":1,"name":"Cowboy Bebop"}`, 1,
		},
		"myanimelist_id field": {
			`{"id":1,"myanimelist_id":1,"name":"Cowboy Bebop"}`, 1,
		},
		"links array with MAL url": {
			`{"id":21,"links":[{"id":1,"url":"https://myanimelist.net/anime/21/One_Piece","kind":"myanimelist"}]}`, 21,
		},
		"links array ignores non-MAL entries": {
			`{"id":21,"links":[{"id":2,"url":"https://anidb.net/anime/1","kind":"anime_db"},{"id":3,"url":"https://myanimelist.net/anime/21/One_Piece","kind":"myanimelist"}]}`, 21,
		},
		"zero values ignored": {
			`{"id":1,"mal_id":0,"myanimelist_id":0}`, 0,
		},
		"no mapping at all": {
			`{"id":999,"name":"Shikimori-only"}`, 0,
		},
		"mal_id wins over links": {
			`{"id":1,"mal_id":5,"links":[{"url":"https://myanimelist.net/anime/9/X"}]}`, 5,
		},
	}
}

// TestAnimeMALID pins the shikimori->MAL id extraction from the anime
// card payload (PR112): every documented carrier parses, the direct
// field wins over the links scrape, absent mapping yields 0.
func TestAnimeMALID(t *testing.T) {
	t.Parallel()

	for name, tc := range malIDFixtures() {
		var anime Anime
		if err := json.Unmarshal([]byte(tc.payload), &anime); err != nil {
			t.Fatalf("%s: decode fixture: %v", name, err)
		}
		if got := anime.MALID(); got != tc.want {
			t.Errorf("%s: MALID() = %d, want %d", name, got, tc.want)
		}
	}
}
