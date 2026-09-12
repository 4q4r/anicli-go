package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

type detailsPayload struct {
	Name   string   `json:"name"`
	Genres []string `json:"genres"`
}

func TestDetailsCachePayloadHelpers(t *testing.T) {
	t.Parallel()

	d := &AnimeDetails{}
	in := detailsPayload{Name: "Bebop", Genres: []string{"sci-fi", "action"}}
	if err := d.SetPayload(in); err != nil {
		t.Fatalf("set payload: %v", err)
	}
	if d.PayloadJSON == "" {
		t.Fatal("payload JSON not stored")
	}

	var out detailsPayload
	if err := d.DecodePayload(&out); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if out.Name != in.Name || len(out.Genres) != 2 {
		t.Errorf("payload round-trip = %+v, want %+v", out, in)
	}

	d.PayloadJSON = "{not json"
	if err := d.DecodePayload(&out); err == nil {
		t.Error("decode malformed payload: want error, got nil")
	}
}

func TestDetailsCacheUpsertRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	d := &AnimeDetails{
		AnimeID:           4242,
		ImmutableCachedAt: refTime,
		MutableUpdatedAt:  refTime,
		UpdatedAt:         refTime,
	}
	if err := d.SetPayload(detailsPayload{Name: "x"}); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := st.Details.Upsert(ctx, d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if d.ID == 0 {
		t.Fatal("upsert did not set ID")
	}

	got, err := st.Details.Get(ctx, 4242)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if *got != *d {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", *got, *d)
	}
}

func TestDetailsCacheUpsertKeepsImmutableTimestamp(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	first := &AnimeDetails{
		AnimeID:           4242,
		ImmutableCachedAt: refTime,
		MutableUpdatedAt:  refTime,
		UpdatedAt:         refTime,
	}
	if err := st.Details.Upsert(ctx, first); err != nil {
		t.Fatalf("seed: %v", err)
	}

	later := refTime.Add(48 * time.Hour)
	second := &AnimeDetails{
		AnimeID:           4242,
		ImmutableCachedAt: later, // caller cannot move the immutable stamp
		MutableUpdatedAt:  later,
		UpdatedAt:         later,
	}
	if err := st.Details.Upsert(ctx, second); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := st.Details.Get(ctx, 4242)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("re-upsert created a second row: id %d, want %d", got.ID, first.ID)
	}
	if !got.ImmutableCachedAt.Equal(refTime) {
		t.Errorf("immutable_cached_at = %v, want original %v", got.ImmutableCachedAt, refTime)
	}
	if !got.MutableUpdatedAt.Equal(later) || !got.UpdatedAt.Equal(later) {
		t.Errorf("mutable stamps not refreshed: %+v", got)
	}
}

func TestDetailsCacheGetNotFound(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	_, err := st.Details.Get(context.Background(), 7)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound", err)
	}
}

func TestDetailsCachePurgeOlder(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []AnimeDetails{
		{AnimeID: 1, ImmutableCachedAt: old, MutableUpdatedAt: old, UpdatedAt: old},
		{AnimeID: 2, ImmutableCachedAt: fresh, MutableUpdatedAt: fresh, UpdatedAt: fresh},
		{AnimeID: 3, ImmutableCachedAt: old, MutableUpdatedAt: fresh, UpdatedAt: fresh},
	}
	for i := range entries {
		if err := st.Details.Upsert(ctx, &entries[i]); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Cutoff on immutable_cached_at: entries 1 and 3 are older, 2 survives.
	n, err := st.Details.PurgeOlder(ctx, fresh)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("purged = %d, want 2", n)
	}
	if _, err := st.Details.Get(ctx, 2); err != nil {
		t.Errorf("fresh entry purged: %v", err)
	}
	if _, err := st.Details.Get(ctx, 1); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("old entry survived: %v", err)
	}

	n, err = st.Details.PurgeOlder(ctx, fresh)
	if err != nil {
		t.Fatalf("purge empty: %v", err)
	}
	if n != 0 {
		t.Errorf("purged on empty = %d, want 0", n)
	}
}
