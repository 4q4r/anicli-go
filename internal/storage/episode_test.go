package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func seedAnime(t *testing.T, st *Store) int64 {
	t.Helper()
	p := fullProgress("https://animego/a1")
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("seed anime: %v", err)
	}
	return p.ID
}

func TestEpisodeProgressUpsertRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	animeID := seedAnime(t, st)

	quality := int64(720)
	e := &EpisodeProgress{
		AnimeID:     animeID,
		Episode:     "5",
		PositionSec: 250,
		DurationSec: 1440,
		VideoKey:    strPtr("hls-1080"),
		AudioKey:    strPtr("jap"),
		Quality:     &quality,
	}
	if err := st.Episodes.Upsert(context.Background(), e); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("upsert did not set ID")
	}

	got, err := st.Episodes.GetByID(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(*got, *e) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, *e)
	}
}

func TestEpisodeProgressUpsertOverwrite(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	if err := st.Episodes.Upsert(ctx, &EpisodeProgress{
		AnimeID: animeID, Episode: "5", PositionSec: 10, DurationSec: 100,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.Episodes.Upsert(ctx, &EpisodeProgress{
		AnimeID: animeID, Episode: "5", PositionSec: 999, DurationSec: 1000,
	}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	got, err := st.Episodes.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert must not duplicate)", len(got))
	}
	if got[0].PositionSec != 999 || got[0].DurationSec != 1000 {
		t.Errorf("overwrite not persisted: %+v", got[0])
	}
}

func TestEpisodeProgressListByAnimeOrder(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	// Inserted out of numeric order; lexical order ("1","10","2") would be wrong.
	for _, ep := range []string{"2", "10", "1"} {
		if err := st.Episodes.Upsert(ctx, &EpisodeProgress{
			AnimeID: animeID, Episode: ep,
		}); err != nil {
			t.Fatalf("seed %s: %v", ep, err)
		}
	}
	// A non-numeric episode must not break listing; the list then keeps
	// insertion order (id order).
	if err := st.Episodes.Upsert(ctx, &EpisodeProgress{
		AnimeID: animeID, Episode: "OVA",
	}); err != nil {
		t.Fatalf("seed OVA: %v", err)
	}

	got, err := st.Episodes.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"1", "2", "10", "OVA"}
	if len(got) != len(want) {
		t.Fatalf("episodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Episode != want[i] {
			t.Fatalf("episodes = %v, want %v", got, want)
		}
	}
}

func TestEpisodeProgressListByAnimeEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	got, err := st.Episodes.ListByAnime(context.Background(), 1)
	if err != nil {
		t.Fatalf("list on empty table: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %d, want 0", len(got))
	}
}

func TestEpisodeProgressForeignKey(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	err := st.Episodes.Upsert(context.Background(), &EpisodeProgress{
		AnimeID: 424242, Episode: "1",
	})
	if err == nil {
		t.Fatal("upsert for missing anime: want FK error, got nil")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("error = %v, want FOREIGN KEY constraint failure", err)
	}
}

func TestEpisodeProgressGetByIDNotFound(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, err := st.Episodes.GetByID(context.Background(), 9)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound", err)
	}
}
