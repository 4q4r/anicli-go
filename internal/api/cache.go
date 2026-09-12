// Package api is the HTTP API face of anicli-go (PR10): a chi router
// exposing the ported anicli-py api_server.py contract — token auth
// with refresh rotation, the library/history/progress CRUD, the
// stream-orchestrator resolve endpoint (normalized variants, no media
// proxying), the personalized home feed with a paginated hero gallery,
// Shikimori-only autocomplete search and the releases calendar.
package api

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// entry is one cached value with its expiry.
type entry struct {
	value   map[string]any
	expires time.Time
}

// TTLCache is the in-process replacement for the Python redis cache
// (KEEP ruling: no redis in the Go build): a map guarded by an
// RWMutex, entries expiring after their TTL, and a janitor goroutine
// that sweeps expired entries until the context is cancelled.
//
// Get returns a deep copy (JSON round-trip): callers mutate returned
// payloads (adding "cached": true), mirroring the Python json.loads
// per-read semantics.
type TTLCache struct {
	mu      sync.RWMutex
	entries map[string]entry
	// now is the clock; swappable for deterministic tests.
	now func() time.Time
}

// NewTTLCache builds the cache and starts the janitor (sweep every
// interval until ctx is done).
func NewTTLCache(ctx context.Context, sweepInterval time.Duration) *TTLCache {
	c := &TTLCache{
		entries: map[string]entry{},
		now:     time.Now,
	}
	go c.janitor(ctx, sweepInterval)
	return c
}

// newTTLCacheWithClock builds a cache with an injected clock and no
// janitor (tests drive expiry explicitly).
func newTTLCacheWithClock(now func() time.Time) *TTLCache {
	return &TTLCache{entries: map[string]entry{}, now: now}
}

// janitor deletes expired entries periodically.
func (c *TTLCache) janitor(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}

// sweep removes every expired entry.
func (c *TTLCache) sweep() {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, key)
		}
	}
}

// Set stores payload under key until ttl elapses.
func (c *TTLCache) Set(key string, payload map[string]any, ttl time.Duration) {
	if payload == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{value: payload, expires: c.now().Add(ttl)}
}

// Get returns a deep copy of the payload stored under key, or nil when
// absent or expired (expired entries are deleted opportunistically).
func (c *TTLCache) Get(key string) map[string]any {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if c.now().After(e.expires) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil
	}
	return deepCopyMap(e.value)
}

// Len reports the number of live entries (test hook).
func (c *TTLCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// deepCopyMap clones v through JSON so nested maps/slices are not
// shared with the cache (python parity: every redis read is a fresh
// json.loads).
func deepCopyMap(v map[string]any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}
