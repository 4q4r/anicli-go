package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUpdateMALPersistsTokensAndPreservesRest pins the [mal] settings
// persistence contract (PR112): the section round-trips the OAuth2
// fields through a read-modify-write that preserves every other
// section, keeps the file loadable and secret-safe (0600), and coexists
// with a [shikimori] section (both trackers authenticate at once).
func TestUpdateMALPersistsTokensAndPreservesRest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	seed := `[player]
path = "custom-mpv"

[shikimori]
enabled = true
access_token = "shiki-at"

[mal]
enabled = false
`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	expires := time.Now().Add(time.Hour).Unix()
	if err := UpdateMAL(path, func(s *MAL) {
		s.Enabled = true
		s.AccessToken = "mal-at-1"
		s.RefreshToken = "mal-rt-1"
		s.TokenExpiresAt = expires
		s.ClientID = "mal-cid-1"
		s.ClientSecret = "mal-csec-1"
	}); err != nil {
		t.Fatalf("UpdateMAL: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after update: %v", err)
	}
	m := loaded.MAL
	if !m.Enabled || m.AccessToken != "mal-at-1" || m.RefreshToken != "mal-rt-1" ||
		m.TokenExpiresAt != expires || m.ClientID != "mal-cid-1" || m.ClientSecret != "mal-csec-1" {
		t.Errorf("mal round-trip = %+v", m)
	}
	// The other sections survive the read-modify-write untouched —
	// including the shikimori tokens (dual-provider ruling).
	if loaded.Player.Path != "custom-mpv" {
		t.Errorf("player section lost: %+v", loaded.Player)
	}
	if !loaded.Shikimori.Enabled || loaded.Shikimori.AccessToken != "shiki-at" {
		t.Errorf("shikimori section lost: %+v", loaded.Shikimori)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings perms = %o, want 0600 (secrets live here)", perm)
	}
}

// TestUpdateMALCreatesMissingFile pins: persisting MAL tokens onto a
// missing settings path creates the file — `anicli mal auth` on a fresh
// install must work.
func TestUpdateMALCreatesMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "settings.toml")
	if err := UpdateMAL(path, func(s *MAL) {
		s.Enabled = true
		s.AccessToken = "at"
	}); err != nil {
		t.Fatalf("UpdateMAL on missing file: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load created file: %v", err)
	}
	if !loaded.MAL.Enabled || loaded.MAL.AccessToken != "at" {
		t.Errorf("mal = %+v, want enabled with token", loaded.MAL)
	}
}

// TestUpdateMALRejectsBrokenFile pins fail-loud for the [mal] rewrite:
// a malformed file must not be silently clobbered.
func TestUpdateMALRejectsBrokenFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SettingsFileName)
	if err := os.WriteFile(path, []byte("[mal]\nenabled = definitely-not-bool\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if err := UpdateMAL(path, func(s *MAL) { s.Enabled = true }); err == nil {
		t.Error("UpdateMAL on a broken file: expected an error, got nil")
	}
}

// TestDefaultMALDisabled pins the default: the MAL integration is
// opt-in (enabled = false) with no credentials.
func TestDefaultMALDisabled(t *testing.T) {
	t.Parallel()

	got := Default()
	if got.MAL.Enabled {
		t.Error("MAL.Enabled = true by default, want false (opt-in)")
	}
	if got.MAL.AccessToken != "" || got.MAL.RefreshToken != "" || got.MAL.ClientID != "" {
		t.Errorf("MAL secrets must default empty, got %+v", got.MAL)
	}
}
