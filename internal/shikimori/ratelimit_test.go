package shikimori

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic time source: sleep advances it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

// newTestLimiter wires a limiter to the fake clock and records the delays
// the sleeper was asked to absorb.
func newTestLimiter(t *testing.T) (*limiter, *delayLog) {
	t.Helper()
	clock := newFakeClock()
	log := &delayLog{}
	l := newLimiter()
	l.now = clock.Now
	l.sleep = func(_ context.Context, d time.Duration) error {
		log.record(d)
		return clock.Sleep(context.Background(), d)
	}
	return l, log
}

type delayLog struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (l *delayLog) record(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.delays = append(l.delays, d)
}

func (l *delayLog) snapshot() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.delays...)
}

func TestLimiterFirstRequestAdmittedImmediately(t *testing.T) {
	t.Parallel()

	l, log := newTestLimiter(t)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if delays := log.snapshot(); len(delays) != 0 {
		t.Errorf("first Wait slept %v, want no delay", delays)
	}
}

func TestLimiterEnforcesOnePerSecondSpacing(t *testing.T) {
	t.Parallel()

	l, log := newTestLimiter(t)
	ctx := context.Background()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}
	if delays := log.snapshot(); len(delays) != 1 || delays[0] != time.Second {
		t.Errorf("second Wait delays = %v, want exactly [1s]", delays)
	}
}

func TestLimiterBucketFivePerMinute(t *testing.T) {
	t.Parallel()

	l, log := newTestLimiter(t)
	ctx := context.Background()
	for i := range bucketSize {
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("Wait %d: %v", i+1, err)
		}
	}
	// The window is full: the sixth request must wait until the oldest
	// admission leaves the 60s window. Five admissions span 4s of spacing
	// (t=0..4s), so the remaining wait past the last admission is
	// window[0]+60s - (last admission + 1s spacing floor).
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 6: %v", err)
	}
	delays := log.snapshot()
	want := []time.Duration{
		time.Second,                // admission 2
		time.Second,                // admission 3
		time.Second,                // admission 4
		time.Second,                // admission 5
		windowSize - 4*time.Second, // admission 6: t=60s minus current t=4s
	}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delays[%d] = %v, want %v (full: %v)", i, delays[i], want[i], delays)
		}
	}
}

func TestLimiterPenalizeExtendsNextAdmission(t *testing.T) {
	t.Parallel()

	l, log := newTestLimiter(t)
	ctx := context.Background()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	l.Penalize(30 * time.Second)
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}
	delays := log.snapshot()
	if len(delays) == 0 || delays[len(delays)-1] < 30*time.Second {
		t.Errorf("post-penalty wait = %v, want >= 30s", delays)
	}
}

func TestLimiterPenalizeKeepsMaxDeadline(t *testing.T) {
	t.Parallel()

	l, log := newTestLimiter(t)
	ctx := context.Background()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	l.Penalize(10 * time.Second)
	l.Penalize(2 * time.Second) // must not shorten the 10s penalty
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}
	delays := log.snapshot()
	if len(delays) == 0 || delays[len(delays)-1] < 10*time.Second {
		t.Errorf("post-penalty wait = %v, want >= 10s", delays)
	}
}

func TestLimiterWaitHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := newLimiter()
	l.now = clock.Now
	l.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("cancelled Wait returned nil, want context error")
	}
}

func TestLimiterConcurrentWaitsSerialize(t *testing.T) {
	t.Parallel()

	l, _ := newTestLimiter(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, bucketSize)
	for i := range bucketSize {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = l.Wait(ctx)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d Wait: %v", i, err)
		}
	}

	l.mu.Lock()
	stamps := append([]time.Time(nil), l.window...)
	l.mu.Unlock()

	if len(stamps) != bucketSize {
		t.Fatalf("admitted %d, want %d", len(stamps), bucketSize)
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i].Sub(stamps[i-1]) < minInterval {
			t.Errorf("admissions %d,%d only %v apart, want >= %v",
				i-1, i, stamps[i].Sub(stamps[i-1]), minInterval)
		}
	}
}
