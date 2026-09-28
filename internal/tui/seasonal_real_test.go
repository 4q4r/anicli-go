package tui

// realSeasonal adapter tests (PR114): the Shikimori-first mapping,
// the MAL fallback dispatch and the honest no-source error.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// fakeShikiSeason records the shikimori leg.
type fakeShikiSeason struct {
	rows  []shikimori.Anime
	err   error
	calls int
}

// fakeMalSeason records the MAL leg.
type fakeMalSeason struct {
	nodes []mal.SeasonalNode
	err   error
	calls int
}

// adapterDeps builds a realSeasonal over the fakes.
func adapterDeps(shiki *fakeShikiSeason, malS *fakeMalSeason, shikiOn, malAuthed bool) realSeasonal {
	s := realSeasonal{}
	if shiki != nil && shikiOn {
		s.shikiFetch = func(_ context.Context, _ int, _ string, _ bool) ([]shikimori.Anime, error) {
			shiki.calls++
			return shiki.rows, shiki.err
		}
	}
	if malS != nil && malAuthed {
		s.malFetch = func(_ context.Context, _ int, _ string, _ int) ([]mal.SeasonalNode, error) {
			malS.calls++
			return malS.nodes, malS.err
		}
	}
	return s
}

// TestRealSeasonalShikimoriPreferred: the healthy shikimori path maps
// every row (ru title preferred with the en fallback, the zero score
// dropped, the weekday derived from next_episode_at) and never touches
// MAL.
func TestRealSeasonalShikimoriPreferred(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, time.October, 5, 16, 0, 0, 0, time.UTC).Format(time.RFC3339)
	shiki := &fakeShikiSeason{rows: []shikimori.Anime{
		{ID: 1, Name: "En Title", Russian: "Ру титул", Episodes: 12, EpisodesAired: 4,
			Score: "8.61", NextEpisodeAt: monday},
		{ID: 2, Name: "No Russian", Episodes: 0, EpisodesAired: 0, Score: "0"},
	}}
	malS := &fakeMalSeason{}
	svc := adapterDeps(shiki, malS, true, true)

	rows, err := svc.Season(context.Background(), 2026, "fall", true)
	if err != nil {
		t.Fatalf("Season: %v", err)
	}
	if malS.calls != 0 {
		t.Fatalf("MAL must stay untouched when shikimori succeeds")
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	first := rows[0]
	if first.ID != "1" || first.Title != "Ру титул" || first.Episodes != 12 ||
		first.EpisodesAired != 4 || first.Score != "8.61" {
		t.Fatalf("row0 drifted: %+v", first)
	}
	if !first.HasWeekday || first.Weekday != time.Monday {
		t.Fatalf("row0 weekday = (%v,%v), want monday", first.Weekday, first.HasWeekday)
	}
	if rows[1].Title != "No Russian" || rows[1].Score != "" || rows[1].HasWeekday {
		t.Fatalf("row1 drifted: %+v", rows[1])
	}
}

// TestRealSeasonalMALFallback: a failing shikimori defers to an
// authenticated MAL; ongoingOnly drops the non-airing nodes and the
// broadcast day maps through.
func TestRealSeasonalMALFallback(t *testing.T) {
	t.Parallel()

	shiki := &fakeShikiSeason{err: errors.New("shiki down")}
	malS := &fakeMalSeason{nodes: []mal.SeasonalNode{
		{ID: 11, Title: "Wire One", Mean: 8.42, NumEpisodes: 12, Status: "currently_airing",
			Day: time.Thursday, HasDay: true, StartTime: "22:30"},
		{ID: 12, Title: "Wire Finished", Mean: 7.9, NumEpisodes: 13, Status: "finished_airing",
			Day: time.Friday, HasDay: true},
	}}
	svc := adapterDeps(shiki, malS, true, true)

	rows, err := svc.Season(context.Background(), 2026, "fall", true)
	if err != nil {
		t.Fatalf("Season: %v", err)
	}
	if shiki.calls != 1 || malS.calls != 1 {
		t.Fatalf("legs = shiki %d, mal %d; want 1/1", shiki.calls, malS.calls)
	}
	if len(rows) != 1 {
		t.Fatalf("ongoingOnly must keep only currently_airing nodes, got %d", len(rows))
	}
	row := rows[0]
	if row.ID != "mal:11" || row.Title != "Wire One" || row.Score != "8.42" ||
		!row.HasWeekday || row.Weekday != time.Thursday {
		t.Fatalf("mal row drifted: %+v", row)
	}

	// A browsed season (ongoingOnly=false) keeps the finished node.
	malS.calls = 0
	rows, err = svc.Season(context.Background(), 2026, "fall", false)
	if err != nil {
		t.Fatalf("Season (browsed): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("browsed season must keep every node, got %d", len(rows))
	}
}

// TestRealSeasonalShikimoriDisabledMALAuthed: the disabled primary
// goes straight to MAL without a doomed request.
func TestRealSeasonalShikimoriDisabledMALAuthed(t *testing.T) {
	t.Parallel()

	malS := &fakeMalSeason{nodes: []mal.SeasonalNode{
		{ID: 21, Title: "Solo", Mean: 9.1, Status: "currently_airing", Day: time.Sunday, HasDay: true},
	}}
	svc := adapterDeps(nil, malS, false, true)

	rows, err := svc.Season(context.Background(), 2026, "winter", true)
	if err != nil {
		t.Fatalf("Season: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "Solo" {
		t.Fatalf("rows drifted: %+v", rows)
	}
}

// TestRealSeasonalBothFail: both legs failing surfaces a combined
// error naming each.
func TestRealSeasonalBothFail(t *testing.T) {
	t.Parallel()

	shiki := &fakeShikiSeason{err: errors.New("shiki down")}
	malS := &fakeMalSeason{err: errors.New("mal down")}
	svc := adapterDeps(shiki, malS, true, true)

	_, err := svc.Season(context.Background(), 2026, "fall", true)
	if err == nil {
		t.Fatal("both legs failing must surface an error")
	}
	for _, want := range []string{"shiki down", "mal down"} {
		if !contains(err.Error(), want) {
			t.Fatalf("combined error must name %q, got %v", want, err)
		}
	}
}

// TestRealSeasonalNoSource: shikimori off and MAL unauthenticated
// yields the honest unavailability error.
func TestRealSeasonalNoSource(t *testing.T) {
	t.Parallel()

	svc := adapterDeps(nil, nil, false, false)
	_, err := svc.Season(context.Background(), 2026, "fall", true)
	if err == nil || !contains(err.Error(), "no source") {
		t.Fatalf("err = %v, want the no-source verdict", err)
	}
}
