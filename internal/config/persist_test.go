package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUpdateShikimoriPersistsTokensAndPreservesRest pins the settings
// persistence contract (PR25 E): the [shikimori] section is updated via a
// read-modify-write that preserves every other setting, the file stays
// loadable and secret-safe (0600), and the OAuth fields round-trip.
func TestUpdateShikimoriPersistsTokensAndPreservesRest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	seed := `[player]
path = "custom-mpv"
quality = "720"

[network]
connect_timeout = "42s"
max_parallel = 7

[shikimori]
enabled = false
`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	expires := time.Now().Add(24 * time.Hour).Unix()
	if err := UpdateShikimori(path, func(s *Shikimori) {
		s.Enabled = true
		s.AccessToken = "at-1"
		s.RefreshToken = "rt-1"
		s.TokenExpiresAt = expires
		s.ClientID = "cid-1"
		s.ClientSecret = "csec-1"
	}); err != nil {
		t.Fatalf("UpdateShikimori: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after update: %v", err)
	}
	sh := loaded.Shikimori
	if !sh.Enabled || sh.AccessToken != "at-1" || sh.RefreshToken != "rt-1" ||
		sh.TokenExpiresAt != expires || sh.ClientID != "cid-1" || sh.ClientSecret != "csec-1" {
		t.Errorf("shikimori round-trip = %+v", sh)
	}
	// Other settings survive the read-modify-write untouched.
	if loaded.Player.Path != "custom-mpv" || loaded.Player.Quality != "720" {
		t.Errorf("player section lost: %+v", loaded.Player)
	}
	if loaded.Network.ConnectTimeout != 42*time.Second || loaded.Network.MaxParallel != 7 {
		t.Errorf("network section lost: %+v", loaded.Network)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings perms = %o, want 0600 (secrets live here)", perm)
	}
}

// TestUpdateShikimoriCreatesMissingFile pins: persisting onto a missing
// settings path creates the file (defaults + the mutated section) instead
// of failing — `anicli shikimori auth` on a fresh install must work.
func TestUpdateShikimoriCreatesMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "settings.toml")
	if err := UpdateShikimori(path, func(s *Shikimori) {
		s.Enabled = true
		s.AccessToken = "at"
	}); err != nil {
		t.Fatalf("UpdateShikimori on missing file: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load created file: %v", err)
	}
	if !loaded.Shikimori.Enabled || loaded.Shikimori.AccessToken != "at" {
		t.Errorf("shikimori = %+v, want enabled with token", loaded.Shikimori)
	}
}

// TestUpdateShikimoriRejectsBrokenFile pins fail-loud: a malformed or
// unknown-key file must not be silently rewritten by the persistence
// path — the caller surfaces the error instead of clobbering the file.
func TestUpdateShikimoriRejectsBrokenFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SettingsFileName)
	if err := os.WriteFile(path, []byte("[shikimori]\nenabled = tru\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := UpdateShikimori(path, func(*Shikimori) {}); err == nil {
		t.Fatal("UpdateShikimori must fail on a malformed settings file")
	}

	before, _ := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if !strings.Contains(string(before), "enabled = tru") {
		t.Error("a failed update must not rewrite the file")
	}
}
