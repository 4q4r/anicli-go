package storage

import (
	"context"
	"math"
	"testing"
)

func TestSkipPredictionSaveGetRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	skipData := map[string][2]float64{
		"op":    {12.5, 103.0},
		"ed":    {1290.0, 1380.5},
		"recap": {0.0, 30.0},
	}
	if err := st.Skips.Save(ctx, 4242, 5.0, skipData, 1440.0); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := st.Skips.Get(ctx, 4242, 5.0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != len(skipData) {
		t.Fatalf("rows = %d, want %d", len(got), len(skipData))
	}
	byType := map[string]PredictedSkipTime{}
	for _, s := range got {
		byType[s.SkipType] = s
	}
	for skipType, times := range skipData {
		s, ok := byType[skipType]
		if !ok {
			t.Fatalf("skip type %q missing", skipType)
		}
		if s.ShikimoriID != 4242 || s.EpisodeNum != 5.0 {
			t.Errorf("%q keys = %d/%v, want 4242/5", skipType, s.ShikimoriID, s.EpisodeNum)
		}
		if s.StartTime != times[0] || s.EndTime != times[1] {
			t.Errorf("%q times = %v-%v, want %v-%v", skipType, s.StartTime, s.EndTime, times[0], times[1])
		}
		if s.EpisodeLength != 1440.0 {
			t.Errorf("%q episode length = %v, want 1440", skipType, s.EpisodeLength)
		}
		if s.CreatedAt.IsZero() {
			t.Errorf("%q created_at not set", skipType)
		}
	}
}

func TestSkipPredictionSaveOverwrites(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	if err := st.Skips.Save(ctx, 1, 1.0, map[string][2]float64{
		"op": {10, 90},
	}, 1400); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// Re-analyzed episode must update timestamps, not duplicate (python FIX).
	if err := st.Skips.Save(ctx, 1, 1.0, map[string][2]float64{
		"op": {20, 95},
	}, 1450); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := st.Skips.Get(ctx, 1, 1.0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].StartTime != 20 || got[0].EndTime != 95 || got[0].EpisodeLength != 1450 {
		t.Errorf("overwrite not persisted: %+v", got[0])
	}
}

func TestSkipPredictionGetEmpty(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	if err := st.Skips.Save(context.Background(), 1, 1.0,
		map[string][2]float64{"op": {0, 1}}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Different episode of same anime: no rows.
	got, err := st.Skips.Get(context.Background(), 1, 2.0)
	if err != nil {
		t.Fatalf("get other episode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %d, want 0", len(got))
	}

	// Whole table empty.
	got, err = st.Skips.Get(context.Background(), 2, 1.0)
	if err != nil {
		t.Fatalf("get other anime: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows = %d, want 0", len(got))
	}
}

func TestSkipPredictionClearAll(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	for _, shiki := range []int64{1, 2} {
		if err := st.Skips.Save(ctx, shiki, 1.0,
			map[string][2]float64{"op": {0, 1}, "ed": {2, 3}}, 1); err != nil {
			t.Fatalf("seed %d: %v", shiki, err)
		}
	}

	n, err := st.Skips.ClearAll(ctx)
	if err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if n != 4 {
		t.Errorf("deleted = %d, want 4", n)
	}

	got, err := st.Skips.Get(ctx, 1, 1.0)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rows after clear = %d, want 0", len(got))
	}

	// Clearing an already-empty table reports zero, not an error.
	n, err = st.Skips.ClearAll(ctx)
	if err != nil {
		t.Fatalf("clear empty: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted on empty = %d, want 0", n)
	}
}

func TestSkipPredictionFloatPrecision(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()
	const ep = 5.25
	if err := st.Skips.Save(ctx, 9, ep, map[string][2]float64{
		"op": {math.Nextafter(0, 1), 90.125},
	}, 0); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := st.Skips.Get(ctx, 9, ep)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].StartTime == 0 || got[0].EndTime != 90.125 {
		t.Errorf("float fidelity lost: %+v", got[0])
	}
}
