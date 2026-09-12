package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func sampleRule(animeID int64) *AutoDownloadRule {
	return &AutoDownloadRule{
		AnimeID:           animeID,
		Enabled:           true,
		PreferredSourceID: strPtr("animego"),
		PreferredVideoDub: strPtr("subs"),
		PreferredQuality:  1080,
		MaxNewEpisodes:    3,
		Title:             strPtr("Ковбой Бибоп"),
		CreatedAt:         refTime,
		UpdatedAt:         refTime,
	}
}

func TestAutoRuleListEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	rules, err := st.AutoRules.List(context.Background())
	if err != nil {
		t.Fatalf("list on empty table: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("rows = %d, want 0", len(rules))
	}
}

func TestAutoRuleUpsertRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	r := sampleRule(animeID)
	if err := st.AutoRules.Upsert(ctx, r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("upsert did not set ID")
	}

	got, err := st.AutoRules.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].ID != r.ID || got[0].AnimeID != animeID || !got[0].Enabled ||
		deref(got[0].PreferredSourceID) != "animego" ||
		deref(got[0].PreferredVideoDub) != "subs" ||
		got[0].PreferredQuality != 1080 || got[0].MaxNewEpisodes != 3 ||
		deref(got[0].Title) != "Ковбой Бибоп" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(refTime) || !got[0].UpdatedAt.Equal(refTime) {
		t.Errorf("timestamps = %v/%v, want %v/%v",
			got[0].CreatedAt, got[0].UpdatedAt, refTime, refTime)
	}
}

func TestAutoRuleUpsertOverwrite(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	r := sampleRule(animeID)
	if err := st.AutoRules.Upsert(ctx, r); err != nil {
		t.Fatalf("seed: %v", err)
	}

	later := refTime.Add(time.Hour)
	r.Enabled = false
	r.PreferredQuality = 720
	r.MaxNewEpisodes = 1
	r.Title = nil
	r.UpdatedAt = later
	if err := st.AutoRules.Upsert(ctx, r); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	got, err := st.AutoRules.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1 (one rule per anime)", len(got))
	}
	if got[0].ID != r.ID {
		t.Errorf("re-upsert created a second row: id %d, want %d", got[0].ID, r.ID)
	}
	if got[0].Enabled || got[0].PreferredQuality != 720 || got[0].MaxNewEpisodes != 1 || got[0].Title != nil {
		t.Errorf("overwrite not persisted: %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(refTime) {
		t.Errorf("created_at = %v, want preserved %v", got[0].CreatedAt, refTime)
	}
	if !got[0].UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want %v", got[0].UpdatedAt, later)
	}
}

func TestAutoRuleDelete(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	animeID := seedAnime(t, st)

	r := sampleRule(animeID)
	if err := st.AutoRules.Upsert(ctx, r); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.AutoRules.Delete(ctx, r.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rules, err := st.AutoRules.List(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("rows after delete = %d, want 0", len(rules))
	}
	if err := st.AutoRules.Delete(ctx, r.ID); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("delete missing = %v, want contracts.ErrNotFound", err)
	}
}

func TestAutoRuleForeignKey(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	err := st.AutoRules.Upsert(context.Background(), sampleRule(555555))
	if err == nil {
		t.Fatal("upsert rule for missing anime: want FK error, got nil")
	}
}
