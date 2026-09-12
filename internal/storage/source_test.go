package storage

import (
	"context"
	"testing"
	"time"
)

func TestSourceReplaceAndList(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	// Replace into nothing: empty list.
	srcs, err := st.Sources.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("rows = %d, want 0", len(srcs))
	}

	resolved := refTime.Add(time.Hour)
	quality := int64(1080)
	first := []AnimeSource{
		{SourceID: "animego", SourceURL: "https://animego/a1", VideoDub: strPtr("subs"),
			AudioDub: strPtr("jap"), Quality: &quality, LastResolvedAt: &resolved},
		{SourceID: "kodik", SourceURL: "https://kodik/k/a1"},
	}
	if err := st.Sources.ReplaceForAnime(ctx, animeID, first); err != nil {
		t.Fatalf("replace: %v", err)
	}

	srcs, err = st.Sources.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("rows = %d, want 2", len(srcs))
	}
	bySource := map[string]AnimeSource{}
	for _, s := range srcs {
		bySource[s.SourceID] = s
	}
	got := bySource["animego"]
	if got.AnimeProgressID != animeID || got.SourceURL != "https://animego/a1" ||
		deref(got.VideoDub) != "subs" || deref(got.AudioDub) != "jap" ||
		got.Quality == nil || *got.Quality != 1080 {
		t.Errorf("animego row mismatch: %+v", got)
	}
	if got.LastResolvedAt == nil || !got.LastResolvedAt.Equal(resolved) {
		t.Errorf("last_resolved_at = %v, want %v", got.LastResolvedAt, resolved)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not set on insert")
	}
	k := bySource["kodik"]
	if k.VideoDub != nil || k.AudioDub != nil || k.Quality != nil || k.LastResolvedAt != nil {
		t.Errorf("kodik nullable fields = %+v, want all nil", k)
	}

	// Replace with a single source: old rows must be gone.
	if err := st.Sources.ReplaceForAnime(ctx, animeID, []AnimeSource{
		{SourceID: "yummy", SourceURL: "https://yummy/a1"},
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	srcs, err = st.Sources.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(srcs) != 1 || srcs[0].SourceID != "yummy" {
		t.Fatalf("rows after replace = %+v, want single yummy row", srcs)
	}

	// Replace with an empty list clears everything.
	if err := st.Sources.ReplaceForAnime(ctx, animeID, nil); err != nil {
		t.Fatalf("clear replace: %v", err)
	}
	srcs, err = st.Sources.ListByAnime(ctx, animeID)
	if err != nil {
		t.Fatalf("list after clear: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("rows after clear = %d, want 0", len(srcs))
	}
}

func TestSourceReplaceForMissingAnime(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	err := st.Sources.ReplaceForAnime(context.Background(), 987654, []AnimeSource{
		{SourceID: "animego", SourceURL: "https://animego/missing"},
	})
	if err == nil {
		t.Fatal("replace for missing anime: want FK error, got nil")
	}
}

func TestSourceListByAnimeEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	srcs, err := st.Sources.ListByAnime(context.Background(), 3)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("rows = %d, want 0", len(srcs))
	}
}
