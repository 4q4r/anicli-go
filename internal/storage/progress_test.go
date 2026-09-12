package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

var refTime = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func strPtr(s string) *string { return &s }

// fullProgress builds a row exercising every column, keyed by source URL.
func fullProgress(sourceURL string) *AnimeProgress {
	return &AnimeProgress{
		Title:          "Ковбой Бибоп",
		Poster:         strPtr("https://x/p.jpg"),
		SourceID:       "animego",
		SourceURL:      sourceURL,
		CurrentEpisode: "3",
		VideoDub:       strPtr("subs"),
		AudioDub:       strPtr("jap"),
		ShikimoriTitle: strPtr("Cowboy Bebop"),
		BoundTitle:     strPtr("Ковбой Бибоп (animego)"),
		BoundSimilarity: func() *float64 {
			v := 0.87
			return &v
		}(),
		ShikimoriID:     func() *int64 { v := int64(1); return &v }(),
		ShikimoriRateID: func() *int64 { v := int64(42); return &v }(),
		ShikimoriStatus: "watching",
		Score:           9,
		TotalEpisodes:   26,
		Rewatches:       1,
		NeedsCorrection: true,
		ProgressSeconds: 120,
		TotalSeconds:    1440,
		Dirty:           true,
		UpdatedAt:       refTime,
	}
}

func TestProgressUpsertInsertRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	want := fullProgress("https://animego/a1")

	if err := st.Progress.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if want.ID == 0 {
		t.Fatal("upsert did not set ID")
	}

	got, err := st.Progress.GetByID(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if !reflect.DeepEqual(*got, *want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestProgressUpsertOverwriteSameSource(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	p := fullProgress("https://animego/a1")
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	firstID := p.ID

	p.CurrentEpisode = "7"
	p.VideoDub = strPtr("dub")
	p.AudioDub = nil
	p.ProgressSeconds = 700
	p.TotalSeconds = 1500
	p.Dirty = false
	p.UpdatedAt = refTime.Add(24 * time.Hour)
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("overwrite upsert: %v", err)
	}
	if p.ID != firstID {
		t.Errorf("upsert conflict created new row: id %d, want %d", p.ID, firstID)
	}

	got, err := st.Progress.GetByID(context.Background(), firstID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.CurrentEpisode != "7" || deref(got.VideoDub) != "dub" || got.AudioDub != nil ||
		got.ProgressSeconds != 700 || got.TotalSeconds != 1500 || got.Dirty {
		t.Errorf("overwrite not persisted: %+v", *got)
	}

	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM anime_progress`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1 (upsert must not duplicate)", n)
	}
}

func TestProgressGetByIDNotFound(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, err := st.Progress.GetByID(context.Background(), 777)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound", err)
	}
}

func TestProgressGetByShikimoriID(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	shiki := int64(4242)
	older := fullProgress("https://animego/old")
	older.ShikimoriID = &shiki
	older.UpdatedAt = refTime
	newer := fullProgress("https://yummy/new")
	newer.ShikimoriID = &shiki
	newer.UpdatedAt = refTime.Add(time.Hour)

	for _, p := range []*AnimeProgress{older, newer} {
		if err := st.Progress.Upsert(context.Background(), p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := st.Progress.GetByShikimoriID(context.Background(), shiki)
	if err != nil {
		t.Fatalf("get by shikimori id: %v", err)
	}
	if got.SourceURL != newer.SourceURL {
		t.Errorf("got %q, want the most recently updated row %q",
			got.SourceURL, newer.SourceURL)
	}

	_, err = st.Progress.GetByShikimoriID(context.Background(), 1)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("missing shikimori id error = %v, want contracts.ErrNotFound", err)
	}
}

func TestProgressListHistory(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	seed := []AnimeProgress{
		{Title: "a", SourceID: "s", SourceURL: "u1", CurrentEpisode: "1",
			ShikimoriStatus: "watching", UpdatedAt: refTime},
		{Title: "b", SourceID: "s", SourceURL: "u2", CurrentEpisode: "2",
			ShikimoriStatus: "completed", UpdatedAt: refTime.Add(1 * time.Hour)},
		{Title: "c", SourceID: "s", SourceURL: "u3", CurrentEpisode: "3",
			ShikimoriStatus: "watching", UpdatedAt: refTime.Add(2 * time.Hour)},
		{Title: "d", SourceID: "s", SourceURL: "u4", CurrentEpisode: "4",
			ShikimoriStatus: "planned", UpdatedAt: refTime.Add(3 * time.Hour)},
	}
	for i := range seed {
		if err := st.Progress.Upsert(context.Background(), &seed[i]); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	cases := []struct {
		name   string
		status string
		limit  int
		offset int
		want   []string // titles, newest first
	}{
		{"all unlimited", "", 0, 0, []string{"d", "c", "b", "a"}},
		{"limit two", "", 2, 0, []string{"d", "c"}},
		{"limit with offset", "", 2, 1, []string{"c", "b"}},
		{"offset past end", "", 2, 10, nil},
		{"status filter", "watching", 0, 0, []string{"c", "a"}},
		{"status filter empty result", "dropped", 0, 0, nil},
		{"status with limit", "watching", 1, 0, []string{"c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.Progress.ListHistory(context.Background(), tc.status, tc.limit, tc.offset)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var titles []string
			for _, p := range got {
				titles = append(titles, p.Title)
			}
			if len(titles) != len(tc.want) {
				t.Fatalf("titles = %v, want %v", titles, tc.want)
			}
			for i := range titles {
				if titles[i] != tc.want[i] {
					t.Fatalf("titles = %v, want %v", titles, tc.want)
				}
			}
		})
	}
}

func TestProgressListHistoryEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	got, err := st.Progress.ListHistory(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("list on empty table: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %d, want 0", len(got))
	}
}

func TestProgressUpdatePlayback(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	p := fullProgress("https://animego/a1")
	p.VideoDub = strPtr("old")
	p.AudioDub = strPtr("old")
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.Progress.UpdatePlayback(context.Background(), p.ID,
		"9", "newdub", "", 333, 999); err != nil {
		t.Fatalf("update playback: %v", err)
	}

	got, err := st.Progress.GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.CurrentEpisode != "9" {
		t.Errorf("episode = %q, want 9", got.CurrentEpisode)
	}
	if got.VideoDub == nil || *got.VideoDub != "newdub" {
		t.Errorf("video dub = %v, want newdub", got.VideoDub)
	}
	if got.AudioDub != nil {
		t.Errorf("audio dub = %v, want nil (empty string maps to NULL)", *got.AudioDub)
	}
	if got.ProgressSeconds != 333 || got.TotalSeconds != 999 {
		t.Errorf("seconds = %d/%d, want 333/999", got.ProgressSeconds, got.TotalSeconds)
	}
	if !got.UpdatedAt.After(refTime) {
		t.Errorf("updated_at = %v, want bumped past seed %v", got.UpdatedAt, refTime)
	}
}

func TestProgressUpdatePlaybackMissing(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	err := st.Progress.UpdatePlayback(context.Background(), 55, "1", "", "", 0, 0)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound", err)
	}
}

func TestProgressSetRateID(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	p := fullProgress("https://animego/a1")
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.Progress.SetRateID(context.Background(), p.ID, 987); err != nil {
		t.Fatalf("set rate id: %v", err)
	}
	got, err := st.Progress.GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 987 {
		t.Errorf("rate id = %v, want 987", got.ShikimoriRateID)
	}

	err = st.Progress.SetRateID(context.Background(), 55, 1)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("missing id error = %v, want contracts.ErrNotFound", err)
	}
}

func TestProgressDirtyToggle(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	p := fullProgress("https://animego/a1")
	p.Dirty = false
	if err := st.Progress.Upsert(context.Background(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.Progress.MarkDirty(context.Background(), p.ID); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	got, err := st.Progress.GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.Dirty {
		t.Error("dirty flag not set")
	}

	if err := st.Progress.ClearDirty(context.Background(), p.ID); err != nil {
		t.Fatalf("clear dirty: %v", err)
	}
	got, err = st.Progress.GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Dirty {
		t.Error("dirty flag not cleared")
	}

	if err := st.Progress.MarkDirty(context.Background(), 55); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("missing id error = %v, want contracts.ErrNotFound", err)
	}
}

func TestProgressDeleteCascades(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	p := fullProgress("https://animego/a1")
	if err := st.Progress.Upsert(ctx, p); err != nil {
		t.Fatalf("seed anime: %v", err)
	}

	for _, ep := range []string{"1", "2"} {
		if err := st.Episodes.Upsert(ctx, &EpisodeProgress{
			AnimeID: p.ID, Episode: ep, PositionSec: 10, DurationSec: 100,
		}); err != nil {
			t.Fatalf("seed episode %s: %v", ep, err)
		}
	}
	if err := st.Sources.ReplaceForAnime(ctx, p.ID, []AnimeSource{
		{SourceID: "animego", SourceURL: "https://animego/a1"},
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	if err := st.Progress.Delete(ctx, p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := st.Progress.GetByID(ctx, p.ID); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	eps, err := st.Episodes.ListByAnime(ctx, p.ID)
	if err != nil {
		t.Fatalf("list episodes after cascade: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("episodes after cascade = %d, want 0", len(eps))
	}
	srcs, err := st.Sources.ListByAnime(ctx, p.ID)
	if err != nil {
		t.Fatalf("list sources after cascade: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("sources after cascade = %d, want 0", len(srcs))
	}
}

func TestProgressDeleteMissing(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	if err := st.Progress.Delete(context.Background(), 55); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound", err)
	}
}
