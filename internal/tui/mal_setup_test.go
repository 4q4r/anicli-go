package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
)

// fakeMALWriter records the [mal] sections the setup screens persist.
type fakeMALWriter struct {
	mu       sync.Mutex
	sections []config.MAL
	err      error
}

func (f *fakeMALWriter) write(section config.MAL) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections = append(f.sections, section)
	return f.err
}

func (f *fakeMALWriter) recorded() []config.MAL {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]config.MAL(nil), f.sections...)
}

// fakeMALWhoAmI records the candidate [mal] sections it verified.
type fakeMALWhoAmI struct {
	mu       sync.Mutex
	seen     []config.MAL
	user     MALUser
	verifyEr error
}

func (f *fakeMALWhoAmI) probe(_ context.Context, section config.MAL) (MALUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, section)
	return f.user, f.verifyEr
}

// fakeMALOAuth is a scriptable PKCE flow starter.
type fakeMALOAuth struct {
	mu      sync.Mutex
	started int
	url     string
	err     error
	// resolve is handed to the screen when err == nil.
	resolve func(ctx context.Context) (MALOAuthResult, error)
}

func (f *fakeMALOAuth) start(_, _ string, _ int) (string, func(context.Context) (MALOAuthResult, error), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	if f.err != nil {
		return "", nil, f.err
	}
	return f.url, f.resolve, nil
}

// malSetupDeps builds the deps the MAL setup screens consume.
func malSetupDeps(cfg config.MAL, w *fakeMALWriter, who *fakeMALWhoAmI, flow *fakeMALOAuth) *Deps {
	return &Deps{
		MALCfg:            cfg,
		MALSettingsWriter: w.write,
		MALWhoAmI:         who.probe,
		MALOAuth:          flow.start,
	}
}

func TestMALOAuthScreenInputs(t *testing.T) {
	flow := &fakeMALOAuth{url: "https://myanimelist.net/v1/oauth2/authorize?x=1"}
	deps := malSetupDeps(config.MAL{}, &fakeMALWriter{}, &fakeMALWhoAmI{}, flow)
	s := newMALOAuthScreen(deps)

	if s.phase != malOAuthInputID {
		t.Fatalf("initial phase = %v, want the client_id input", s.phase)
	}

	// client_id → secret prompt.
	s.idInput.SetValue("cid-1")
	if _, cmd := s.Update(enter()); cmdMsg(cmd) != nil {
		t.Fatalf("id submit schedules nothing by itself, got %#v", cmdMsg(cmd))
	}
	if s.phase != malOAuthInputSecret {
		t.Fatalf("after client_id phase = %v, want the secret input", s.phase)
	}
	// secret → the flow starts.
	s.secretInput.SetValue("csec-1")
	settled := cmdMsg(updateCmd(t, s, enter()))
	if s.phase != malOAuthRunning {
		t.Fatalf("after secret phase = %v, want running", s.phase)
	}
	if ready, ok := settled.(malOAuthReadyMsg); !ok || ready.err != nil || ready.authURL != flow.url {
		t.Fatalf("ready msg = %#v, want the authorize URL", settled)
	}
	if flow.started != 1 {
		t.Errorf("flow started %d times, want 1", flow.started)
	}
}

func TestMALOAuthScreenPrefilledCreds(t *testing.T) {
	flow := &fakeMALOAuth{url: "https://myanimelist.net/v1/oauth2/authorize?x=1"}
	deps := malSetupDeps(config.MAL{ClientID: "cid", ClientSecret: "csec"}, &fakeMALWriter{}, &fakeMALWhoAmI{}, flow)
	s := newMALOAuthScreen(deps)

	if s.phase != malOAuthRunning {
		t.Fatalf("prefilled phase = %v, want running", s.phase)
	}
	ready, ok := cmdMsg(s.Init()).(malOAuthReadyMsg) // run the start command
	if !ok || ready.err != nil || ready.authURL != flow.url {
		t.Fatalf("ready msg = %#v, want a started flow", cmdMsg(s.Init()))
	}
}

func TestMALOAuthScreenResolveFailure(t *testing.T) {
	flow := &fakeMALOAuth{url: "https://x", resolve: func(context.Context) (MALOAuthResult, error) {
		return MALOAuthResult{}, context.DeadlineExceeded
	}}
	w := &fakeMALWriter{}
	deps := malSetupDeps(config.MAL{ClientID: "cid", ClientSecret: "csec"}, w, &fakeMALWhoAmI{}, flow)
	s := newMALOAuthScreen(deps)

	driveMALOAuth(t, s)
	if s.phase != malOAuthError {
		t.Fatalf("phase = %v, want error", s.phase)
	}
	if len(w.recorded()) != 0 {
		t.Fatalf("a failed flow must not write settings, wrote %+v", w.recorded())
	}
}

// driveMALOAuth settles the whole ready→wait→save chain from Init,
// returning the final settled message (the persist+verify result).
func driveMALOAuth(t *testing.T, s *malOAuthScreen) tea.Msg {
	t.Helper()
	msg := cmdMsg(s.Init())
	for range 6 {
		if s.phase == malOAuthDone || s.phase == malOAuthError {
			return msg
		}
		if msg == nil {
			return nil
		}
		_, cmd := s.Update(msg)
		msg = cmdMsg(cmd)
	}
	return msg
}

// TestMALOAuthScreenHappyPath pins the whole flow: tokens resolve, the
// [mal] section persists (enabled + tokens + app credentials), the
// whoami greets, and the deps snapshot updates so the provider menu
// renders the fresh authorization status.
func TestMALOAuthScreenHappyPath(t *testing.T) {
	flow := &fakeMALOAuth{url: "https://x", resolve: func(context.Context) (MALOAuthResult, error) {
		return MALOAuthResult{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}}
	w := &fakeMALWriter{}
	who := &fakeMALWhoAmI{user: MALUser{ID: 777, Name: "owner"}}
	deps := malSetupDeps(config.MAL{ClientID: "cid", ClientSecret: "csec"}, w, who, flow)
	s := newMALOAuthScreen(deps)

	driveMALOAuth(t, s)
	if s.phase != malOAuthDone {
		t.Fatalf("phase = %v, want done (fail line %q)", s.phase, s.failLine)
	}

	sections := w.recorded()
	if len(sections) != 1 {
		t.Fatalf("[mal] writes = %d, want 1", len(sections))
	}
	sec := sections[0]
	if !sec.Enabled || sec.AccessToken != "at" || sec.RefreshToken != "rt" ||
		sec.ClientID != "cid" || sec.ClientSecret != "csec" {
		t.Errorf("persisted section = %+v", sec)
	}
	if deps.MALCfg.AccessToken != "at" || !deps.MALCfg.Enabled {
		t.Errorf("deps snapshot not refreshed: %+v", deps.MALCfg)
	}
	if len(who.seen) != 1 || !who.seen[0].Enabled || who.seen[0].AccessToken != "at" {
		t.Errorf("whoami probe sections = %+v", who.seen)
	}
}

func TestMALOAuthScreenPersistFailure(t *testing.T) {
	flow := &fakeMALOAuth{url: "https://x", resolve: func(context.Context) (MALOAuthResult, error) {
		return MALOAuthResult{AccessToken: "at"}, nil
	}}
	w := &fakeMALWriter{err: errShikiSettingsUnavailable}
	deps := malSetupDeps(config.MAL{ClientID: "cid", ClientSecret: "csec"}, w, &fakeMALWhoAmI{}, flow)
	s := newMALOAuthScreen(deps)

	driveMALOAuth(t, s)
	if s.phase != malOAuthError {
		t.Fatalf("phase = %v, want error", s.phase)
	}
	if !strings.Contains(s.failLine, "settings") {
		t.Errorf("fail line = %q, want the settings failure", s.failLine)
	}
}

func TestMALOAuthScreenNilSeam(t *testing.T) {
	deps := malSetupDeps(config.MAL{ClientID: "cid", ClientSecret: "csec"}, &fakeMALWriter{}, &fakeMALWhoAmI{}, &fakeMALOAuth{})
	deps.MALOAuth = nil
	s := newMALOAuthScreen(deps)

	ready := cmdMsg(s.Init()) // run the start command (nil seam → error msg)
	_, _ = s.Update(ready)    // the ready msg carries the nil-seam error
	if s.phase != malOAuthError {
		t.Fatalf("phase = %v, want the loud nil-seam error", s.phase)
	}
}

// TestProviderSetupMenu pins the PR112 provider pick: both trackers are
// offered with live authorization status; Shikimori leads to its method
// menu, MyAnimeList to the PKCE screen; Esc still quits (Shikimori auth
// is mandatory).
func TestProviderSetupMenu(t *testing.T) {
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, &fakeSettingsWriter{}, &fakeShikiWhoAmI{})
	s := NewShikimoriSetup(deps)

	t.Run("view offers both providers with status", func(t *testing.T) {
		view := s.View().Content
		for _, want := range []string{
			"Shikimori",
			"MyAnimeList",
			"○ not authorized",
		} {
			if !strings.Contains(view, want) {
				t.Fatalf("provider menu must contain %q, got:\n%s", want, view)
			}
		}
	})

	t.Run("shikimori pick opens the method menu", func(t *testing.T) {
		m := NewShikimoriSetup(deps)
		m.list.Jump(indexOfSetupChoice(m, "shikimori"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != shikiMethodID {
			t.Fatalf("shikimori pick must push the method menu, got %#v", msg)
		}
	})

	t.Run("myanimelist pick opens the MAL oauth screen", func(t *testing.T) {
		m := NewShikimoriSetup(deps)
		m.list.Jump(indexOfSetupChoice(m, "myanimelist"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != malOAuthID {
			t.Fatalf("myanimelist pick must push the MAL oauth screen, got %#v", msg)
		}
	})

	t.Run("authenticated markers render", func(t *testing.T) {
		deps2 := shikiSetupDeps(config.Shikimori{Enabled: true, Session: "s"}, &fakeSettingsWriter{}, &fakeShikiWhoAmI{})
		deps2.MALCfg = config.MAL{Enabled: true, AccessToken: "at"}
		m := NewShikimoriSetup(deps2)
		view := m.View().Content
		if strings.Count(view, "✓ authorized") != 2 {
			t.Fatalf("both providers must show ✓ authorized, got:\n%s", view)
		}
	})

	t.Run("esc quits (shikimori auth is mandatory)", func(t *testing.T) {
		m := NewShikimoriSetup(deps)
		if cmd := updateCmd(t, m, esc()); !isQuitCmd(cmd) {
			t.Fatalf("esc at the provider menu must quit, got %#v", cmdMsg(cmd))
		}
	})
}

// indexOfMethodChoice finds a choice index in the Shikimori method
// submenu.
func indexOfMethodChoice(s *shikiMethodScreen, id string) int {
	for i, c := range s.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// TestShikiMethodMenu pins the Shikimori method submenu (PR112 split
// out of the old single menu).
func TestShikiMethodMenu(t *testing.T) {
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, &fakeSettingsWriter{}, &fakeShikiWhoAmI{})
	s := newShikiMethodScreen(deps)

	t.Run("cookie pushes the cookie screen", func(t *testing.T) {
		m := newShikiMethodScreen(deps)
		m.list.Jump(indexOfMethodChoice(m, "cookie"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != shikiCookieID {
			t.Fatalf("cookie pick must push the cookie screen, got %#v", msg)
		}
	})
	t.Run("oauth pushes the oauth screen", func(t *testing.T) {
		m := newShikiMethodScreen(deps)
		m.list.Jump(indexOfMethodChoice(m, "oauth"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != shikiOAuthID {
			t.Fatalf("oauth pick must push the oauth screen, got %#v", msg)
		}
	})
	t.Run("esc pops back to the provider menu", func(t *testing.T) {
		msg := cmdMsg(updateCmd(t, s, esc()))
		if _, ok := msg.(popMsg); !ok {
			t.Fatalf("esc must pop, got %#v", msg)
		}
	})
}
