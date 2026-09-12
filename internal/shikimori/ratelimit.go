package shikimori

import (
	"context"
	"sync"
	"time"
)

// Rate limit budget, task-fixed (PR8 spec): at most one request per second
// and at most five requests per rolling 60-second window — stricter than
// the frozen Python original, which only enforced a 0.25s spacing
// (anicli-py anicli/core/shikimori.py _throttle). Shikimori API etiquette
// demands conservative client pacing; both constraints hold simultaneously.
const (
	// minInterval is the mandatory spacing between two admissions.
	minInterval = 1 * time.Second
	// bucketSize is the maximum admissions inside one window.
	bucketSize = 5
	// windowSize is the sliding-window length.
	windowSize = 60 * time.Second
)

// limiter is the pre-request throttle ported from the Python _throttle
// shape (wait before every outgoing request), extended with the PR8
// budget above and a Retry-After penalty channel.
//
// Retry-After handling is layered: the underlying netclient already honors
// integer Retry-After headers (up to 10s) while retrying 429 responses
// internally. When a 429 nevertheless surfaces to this package (netclient
// retries exhausted), Penalize spaces out subsequent requests by the same
// 10s cap so a hammered endpoint is not immediately re-hit.
type limiter struct {
	mu      sync.Mutex
	window  []time.Time // admission stamps inside the sliding window, ascending
	penalty time.Time   // earliest admission allowed after a Retry-After penalty

	// now and sleep are swapped by tests for determinism.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// newLimiter builds a limiter on the real clock.
func newLimiter() *limiter {
	return &limiter{
		now:   time.Now,
		sleep: clockSleep,
	}
}

// Wait blocks until one request is admitted under both the 1 rps spacing
// and the 5-per-minute sliding window, then records the admission.
func (l *limiter) Wait(ctx context.Context) error {
	for {
		delay := l.reserve()
		if delay <= 0 {
			return nil
		}
		if err := l.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// reserve computes the delay until the next admission and, when the
// admission is due now, records it. A zero return means admitted.
func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneLocked(now)

	next := now
	if n := len(l.window); n > 0 {
		// Spacing against the most recent admission.
		if at := l.window[n-1].Add(minInterval); at.After(next) {
			next = at
		}
		// Bucket: wait until the oldest admission ages out of the window.
		if n >= bucketSize {
			if at := l.window[0].Add(windowSize); at.After(next) {
				next = at
			}
		}
	}
	if l.penalty.After(next) {
		next = l.penalty
	}

	delay := next.Sub(now)
	if delay <= 0 {
		l.window = append(l.window, now)
		return 0
	}
	return delay
}

// Penalize pushes the earliest next admission out by d (never shortening
// an existing penalty). Called after a surfaced 429 with the netclient
// Retry-After cap.
func (l *limiter) Penalize(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if at := l.now().Add(d); at.After(l.penalty) {
		l.penalty = at
	}
}

// pruneLocked drops admissions older than the window.
func (l *limiter) pruneLocked(now time.Time) {
	cutoff := now.Add(-windowSize)
	drop := 0
	for drop < len(l.window) && l.window[drop].Before(cutoff) {
		drop++
	}
	if drop > 0 {
		l.window = append(l.window[:0], l.window[drop:]...)
	}
}

// clockSleep sleeps for d unless ctx finishes first.
func clockSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
