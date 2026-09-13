package cfbrowser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T, ttl time.Duration) *ClearanceStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfstore.json")
	return NewClearanceStore(path, ttl)
}

func sampleClearance() Clearance {
	return Clearance{
		Cookies: []Cookie{
			{Name: "cf_clearance", Value: "tok-123", Domain: "animego.one", Path: "/"},
			{Name: "__cfruid", Value: "abc", Domain: ".animego.one", Path: "/"},
		},
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64) Chrome/146.0.0.0",
		AcceptLanguage: "ru-RU,ru;q=0.9",
		Obtained:       time.Now(),
	}
}

func TestClearanceRoundTrip(t *testing.T) {
	store := newTestStore(t, 30*time.Minute)
	c := sampleClearance()
	if err := store.Put("animego.one", c); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Fresh instance reads persisted state (atomic-write durability).
	store2 := NewClearanceStore(store.path, 30*time.Minute)
	got, ok := store2.Get("animego.one")
	if !ok {
		t.Fatal("expected persisted clearance")
	}
	if got.UserAgent != c.UserAgent || len(got.Cookies) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Cookies[0].Name != "cf_clearance" || got.Cookies[0].Value != "tok-123" {
		t.Errorf("cookie mismatch: %+v", got.Cookies[0])
	}
}

func TestClearanceTTLExpiry(t *testing.T) {
	store := newTestStore(t, 30*time.Minute)
	now := time.Now()
	store.now = func() time.Time { return now }

	if err := store.Put("animego.one", sampleClearance()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("animego.one"); !ok {
		t.Fatal("fresh clearance must be valid")
	}

	// Advance past the TTL: expired.
	store.now = func() time.Time { return now.Add(31 * time.Minute) }
	if _, ok := store.Get("animego.one"); ok {
		t.Fatal("clearance must expire after the TTL")
	}

	// Refresh-on-403 path: a fresh Put revives the entry.
	revived := sampleClearance()
	revived.Obtained = now.Add(31 * time.Minute)
	if err := store.Put("animego.one", revived); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Get("animego.one"); !ok || got.Cookies[0].Value != "tok-123" {
		t.Fatalf("revived clearance missing: %v %v", got, ok)
	}
}

func TestClearanceDeleteAndClear(t *testing.T) {
	store := newTestStore(t, time.Minute)
	if err := store.Put("a.one", sampleClearance()); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("b.two", sampleClearance()); err != nil {
		t.Fatal(err)
	}
	store.Delete("a.one")
	if _, ok := store.Get("a.one"); ok {
		t.Fatal("deleted host must not resolve")
	}
	if _, ok := store.Get("b.two"); !ok {
		t.Fatal("other host must survive Delete")
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := store.Get("b.two"); ok {
		t.Fatal("clear must wipe every host")
	}
}

func TestClearanceAtomicWriteNoTmpResidue(t *testing.T) {
	dir := t.TempDir()
	store := NewClearanceStore(filepath.Join(dir, "cfstore.json"), time.Minute)
	for range 3 {
		if err := store.Put("animego.one", sampleClearance()); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cfstore.json" {
			t.Errorf("atomic writes must leave no residue, found %q", e.Name())
		}
	}
	// Overwrite keeps the file valid JSON (write-temp + rename).
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]Clearance
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("store must stay valid JSON across overwrites: %v", err)
	}
}

func TestClearanceJSONShape(t *testing.T) {
	store := newTestStore(t, time.Minute)
	c := sampleClearance()
	if err := store.Put("animego.one", c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("store must be valid JSON: %v", err)
	}
	entry, ok := doc["animego.one"].(map[string]any)
	if !ok {
		t.Fatalf("store must be map[host]clearance, got %T", doc["animego.one"])
	}
	if entry["user_agent"] != c.UserAgent {
		t.Errorf("user_agent field mismatch: %v", entry["user_agent"])
	}
}
