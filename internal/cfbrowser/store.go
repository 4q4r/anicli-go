package cfbrowser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultClearanceTTL bounds how long a harvested clearance is
// replayed before the next 403 forces a fresh solve.
const DefaultClearanceTTL = 30 * time.Minute

// ClearanceStore persists per-host clearances as map[host]Clearance
// with a TTL, written atomically (temp file + rename) and guarded by
// an RWMutex. It lives at <DataDir>/cfstore.json.
type ClearanceStore struct {
	path string
	ttl  time.Duration

	mu    sync.RWMutex
	hosts map[string]Clearance

	// now overrides the clock in tests; default time.Now.
	now func() time.Time
}

// NewClearanceStore opens (or creates) the store at path. ttl <= 0
// selects DefaultClearanceTTL. A missing or unreadable file starts
// from an empty map — a corrupt store never blocks solving, the next
// Put overwrites it.
func NewClearanceStore(path string, ttl time.Duration) *ClearanceStore {
	if ttl <= 0 {
		ttl = DefaultClearanceTTL
	}
	s := &ClearanceStore{
		path:  path,
		ttl:   ttl,
		hosts: make(map[string]Clearance),
		now:   time.Now,
	}
	if raw, err := os.ReadFile(path); err == nil {
		var hosts map[string]Clearance
		if json.Unmarshal(raw, &hosts) == nil && hosts != nil {
			s.hosts = hosts
		}
	}
	return s
}

// Get returns the live clearance for host (expired entries are
// reported missing; the lazy tombstone is dropped on the next Put).
func (s *ClearanceStore) Get(host string) (Clearance, bool) {
	s.mu.RLock()
	c, ok := s.hosts[host]
	s.mu.RUnlock()
	if !ok {
		return Clearance{}, false
	}
	if s.now().Sub(c.Obtained) > s.ttl {
		return Clearance{}, false
	}
	return c, true
}

// Put stores the clearance for host and persists atomically.
func (s *ClearanceStore) Put(host string, c Clearance) error {
	s.mu.Lock()
	s.hosts[host] = c
	s.mu.Unlock()
	return s.persist()
}

// Delete invalidates one host (the refresh-on-403 path before a
// re-solve) and persists.
func (s *ClearanceStore) Delete(host string) error {
	s.mu.Lock()
	delete(s.hosts, host)
	s.mu.Unlock()
	return s.persist()
}

// Clear wipes every host and persists.
func (s *ClearanceStore) Clear() error {
	s.mu.Lock()
	s.hosts = make(map[string]Clearance)
	s.mu.Unlock()
	return s.persist()
}

// persist writes the store via a same-directory temp file + rename so
// a crash mid-write never leaves a partial cfstore.json.
func (s *ClearanceStore) persist() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.hosts, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("cfbrowser: encode clearance store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("cfbrowser: create store dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { //nolint:gosec // cookies are secrets: owner-only
		return fmt.Errorf("cfbrowser: write clearance temp store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cfbrowser: swap clearance store: %w", err)
	}
	return nil
}
