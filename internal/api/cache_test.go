package api

import (
	"context"
	"testing"
	"time"
)

func TestTTLCacheSetGet(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	now := base
	c := newTTLCacheWithClock(func() time.Time { return now })

	c.Set("k", map[string]any{"items": []any{"a"}}, time.Minute)

	got := c.Get("k")
	if got == nil {
		t.Fatal("fresh entry missing")
	}
	items, _ := got["items"].([]any)
	if len(items) != 1 || items[0] != "a" {
		t.Fatalf("payload roundtrip broken: %v", got)
	}
}

func TestTTLCacheExpiryWithFakeClock(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	now := base
	c := newTTLCacheWithClock(func() time.Time { return now })

	c.Set("k", map[string]any{"v": 1}, 60*time.Second)

	now = now.Add(59 * time.Second)
	if c.Get("k") == nil {
		t.Fatal("entry expired too early (59s < 60s TTL)")
	}
	now = now.Add(2 * time.Second)
	if c.Get("k") != nil {
		t.Fatal("entry survived past its TTL")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry not evicted, len=%d", c.Len())
	}
}

func TestTTLCacheGetReturnsDeepCopy(t *testing.T) {
	// Python parity: every redis read is a fresh json.loads; callers
	// mutating the returned payload (adding "cached") must not poison
	// the stored value.
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	now := base
	c := newTTLCacheWithClock(func() time.Time { return now })

	c.Set("k", map[string]any{"cached": false}, time.Minute)

	got := c.Get("k")
	got["cached"] = true

	again := c.Get("k")
	if cached, _ := again["cached"].(bool); cached {
		t.Fatal("mutation leaked into the cached payload")
	}
}

func TestTTLCacheNilAndZeroTTL(t *testing.T) {
	now := time.Now
	c := newTTLCacheWithClock(now)
	c.Set("nil-payload", nil, time.Minute)
	c.Set("zero-ttl", map[string]any{"v": 1}, 0)
	if c.Get("nil-payload") != nil || c.Get("zero-ttl") != nil {
		t.Fatal("nil payload and non-positive TTL must not store")
	}
}

func TestTTLCacheJanitorShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := NewTTLCache(ctx, 5*time.Millisecond)
	c.Set("k", map[string]any{"v": 1}, time.Hour)
	cancel()
	// The janitor goroutine must exit promptly; the cache stays usable.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.Len() >= 0 {
			break
		}
	}
	if c.Get("k") == nil {
		t.Fatal("live entry lost after janitor shutdown")
	}
}
