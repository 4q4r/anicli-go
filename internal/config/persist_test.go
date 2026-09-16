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

// TestUpdateShikimoriPreservesCommentsAndOrdering pins the PR32
// comment-preserving rewrite: only the [shikimori] section's key-value
// lines are replaced. Comments inside the section survive (moved above
// the fresh keys; inline comments are allowed to go), every other
// section — including its comments and key order — stays byte-for-byte
// verbatim, and the section ordering is untouched.
func TestUpdateShikimoriPreservesCommentsAndOrdering(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	seed := `# anicli settings — edited by hand over months.

[player]
# my player of choice
path = "custom-mpv"
quality = "720"

# ------------------------------------------
# [shikimori] Интеграция с трекером
# ------------------------------------------
[shikimori]
# Включить модуль.
enabled = false

# Cookie "_kawai_session" (обязательна для записи прогресса).
# session = "ВАШ_ТОКЕН_ЗДЕСЬ"

[skip]
# Порядок fallback-цепочки провайдеров пропусков.
providers_order = ["aniskip", "anime_skip", "intro_skipper"]
`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateShikimori(path, func(s *Shikimori) {
		s.Enabled = true
		s.Session = "live-cookie"
	}); err != nil {
		t.Fatalf("UpdateShikimori: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
	got := string(after)

	// The user's file-level comment survives.
	if !strings.Contains(got, "# anicli settings — edited by hand over months.") {
		t.Errorf("file header comment lost:\n%s", got)
	}
	// Untouched sections stay verbatim, comments included.
	if !strings.Contains(got, "# my player of choice\npath = \"custom-mpv\"") {
		t.Errorf("player section or its comment altered:\n%s", got)
	}
	if !strings.Contains(got, "# Порядок fallback-цепочки провайдеров пропусков.\nproviders_order = [\"aniskip\", \"anime_skip\", \"intro_skipper\"]") {
		t.Errorf("skip section or its comment altered:\n%s", got)
	}
	// Shikimori section comments survive; fresh values are in place.
	if !strings.Contains(got, "# Включить модуль.") ||
		!strings.Contains(got, "# Cookie \"_kawai_session\" (обязательна для записи прогресса).") {
		t.Errorf("shikimori section comments lost:\n%s", got)
	}
	if !strings.Contains(got, "enabled = true") || !strings.Contains(got, "session = \"live-cookie\"") {
		t.Errorf("fresh shikimori values missing:\n%s", got)
	}
	// Section ordering: player before shikimori, skip after.
	playerAt := strings.Index(got, "[player]")
	shikiAt := strings.Index(got, "[shikimori]")
	skipAt := strings.Index(got, "[skip]")
	if playerAt >= shikiAt || shikiAt >= skipAt {
		t.Errorf("section ordering changed:\n%s", got)
	}
	// The stale commented-out session stays (it is a comment line).
	if !strings.Contains(got, "# session = \"ВАШ_ТОКЕН_ЗДЕСЬ\"") {
		t.Errorf("commented-out session template lost:\n%s", got)
	}

	// Round-trip: the rewritten file still loads with the new values.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after rewrite: %v", err)
	}
	if !loaded.Shikimori.Enabled || loaded.Shikimori.Session != "live-cookie" {
		t.Errorf("shikimori = %+v, want enabled with session", loaded.Shikimori)
	}
	if loaded.Player.Path != "custom-mpv" || loaded.Player.Quality != "720" {
		t.Errorf("player lost: %+v", loaded.Player)
	}
}

// TestUpdateShikimoriAppendsMissingSection pins: a settings file
// without a [shikimori] section keeps every existing line verbatim and
// gains the section at the end; the file stays loadable.
func TestUpdateShikimoriAppendsMissingSection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	seed := "[player]\n# my player\npath = \"mpv\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateShikimori(path, func(s *Shikimori) {
		s.Enabled = true
		s.AccessToken = "at-1"
	}); err != nil {
		t.Fatalf("UpdateShikimori: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
	got := string(after)
	if !strings.HasPrefix(got, seed) {
		t.Errorf("existing content must stay verbatim at the top, got:\n%s", got)
	}
	if !strings.Contains(got, "[shikimori]") || !strings.Contains(got, "access_token = \"at-1\"") {
		t.Errorf("appended [shikimori] section missing:\n%s", got)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after append: %v", err)
	}
	if !loaded.Shikimori.Enabled || loaded.Shikimori.AccessToken != "at-1" {
		t.Errorf("shikimori = %+v", loaded.Shikimori)
	}
	if loaded.Player.Path != "mpv" {
		t.Errorf("player lost: %+v", loaded.Player)
	}
}

// TestUpdateShikimoriLastSectionTrailingNewline pins the trailing
// edge: when [shikimori] is the LAST section, the rewrite keeps the
// file syntactically valid (exactly one trailing newline, no stray
// bytes after the fresh keys).
func TestUpdateShikimoriLastSectionTrailingNewline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	seed := "[player]\npath = \"mpv\"\n\n[shikimori]\nenabled = false\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateShikimori(path, func(s *Shikimori) { s.Session = "s-1" }); err != nil {
		t.Fatalf("UpdateShikimori: %v", err)
	}

	after, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
	got := string(after)
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("file must end with exactly one newline, got tail %q", got[max(0, len(got)-40):])
	}
	if strings.Contains(got, "[shikimori]\n\n") {
		t.Errorf("no blank line may sit between the section header and its keys:\n%s", got)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after trailing rewrite: %v", err)
	}
}
