package netclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelRespectsLimit(t *testing.T) {
	t.Parallel()

	var running, maxRunning atomic.Int32
	items := make([]int, 12)
	for i := range items {
		items[i] = i
	}

	var mu sync.Mutex // guards processed
	processed := make(map[int]bool)

	err := Parallel(context.Background(), items, 4, func(_ context.Context, n int) error {
		cur := running.Add(1)
		for {
			old := maxRunning.Load()
			if cur <= old || maxRunning.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)

		mu.Lock()
		processed[n] = true
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	if got := maxRunning.Load(); got > 4 {
		t.Errorf("max concurrent = %d, want <= 4", got)
	}
	if len(processed) != 12 {
		t.Errorf("processed %d items, want 12", len(processed))
	}
}

func TestParallelPropagatesError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("provider exploded")
	ran := make(chan int, 8)
	items := []int{1, 2, 3, 4, 5, 6, 7, 8}

	err := Parallel(context.Background(), items, 2, func(_ context.Context, n int) error {
		ran <- n
		if n == 3 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Parallel err = %v, want sentinel", err)
	}
	close(ran)
	if count := len(ran); count == 0 {
		t.Error("no items ran")
	}
}

func TestParallelHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	items := make([]int, 100)

	// With limit 1 the first item cancels the group mid-flight; every
	// later item must fail fast on the canceled context instead of doing
	// work.
	var work atomic.Int32
	err := Parallel(ctx, items, 1, func(inner context.Context, _ int) error {
		select {
		case <-inner.Done():
			return inner.Err()
		default:
		}
		if work.Add(1) == 1 {
			cancel()
			return nil
		}
		t.Error("work executed after group cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Parallel err = %v, want context.Canceled", err)
	}
	if got := work.Load(); got != 1 {
		t.Errorf("completed work items = %d, want 1 (everything after cancel must fail fast)", got)
	}
}

func TestParallelUnboundedWhenLimitNonPositive(t *testing.T) {
	t.Parallel()

	items := make([]int, 16)
	var ran atomic.Int32
	if err := Parallel(context.Background(), items, 0, func(_ context.Context, _ int) error {
		ran.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	if got := ran.Load(); got != 16 {
		t.Errorf("ran %d items with limit 0, want all 16 (errgroup unbounded)", got)
	}
}

func TestParallelEmptyItems(t *testing.T) {
	t.Parallel()

	if err := Parallel(context.Background(), nil, 4, func(_ context.Context, n int) error {
		t.Errorf("fn must not run for empty items (got %d)", n)
		return nil
	}); err != nil {
		t.Fatalf("Parallel on empty items: %v", err)
	}
}
