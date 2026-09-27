package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/tui"
)

// fakeMALAuthClient is a scriptable token-exchange + whoami surface.
type fakeMALAuthClient struct {
	set      *mal.TokenSet
	userID   int64
	nickname string
	exchange func(code, verifier string) error
}

func (f *fakeMALAuthClient) ExchangeCode(_ context.Context, _, code, verifier string) (*mal.TokenSet, error) {
	if f.exchange != nil {
		if err := f.exchange(code, verifier); err != nil {
			return nil, err
		}
	}
	return f.set, nil
}

func (f *fakeMALAuthClient) WhoAmI(context.Context) (int64, string, error) {
	return f.userID, f.nickname, nil
}

// stubMALSeams replaces the MAL client and browser seams for one test.
func stubMALSeams(t *testing.T, client malAuthClient, browser func(authURL string) error) {
	t.Helper()
	origClient, origBrowser := newMALAuthClient, shikiOpenBrowser
	newMALAuthClient = func(config.Settings) (malAuthClient, error) { return client, nil }
	shikiOpenBrowser = browser
	t.Cleanup(func() {
		newMALAuthClient = origClient
		shikiOpenBrowser = origBrowser
	})
}

// TestMALAuthFlow pins the full PKCE flow (PR112): the authorize URL
// carries the plain code_challenge, the loopback callback delivers the
// code, the exchange + whoami run, and the [mal] section persists with
// the other settings untouched.
func TestMALAuthFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[player]\npath = \"mpv-x\"\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	expires := time.Now().Add(time.Hour).Unix()
	fake := &fakeMALAuthClient{
		set:      &mal.TokenSet{AccessToken: "mal-at", RefreshToken: "mal-rt", ExpiresAt: expires},
		userID:   777,
		nickname: "owner",
	}
	var verifierSeen string
	fake.exchange = func(_, verifier string) error {
		verifierSeen = verifier
		if len(verifier) < 43 {
			return fmt.Errorf("code verifier %q shorter than the PKCE minimum", verifier)
		}
		return nil
	}

	stubMALSeams(t, fake, func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "plain" || q.Get("code_challenge") == "" {
			return errors.New("authorize URL carries no plain PKCE challenge")
		}
		if !strings.Contains(authURL, "/v1/oauth2/authorize") {
			return errors.New("authorize URL must hit the MAL v1 oauth2 endpoint")
		}
		resp, err := http.Get(q.Get("redirect_uri") + "?code=abc-9&state=" + url.QueryEscape(q.Get("state")))
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		return nil
	})

	var out bytes.Buffer
	if err := runMALAuth(context.Background(), &out, path, "mal-cid", "mal-csec", 0); err != nil {
		t.Fatalf("runMALAuth: %v", err)
	}

	if verifierSeen == "" {
		t.Error("the exchange must run with the flow's code verifier")
	}
	if !strings.Contains(out.String(), "myanimelist.net/v1/oauth2/authorize") {
		t.Errorf("output must print the authorize URL, got:\n%s", out.String())
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after auth: %v", err)
	}
	m := loaded.MAL
	if !m.Enabled || m.AccessToken != "mal-at" || m.RefreshToken != "mal-rt" ||
		m.TokenExpiresAt != expires || m.ClientID != "mal-cid" || m.ClientSecret != "mal-csec" {
		t.Errorf("persisted [mal] = %+v", m)
	}
	if loaded.Player.Path != "mpv-x" {
		t.Errorf("player section lost: %+v", loaded.Player)
	}
	if !strings.Contains(out.String(), "777") {
		t.Errorf("output must report the user, got:\n%s", out.String())
	}
}

func TestMALAuthFailsWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	var out bytes.Buffer
	if err := runMALAuth(context.Background(), &out, path, "", "", 0); err == nil {
		t.Fatal("runMALAuth without app credentials must fail loud")
	}
}

func TestMALAuthRejectsStateMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	fake := &fakeMALAuthClient{set: &mal.TokenSet{AccessToken: "at"}}
	var srv *httptest.Server
	stubMALSeams(t, fake, func(authURL string) error {
		u, _ := url.Parse(authURL)
		// A forged state (not the flow's) must be rejected.
		resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=x&state=forged")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("forged state status = %d, want 400", resp.StatusCode)
		}
		return nil
	})
	_ = srv

	if err := runMALAuth(context.Background(), &bytes.Buffer{}, path, "cid", "csec", 0); err == nil {
		t.Fatal("a forged callback state must fail the flow")
	}
}

func TestMALStatusOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := config.UpdateMAL(path, func(s *config.MAL) {
		s.Enabled = true
		s.AccessToken = "at"
	}); err != nil {
		t.Fatalf("seed [mal]: %v", err)
	}

	var out bytes.Buffer
	if err := runMALStatus(context.Background(), &out, path); err != nil {
		t.Fatalf("runMALStatus: %v", err)
	}
	if !strings.Contains(out.String(), "bearer") {
		t.Errorf("status output = %q, want the bearer mode line", out.String())
	}
}

// TestWireMALSetup pins the TUI seams (PR112): the [mal] snapshot, the
// section writer and a working PKCE flow land on Deps.
func TestWireMALSetup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	settings := config.Default()
	settings.MAL.ClientID = "cid-w"
	settings.MAL.ClientSecret = "csec-w"

	deps := &tui.Deps{}
	wireMALSetup(deps, settings, path)

	if deps.MALCfg.ClientID != "cid-w" {
		t.Fatalf("MALCfg = %+v", deps.MALCfg)
	}
	if deps.MALSettingsWriter == nil || deps.MALWhoAmI == nil || deps.MALOAuth == nil {
		t.Fatal("all MAL seams must be wired")
	}

	// The writer persists a section round-trip.
	if err := deps.MALSettingsWriter(config.MAL{Enabled: true, AccessToken: "w-at"}); err != nil {
		t.Fatalf("MALSettingsWriter: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.MAL.Enabled || loaded.MAL.AccessToken != "w-at" {
		t.Errorf("[mal] after writer = %+v", loaded.MAL)
	}
}
