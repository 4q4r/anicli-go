package shikimori

import (
	"encoding/json"
	"os"
	"testing"
)

// TestAnimeFullNameFieldsRoundTripLiveFixture pins the additive name
// fields against the controller's verbatim capture of
// GET shikimori.io/api/animes/51553 (2026-09-24, follows the 301 from
// .one): name="Tongari Boushi no Atelier" (original),
// russian="Ателье колдовских колпаков",
// english=["Witch Hat Atelier"], japanese=["とんがり帽子のアトリエ"],
// synonyms=["Atelier of Witch Hat"]. The parsed shape must round-trip
// the fixture (decode → encode → decode, fields stable).
//
// Note: 51553 is BOTH the Shikimori ID and the AniList ID for this
// title — a coincidence of this capture, not a general rule.
func TestAnimeFullNameFieldsRoundTripLiveFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/anime_51553_live.json")
	if err != nil {
		t.Fatalf("read live fixture: %v", err)
	}
	var anime Anime
	if err := json.Unmarshal(raw, &anime); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if anime.ID != 51553 {
		t.Errorf("ID = %d, want 51553", anime.ID)
	}
	if anime.Name != "Tongari Boushi no Atelier" {
		t.Errorf("Name = %q, want the original title", anime.Name)
	}
	if anime.Russian != "Ателье колдовских колпаков" {
		t.Errorf("Russian = %q, want the russian title", anime.Russian)
	}
	if len(anime.English) != 1 || anime.English[0] != "Witch Hat Atelier" {
		t.Errorf("English = %v, want [Witch Hat Atelier]", anime.English)
	}
	if len(anime.Japanese) != 1 || anime.Japanese[0] != "とんがり帽子のアトリエ" {
		t.Errorf("Japanese = %v, want [とんがり帽子のアトリエ]", anime.Japanese)
	}
	if len(anime.Synonyms) != 1 || anime.Synonyms[0] != "Atelier of Witch Hat" {
		t.Errorf("Synonyms = %v, want [Atelier of Witch Hat]", anime.Synonyms)
	}

	// Round-trip: decode → encode → decode leaves the name inventory
	// byte-stable (the json tags match the wire shape).
	encoded, err := json.Marshal(&anime)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var again Anime
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if again.Name != anime.Name || again.Russian != anime.Russian ||
		stringsJoin(again.English) != stringsJoin(anime.English) ||
		stringsJoin(again.Japanese) != stringsJoin(anime.Japanese) ||
		stringsJoin(again.Synonyms) != stringsJoin(anime.Synonyms) {
		t.Errorf("round-trip diverged: %+v vs %+v", again, anime)
	}
}

func stringsJoin(in []string) string {
	out := ""
	for _, s := range in {
		out += s + "\x00"
	}
	return out
}
