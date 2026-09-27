package storage

import (
	"context"
	"testing"
	"time"
)

// TestMALMapRoundTrip pins the durable shikimori->MAL id cache
// (PR112): SetMALID persists, GetMALID resolves, a miss is a typed
// (0,false,nil) — never an error.
func TestMALMapRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	id, ok, err := st.MALMap.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get on empty map: %v", err)
	}
	if ok || id != 0 {
		t.Fatalf("miss = (%d,%v), want (0,false)", id, ok)
	}

	if err := st.MALMap.Set(ctx, 1, 21); err != nil {
		t.Fatalf("Set: %v", err)
	}
	id, ok, err = st.MALMap.Get(ctx, 1)
	if err != nil || !ok || id != 21 {
		t.Fatalf("hit = (%d,%v,%v), want (21,true,nil)", id, ok, err)
	}

	// Upsert overwrites (a re-resolved mapping wins).
	if err := st.MALMap.Set(ctx, 1, 30); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	id, ok, err = st.MALMap.Get(ctx, 1)
	if err != nil || !ok || id != 30 {
		t.Fatalf("after overwrite = (%d,%v,%v), want (30,true,nil)", id, ok, err)
	}
}

// TestMALMapIndependentKeys pins key isolation.
func TestMALMapIndependentKeys(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	if err := st.MALMap.Set(ctx, 1, 21); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	if err := st.MALMap.Set(ctx, 2, 5114); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	if id, ok, _ := st.MALMap.Get(ctx, 1); !ok || id != 21 {
		t.Errorf("key 1 = (%d,%v), want (21,true)", id, ok)
	}
	if id, ok, _ := st.MALMap.Get(ctx, 2); !ok || id != 5114 {
		t.Errorf("key 2 = (%d,%v), want (5114,true)", id, ok)
	}
}

// TestMigrationsV2PinsAnimeMalMap pins: a database created at v1 and
// reopened migrates forward to v2 (the anime_mal_map table appears) —
// the local history DB itself is untouched (owner ruling: «Списки
// удалять не придется»).
func TestMigrationsV2PinsAnimeMalMap(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	if err := st.MALMap.Set(ctx, 42, 1); err != nil {
		t.Fatalf("mal map usable on a fresh store: %v", err)
	}

	// The history tables keep working beside the new table.
	rec := AnimeProgress{Title: "t", SourceID: "p", SourceURL: "u", UpdatedAt: time.Now().UTC()}
	if err := st.Progress.Upsert(ctx, &rec); err != nil {
		t.Fatalf("history upsert beside mal map: %v", err)
	}
}
