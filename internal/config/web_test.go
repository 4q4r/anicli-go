package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const webUsersTOML = `
[api]
enabled = true
bind = "127.0.0.1:9001"
token_ttl = "5m"
refresh_token_ttl = "48h"
auth_secret_key = "file-secret"

[web.users.alice]
password_hash = "pbkdf2_sha256$240000$uE0qBHS22YmdwZyQyeio4w$L17pdSzHKbUXDkb-itZUhRVltIcfhCRrSQJHLmCKLbM"

[web.users.bob]
password_hash = "pbkdf2_sha256$120000$g6X6shLqMc4YnTK2_GrY3A$Z2XDqxTZZ9JCiXledc_Ho4o8f0Cq3q0A4eXtF7HKgNM"
`

func TestWebUsersAndAPISecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte(webUsersTOML), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(s.Web.Users) != 2 {
		t.Fatalf("Web.Users = %d entries, want 2", len(s.Web.Users))
	}
	alice, ok := s.Web.Users["alice"]
	if !ok {
		t.Fatal("alice missing from Web.Users")
	}
	if !stringsHasPrefix(alice.PasswordHash, "pbkdf2_sha256$") {
		t.Fatalf("alice password hash = %q", alice.PasswordHash)
	}

	if s.API.AuthSecret != "file-secret" {
		t.Fatalf("API.AuthSecret = %q, want file value", s.API.AuthSecret)
	}
	if want := 5 * time.Minute; s.API.TokenTTL != want {
		t.Fatalf("API.TokenTTL = %v, want %v", s.API.TokenTTL, want)
	}
	if want := 48 * time.Hour; s.API.RefreshTokenTTL != want {
		t.Fatalf("API.RefreshTokenTTL = %v, want %v", s.API.RefreshTokenTTL, want)
	}
}

func TestAPIDefaults(t *testing.T) {
	s := Default()
	if s.API.RefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("API.RefreshTokenTTL default = %v, want 720h", s.API.RefreshTokenTTL)
	}
	if s.API.AuthSecret != "" {
		t.Fatalf("API.AuthSecret default = %q, want empty", s.API.AuthSecret)
	}
	if s.Web.Users != nil && len(s.Web.Users) != 0 {
		t.Fatal("Web.Users must default empty")
	}
}

func TestAPIEnabledRequiresSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte("[api]\nenabled = true\n"), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("enabled api without auth_secret_key must fail validation (fail loud at startup, not per request)")
	}
}

func TestAPISecretEnvOverride(t *testing.T) {
	t.Setenv(EnvAPISecret, "env-secret")
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte("[api]\nenabled = true\nauth_secret_key = \"file-secret\"\n"), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.API.AuthSecret != "env-secret" {
		t.Fatalf("API.AuthSecret = %q, want env value to win", s.API.AuthSecret)
	}
}

// stringsHasPrefix avoids importing strings for one check.
func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
