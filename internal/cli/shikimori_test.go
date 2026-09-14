package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/tui"
)

// fakeOAuthClient is the test double of the auth-flow client seam.
type fakeOAuthClient struct {
	set       *shikimori.TokenSet
	userID    int64
	nickname  string
	exchangeF func(code string) (*shikimori.TokenSet, error)
	whoamiF   func() (int64, error)
}

func (f *fakeOAuthClient) ExchangeCode(_ context.Context, _, _, _, code string) (*shikimori.TokenSet, error) {
	if f.exchangeF != nil {
		return f.exchangeF(code)
	}
	return f.set, nil
}

func (f *fakeOAuthClient) GetUserID(context.Context) (int64, error) {
	if f.whoamiF != nil {
		return f.whoamiF()
	}
	return f.userID, nil
}

func (f *fakeOAuthClient) WhoAmI(context.Context) (int64, string, error) {
	if f.whoamiF != nil {
		id, err := f.whoamiF()
		return id, f.nickname, err
	}
	return f.userID, f.nickname, nil
}

// stubOAuthSeams replaces the client and browser seams for one test and
// restores them afterwards.
func stubOAuthSeams(t *testing.T, client shikiOAuthClient, browser func(authURL string) error) {
	t.Helper()
	origClient, origBrowser := newShikiOAuthClient, shikiOpenBrowser
	newShikiOAuthClient = func(config.Settings) (shikiOAuthClient, error) { return client, nil }
	shikiOpenBrowser = browser
	t.Cleanup(func() {
		newShikiOAuthClient = origClient
		shikiOpenBrowser = origBrowser
	})
}

// TestShikimoriAuthFlow pins the full OAuth2 authorization-code flow
// (PR25 B): authorize URL printed with the loopback redirect and
// user_rates scope, code received on the local callback server, tokens
// exchanged and persisted into [shikimori] (other settings preserved),
// success + user info printed.
func TestShikimoriAuthFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[player]\npath = \"mpv-x\"\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	expires := time.Now().Add(86400 * time.Second).Unix()
	fake := &fakeOAuthClient{
		set:    &shikimori.TokenSet{AccessToken: "at-9", RefreshToken: "rt-9", ExpiresAt: expires},
		userID: 42,
	}
	var seenAuthURL string
	stubOAuthSeams(t, fake, func(authURL string) error {
		seenAuthURL = authURL
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		redirect := u.Query().Get("redirect_uri")
		if redirect == "" {
			return errors.New("authorize URL carries no redirect_uri")
		}
		resp, err := http.Get(redirect + "?code=abc-123")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("callback status = %d", resp.StatusCode)
		}
		return nil
	})

	var out bytes.Buffer
	if err := runShikimoriAuth(context.Background(), &out, path, "cid-1", "csec-1", 0); err != nil {
		t.Fatalf("runShikimoriAuth: %v", err)
	}

	// The authorize URL contract.
	u, err := url.Parse(seenAuthURL)
	if err != nil {
		t.Fatalf("authorize URL %q: %v", seenAuthURL, err)
	}
	if q := u.Query(); q.Get("client_id") != "cid-1" || q.Get("scope") != "user_rates" ||
		q.Get("response_type") != "code" || !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("authorize URL query = %v", q)
	}
	if !strings.Contains(out.String(), "oauth/authorize") {
		t.Errorf("output must print the authorize URL, got:\n%s", out.String())
	}

	// Tokens persisted, other settings preserved.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after auth: %v", err)
	}
	sh := loaded.Shikimori
	if !sh.Enabled || sh.AccessToken != "at-9" || sh.RefreshToken != "rt-9" ||
		sh.TokenExpiresAt != expires || sh.ClientID != "cid-1" || sh.ClientSecret != "csec-1" {
		t.Errorf("persisted [shikimori] = %+v", sh)
	}
	if loaded.Player.Path != "mpv-x" {
		t.Errorf("player section lost: %+v", loaded.Player)
	}
	// Success + user info.
	if !strings.Contains(out.String(), "42") || !strings.Contains(out.String(), "сохранены") {
		t.Errorf("output must report stored tokens and the user, got:\n%s", out.String())
	}
}

// TestShikimoriAuthFailsWithoutCredentials pins fail-loud: neither
// flags nor stored app credentials -> explicit error, no server started.
func TestShikimoriAuthFailsWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	stubOAuthSeams(t, &fakeOAuthClient{}, func(string) error { return nil })

	var out bytes.Buffer
	err := runShikimoriAuth(context.Background(), &out, path, "", "", 0)
	if err == nil {
		t.Fatal("auth without credentials must fail")
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("error must name the missing credentials: %v", err)
	}
}

// TestShikimoriAuthExchangeFailureFailsLoud pins: a rejected code
// exchange surfaces the error and writes nothing to settings.
func TestShikimoriAuthExchangeFailureFailsLoud(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.toml")
	fake := &fakeOAuthClient{exchangeF: func(string) (*shikimori.TokenSet, error) {
		return nil, errors.New("invalid_grant: code expired")
	}}
	stubOAuthSeams(t, fake, func(authURL string) error {
		u, _ := url.Parse(authURL)
		resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=stale")
		if err == nil {
			_ = resp.Body.Close()
		}
		return nil
	})

	var out bytes.Buffer
	if err := runShikimoriAuth(context.Background(), &out, path, "cid", "csec", 0); err == nil {
		t.Fatal("a failed exchange must fail the command")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings file must not be created on a failed exchange: %v", err)
	}
}

// TestShikiCallbackServer pins the loopback redirect target: /callback
// with a code answers 200 and publishes the code; an error parameter
// surfaces as a typed failure; unknown paths get the hint page.
func TestShikiCallbackServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, codeCh, errCh := startShikiCallbackServer(ln)
	defer func() { _ = srv.Close() }()
	base := "http://" + ln.Addr().String()

	resp, err := http.Get(base + "/callback?code=xyz-7")
	if err != nil {
		t.Fatalf("callback get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status = %d, want 200", resp.StatusCode)
	}
	select {
	case code := <-codeCh:
		if code != "xyz-7" {
			t.Errorf("code = %q, want xyz-7", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("code not published")
	}

	resp, err = http.Get(base + "/callback?error=access_denied")
	if err != nil {
		t.Fatalf("callback error get: %v", err)
	}
	_ = resp.Body.Close()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("callback error = %v, want access_denied", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("error not published")
	}
}

// stubShikiProbe replaces the live status probe seam.
func stubShikiProbe(t *testing.T, rep shikiStatusReport) {
	t.Helper()
	orig := probeShikimoriStatus
	probeShikimoriStatus = func(context.Context, config.Settings, time.Duration) shikiStatusReport {
		return rep
	}
	t.Cleanup(func() { probeShikimoriStatus = orig })
}

// TestShikimoriStatusOutput pins the status command rendering per mode.
func TestShikimoriStatusOutput(t *testing.T) {
	tests := []struct {
		name string
		rep  shikiStatusReport
		want []string
	}{
		{
			name: "disabled",
			rep:  shikiStatusReport{Mode: "disabled"},
			want: []string{"отключён", "shikimori.enabled"},
		},
		{
			name: "public",
			rep:  shikiStatusReport{Mode: "none"},
			want: []string{"публичный", "без учётных данных"},
		},
		{
			name: "oauth ok",
			rep: shikiStatusReport{Mode: "bearer", UserID: 42,
				ExpiresAt: time.Now().Add(23 * time.Hour).Unix(), HasClientID: true},
			want: []string{"bearer", "42", "токен", "client_id"},
		},
		{
			name: "oauth whoami failed",
			rep: shikiStatusReport{Mode: "bearer", UserErr: errors.New("HTTP 401"),
				ExpiresAt: time.Now().Add(time.Hour).Unix(), HasClientID: true},
			want: []string{"bearer", "401"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubShikiProbe(t, tt.rep)
			var buf bytes.Buffer
			if err := runShikimoriStatus(context.Background(), &buf, filepath.Join(t.TempDir(), "none.toml")); err != nil {
				t.Fatalf("runShikimoriStatus: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("status output missing %q, got:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestDoctorShowsShikimoriRow pins PR25 F: the doctor table carries a
// shikimori row with the auth mode, user id and token expiry (bearer)
// or the disabled verdict. Not parallel: subtests use t.Setenv.
func TestDoctorShowsShikimoriRow(t *testing.T) {

	t.Run("bearer with user and expiry", func(t *testing.T) {
		t.Setenv("ANICLI_DATA", t.TempDir())
		stubDoctorProbe(t, &stubProbe{results: 1})
		stubShikiProbe(t, shikiStatusReport{Mode: "bearer", UserID: 42,
			ExpiresAt: time.Now().Add(23 * time.Hour).Unix(), HasClientID: true})

		var buf bytes.Buffer
		if err := runDoctor(context.Background(), "", &buf); err != nil {
			t.Fatalf("runDoctor: %v", err)
		}
		for _, want := range []string{"shikimori", "bearer", "42", "токен до"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("doctor output missing %q, got:\n%s", want, buf.String())
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Setenv("ANICLI_DATA", t.TempDir())
		stubDoctorProbe(t, &stubProbe{results: 1})
		stubShikiProbe(t, shikiStatusReport{Mode: "disabled"})

		var buf bytes.Buffer
		if err := runDoctor(context.Background(), "", &buf); err != nil {
			t.Fatalf("runDoctor: %v", err)
		}
		if !strings.Contains(buf.String(), "ОТКЛЮЧЁН") {
			t.Errorf("disabled shikimori must render ОТКЛЮЧЁН, got:\n%s", buf.String())
		}
	})
}

// stubProbe replaces the provider probe seam (doctor_test.go's helper
// shape, without recording).
func stubDoctorProbe(t *testing.T, s *stubProbe) {
	t.Helper()
	orig := doctorProbe
	doctorProbe = s.probe
	t.Cleanup(func() { doctorProbe = orig })
}

// TestWireShikiSetup pins the PR26 runTUI wiring: the deps carry the
// [shikimori] snapshot, the settings writer persists through
// config.UpdateShikimori (other sections preserved), whoami verifies
// candidate sections and the OAuth seam reuses the CLI loopback flow.
func TestWireShikiSetup(t *testing.T) {
	// Not parallel: swaps the package-level client/browser seams.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[player]\npath = \"mpv-x\"\n\n[shikimori]\nenabled = true\nclient_id = \"cid\"\nclient_secret = \"csec\"\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	settings, err := loadSettingsOrFail(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	expires := time.Now().Add(86400 * time.Second).Unix()
	fake := &fakeOAuthClient{
		set:       &shikimori.TokenSet{AccessToken: "at-7", RefreshToken: "rt-7", ExpiresAt: expires},
		userID:    42,
		nickname:  "wired-fan",
		exchangeF: nil,
	}
	stubOAuthSeams(t, fake, func(string) error { return nil })

	deps := &tui.Deps{}
	wireShikiSetup(deps, *settings, path)

	t.Run("snapshot and seams wired", func(t *testing.T) {
		if deps.ShikiCfg != settings.Shikimori {
			t.Fatalf("ShikiCfg = %+v, want the loaded section", deps.ShikiCfg)
		}
		if deps.SettingsWriter == nil || deps.ShikiWhoAmI == nil || deps.ShikiOAuth == nil {
			t.Fatal("all three setup seams must be wired")
		}
	})

	t.Run("settings writer persists the section and preserves others", func(t *testing.T) {
		section := settings.Shikimori
		section.Session = "new-cookie"
		if err := deps.SettingsWriter(section); err != nil {
			t.Fatalf("SettingsWriter: %v", err)
		}
		loaded, err := config.Load(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !loaded.Shikimori.Enabled || loaded.Shikimori.Session != "new-cookie" ||
			loaded.Shikimori.ClientID != "cid" || loaded.Shikimori.ClientSecret != "csec" {
			t.Fatalf("persisted [shikimori] = %+v", loaded.Shikimori)
		}
		if loaded.Player.Path != "mpv-x" {
			t.Fatalf("player section lost: %+v", loaded.Player)
		}
	})

	t.Run("whoami verifies a candidate section", func(t *testing.T) {
		section := settings.Shikimori
		section.Session = "verify-me"
		user, err := deps.ShikiWhoAmI(context.Background(), section)
		if err != nil {
			t.Fatalf("ShikiWhoAmI: %v", err)
		}
		if user.ID != 42 || user.Nickname != "wired-fan" {
			t.Fatalf("user = %+v, want (42, wired-fan)", user)
		}
	})

	t.Run("oauth seam runs the loopback flow", func(t *testing.T) {
		fake.exchangeF = func(string) (*shikimori.TokenSet, error) {
			return &shikimori.TokenSet{AccessToken: "at-26", RefreshToken: "rt-26", ExpiresAt: expires}, nil
		}
		authURL, resolve, err := deps.ShikiOAuth("cid", "csec", 0)
		if err != nil {
			t.Fatalf("ShikiOAuth start: %v", err)
		}
		u, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("authorize URL %q: %v", authURL, err)
		}
		if q := u.Query(); q.Get("client_id") != "cid" || !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Fatalf("authorize URL query = %v", q)
		}
		// Deliver the code the way the browser redirect would.
		resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=live-code")
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		_ = resp.Body.Close()

		set, err := resolve(context.Background())
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if set.AccessToken != "at-26" || set.RefreshToken != "rt-26" || set.ExpiresAt != expires {
			t.Fatalf("tokens = %+v", set)
		}
	})
}
