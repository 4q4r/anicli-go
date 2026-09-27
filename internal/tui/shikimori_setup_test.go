package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
)

// fakeSettingsWriter records the sections the setup screens persist.
type fakeSettingsWriter struct {
	mu       sync.Mutex
	sections []config.Shikimori
	err      error
}

func (f *fakeSettingsWriter) write(section config.Shikimori) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections = append(f.sections, section)
	return f.err
}

func (f *fakeSettingsWriter) recorded() []config.Shikimori {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]config.Shikimori(nil), f.sections...)
}

// fakeShikiWhoAmI records the candidate sections it verified.
type fakeShikiWhoAmI struct {
	mu       sync.Mutex
	seen     []config.Shikimori
	user     ShikiUser
	verifyEr error
}

func (f *fakeShikiWhoAmI) probe(_ context.Context, section config.Shikimori) (ShikiUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, section)
	return f.user, f.verifyEr
}

func (f *fakeShikiWhoAmI) sections() []config.Shikimori {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]config.Shikimori(nil), f.seen...)
}

// shikiSetupDeps builds the deps the setup screens consume.
func shikiSetupDeps(cfg config.Shikimori, w *fakeSettingsWriter, who *fakeShikiWhoAmI) *Deps {
	return &Deps{
		ShikiCfg:       cfg,
		SettingsWriter: w.write,
		ShikiWhoAmI:    who.probe,
	}
}

// settleCmd drives one Update, runs the produced command and feeds
// the settled message back into the screen (what the bubbletea
// runtime does between commands and updates).
func settleCmd(t *testing.T, s Screen, msg tea.Msg) tea.Msg {
	t.Helper()
	_, cmd := s.Update(msg)
	settled := cmdMsg(cmd)
	if settled != nil {
		_, _ = s.Update(settled)
	}
	return settled
}

// updateCmd drives one Update and returns its command (asserting
// the screen survived).
func updateCmd(t *testing.T, s Screen, msg tea.Msg) tea.Cmd {
	t.Helper()
	_, cmd := s.Update(msg)
	return cmd
}

// cmdMsg runs cmd and returns its message (nil-safe).
func cmdMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// TestShikiCookieScreenHappyPath pins the PR26 cookie flow: pasting the
// _kawai_session value persists a section that preserves unrelated
// fields (client credentials), verifies via whoami, greets the user by
// nickname and continues to the root menu.
func TestShikiCookieScreenHappyPath(t *testing.T) {
	w := &fakeSettingsWriter{}
	who := &fakeShikiWhoAmI{user: ShikiUser{ID: 42, Nickname: "kawai-fan"}}
	deps := shikiSetupDeps(config.Shikimori{Enabled: true, ClientID: "cid", ClientSecret: "csec"}, w, who)

	s := newShikiCookieScreen(deps)
	if view := s.View().Content; !strings.Contains(view, "_kawai_session") {
		t.Fatalf("cookie screen must prompt for _kawai_session, got:\n%s", view)
	}

	s.input.SetValue("pasted-cookie")
	msg := settleCmd(t, s, enter())
	settled, ok := msg.(shikiSavedMsg)
	if !ok {
		t.Fatalf("submit must settle a shikiSavedMsg, got %#v", msg)
	}
	if settled.persistErr != nil || settled.verifyErr != nil {
		t.Fatalf("happy path must be clean, got %+v", settled)
	}

	// The persisted section: cookie saved, integration enabled,
	// unrelated client credentials preserved.
	sections := w.recorded()
	if len(sections) != 1 {
		t.Fatalf("writer called %d times, want 1", len(sections))
	}
	got := sections[0]
	if !got.Enabled || got.Session != "pasted-cookie" || got.ClientID != "cid" || got.ClientSecret != "csec" {
		t.Fatalf("persisted section = %+v", got)
	}
	// whoami verified the same candidate section.
	probed := who.sections()
	if len(probed) != 1 || probed[0] != got {
		t.Fatalf("whoami probed %+v, want the persisted section", probed)
	}

	// Done phase greets by nickname; Enter continues to the root menu.
	if view := s.View().Content; !strings.Contains(view, "kawai-fan") {
		t.Fatalf("done view must show the nickname, got:\n%s", view)
	}
	if _, ok := cmdMsg(updateCmd(t, s, enter())).(replaceMsg); !ok {
		t.Fatalf("enter on done must replace with root")
	}
}

// TestShikiCookieScreenVerifyFailsButSaved pins the CLI-auth parity:
// the cookie is persisted first, a whoami failure renders a warning
// (not an error) and the flow still continues to the root menu.
func TestShikiCookieScreenVerifyFailsButSaved(t *testing.T) {
	w := &fakeSettingsWriter{}
	who := &fakeShikiWhoAmI{verifyEr: errors.New("HTTP 401")}
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, w, who)

	s := newShikiCookieScreen(deps)
	s.input.SetValue("ck")
	msg := settleCmd(t, s, enter())
	if m, ok := msg.(shikiSavedMsg); !ok || m.persistErr != nil || m.verifyErr == nil {
		t.Fatalf("want saved-with-verify-error, got %#v", msg)
	}
	if len(w.recorded()) != 1 {
		t.Fatalf("the cookie must be persisted before verification")
	}
	view := s.View().Content
	if !strings.Contains(view, "saved") || !strings.Contains(view, "401") {
		t.Fatalf("done view must show saved + warning, got:\n%s", view)
	}
	if _, ok := cmdMsg(updateCmd(t, s, enter())).(replaceMsg); !ok {
		t.Fatalf("enter must still continue to root")
	}
}

// TestShikiCookieScreenPersistFailure pins: a failing writer surfaces
// the error and Esc returns to the setup menu (nothing was saved).
func TestShikiCookieScreenPersistFailure(t *testing.T) {
	w := &fakeSettingsWriter{err: errors.New("disk full")}
	who := &fakeShikiWhoAmI{user: ShikiUser{ID: 1}}
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, w, who)

	s := newShikiCookieScreen(deps)
	s.input.SetValue("ck")
	msg := settleCmd(t, s, enter())
	if m, ok := msg.(shikiSavedMsg); !ok || m.persistErr == nil {
		t.Fatalf("want persist error, got %#v", msg)
	}
	if len(who.sections()) != 0 {
		t.Fatalf("a failed persist must not verify, probed %+v", who.sections())
	}
	if view := s.View().Content; !strings.Contains(view, "disk full") {
		t.Fatalf("error view must show the cause, got:\n%s", view)
	}
	if _, ok := cmdMsg(updateCmd(t, s, esc())).(popMsg); !ok {
		t.Fatalf("esc on error must pop to the setup menu")
	}
}

// TestShikiCookieScreenNilSeams pins loud degradation: without a writer
// or a whoami probe the screen surfaces an error instead of pretending
// success.
func TestShikiCookieScreenNilSeams(t *testing.T) {
	t.Run("no writer", func(t *testing.T) {
		s := newShikiCookieScreen(&Deps{ShikiCfg: config.Shikimori{Enabled: true}})
		s.input.SetValue("ck")
		msg := settleCmd(t, s, enter())
		if m, ok := msg.(shikiSavedMsg); !ok || m.persistErr == nil {
			t.Fatalf("want a persist error, got %#v", msg)
		}
	})
	t.Run("no whoami probe", func(t *testing.T) {
		w := &fakeSettingsWriter{}
		s := newShikiCookieScreen(&Deps{
			ShikiCfg:       config.Shikimori{Enabled: true},
			SettingsWriter: w.write,
		})
		s.input.SetValue("ck")
		msg := settleCmd(t, s, enter())
		if m, ok := msg.(shikiSavedMsg); !ok || m.verifyErr == nil {
			t.Fatalf("want a verify error, got %#v", msg)
		}
	})
}

// TestShikiCookieScreenEscAtInput pins I2: Esc before submitting pops
// back to the setup menu with nothing written.
func TestShikiCookieScreenEscAtInput(t *testing.T) {
	w := &fakeSettingsWriter{}
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, w, &fakeShikiWhoAmI{})
	s := newShikiCookieScreen(deps)
	s.input.SetValue("ck")
	if _, ok := cmdMsg(updateCmd(t, s, esc())).(popMsg); !ok {
		t.Fatalf("esc at input must pop")
	}
	if len(w.recorded()) != 0 {
		t.Fatalf("nothing must be written on esc")
	}
}

// fakeShikiOAuth captures the flow-start seam.
type fakeShikiOAuth struct {
	mu        sync.Mutex
	started   int
	gotID     string
	gotSecret string
	url       string
	startErr  error
	resolveF  func(ctx context.Context) (ShikiOAuthResult, error)
}

func (f *fakeShikiOAuth) start(clientID, clientSecret string, _ int) (string, func(context.Context) (ShikiOAuthResult, error), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	f.gotID = clientID
	f.gotSecret = clientSecret
	resolve := f.resolveF
	if resolve == nil {
		resolve = func(context.Context) (ShikiOAuthResult, error) {
			return ShikiOAuthResult{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: 123456}, nil
		}
	}
	return f.url, resolve, f.startErr
}

// oauthDeps wires cookie-style deps plus the OAuth seam.
func oauthDeps(cfg config.Shikimori, w *fakeSettingsWriter, who *fakeShikiWhoAmI, flow *fakeShikiOAuth) *Deps {
	deps := shikiSetupDeps(cfg, w, who)
	deps.ShikiOAuth = flow.start
	return deps
}

// settleFrom feeds each produced message back into the screen (what
// the bubbletea runtime does) until a terminal phase swallows it.
func settleFrom(t *testing.T, s *shikiOAuthScreen, msg tea.Msg) tea.Msg {
	t.Helper()
	for msg != nil && s.phase != shikiOAuthDone && s.phase != shikiOAuthError {
		_, cmd := s.Update(msg)
		msg = cmdMsg(cmd)
	}
	return msg
}

// driveOAuth settles the whole ready→wait→save chain from Init.
func driveOAuth(t *testing.T, s *shikiOAuthScreen) tea.Msg {
	t.Helper()
	return settleFrom(t, s, cmdMsg(s.Init()))
}

// TestShikiOAuthScreenPrefilledCreds pins: configured client
// credentials skip the inputs, the flow starts on Init, tokens are
// persisted with the env-cookie cleared (bearer wins; no env secret
// ever bakes into the file), and the user is greeted by nickname.
func TestShikiOAuthScreenPrefilledCreds(t *testing.T) {
	w := &fakeSettingsWriter{}
	who := &fakeShikiWhoAmI{user: ShikiUser{ID: 42, Nickname: "oauth-fan"}}
	flow := &fakeShikiOAuth{url: "https://shikimori.io/oauth/authorize?client_id=cid"}
	// Session simulates an env-provided cookie the compose step must
	// NOT write into the file.
	deps := oauthDeps(config.Shikimori{Enabled: true, ClientID: "cid", ClientSecret: "csec", Session: "env-cookie"}, w, who, flow)

	s := newShikiOAuthScreen(deps)
	if msg := driveOAuth(t, s); msg != nil {
		t.Fatalf("flow must settle cleanly, got %#v", msg)
	}
	if s.phase != shikiOAuthDone {
		t.Fatalf("phase = %v, want done", s.phase)
	}
	if flow.started != 1 {
		t.Fatalf("flow started %d times, want 1", flow.started)
	}

	sections := w.recorded()
	if len(sections) != 1 {
		t.Fatalf("writer called %d times, want 1", len(sections))
	}
	got := sections[0]
	if !got.Enabled || got.AccessToken != "at-1" || got.RefreshToken != "rt-1" ||
		got.TokenExpiresAt != 123456 || got.ClientID != "cid" || got.ClientSecret != "csec" {
		t.Fatalf("persisted section = %+v", got)
	}
	if got.Session != "" {
		t.Fatalf("the env-provided session must not bake into the file, got %q", got.Session)
	}

	view := s.View().Content
	if !strings.Contains(view, "tokens saved") || !strings.Contains(view, "oauth-fan") {
		t.Fatalf("done view must show saved tokens + nickname, got:\n%s", view)
	}
	if _, ok := cmdMsg(updateCmd(t, s, enter())).(replaceMsg); !ok {
		t.Fatalf("enter on done must replace with root")
	}
}

// TestShikiOAuthScreenInputs pin: absent credentials collect client_id
// then client_secret before starting the flow with the typed pair.
func TestShikiOAuthScreenInputs(t *testing.T) {
	flow := &fakeShikiOAuth{url: "https://shikimori.io/oauth/authorize"}
	deps := oauthDeps(config.Shikimori{Enabled: true}, &fakeSettingsWriter{}, &fakeShikiWhoAmI{}, flow)

	s := newShikiOAuthScreen(deps)
	if cmd := s.Init(); cmd != nil {
		t.Fatalf("no prefilled creds: Init must not start the flow, got %#v", cmdMsg(cmd))
	}
	if view := s.View().Content; !strings.Contains(view, "client_id") {
		t.Fatalf("first input must ask for client_id, got:\n%s", view)
	}

	s.idInput.SetValue("typed-id")
	if _, cmd := s.Update(enter()); cmdMsg(cmd) != nil {
		t.Fatalf("id submit schedules nothing by itself, got %#v", cmdMsg(cmd))
	}
	if view := s.View().Content; !strings.Contains(view, "client_secret") {
		t.Fatalf("second input must ask for client_secret, got:\n%s", view)
	}

	s.secretInput.SetValue("typed-secret")
	settleFrom(t, s, cmdMsg(updateCmd(t, s, enter())))
	if s.phase != shikiOAuthDone {
		t.Fatalf("phase = %v, want done (view:\n%s)", s.phase, s.View().Content)
	}
	if flow.gotID != "typed-id" || flow.gotSecret != "typed-secret" {
		t.Fatalf("flow started with (%q, %q), want the typed pair", flow.gotID, flow.gotSecret)
	}
}

// TestShikiOAuthScreenResolveFailure pins: a failed wait/exchange
// surfaces the error; Esc returns to the setup menu with nothing
// written.
func TestShikiOAuthScreenResolveFailure(t *testing.T) {
	w := &fakeSettingsWriter{}
	flow := &fakeShikiOAuth{
		url: "https://shikimori.io/oauth/authorize",
		resolveF: func(context.Context) (ShikiOAuthResult, error) {
			return ShikiOAuthResult{}, errors.New("invalid_grant")
		},
	}
	deps := oauthDeps(config.Shikimori{Enabled: true, ClientID: "cid", ClientSecret: "csec"}, w, &fakeShikiWhoAmI{}, flow)

	s := newShikiOAuthScreen(deps)
	driveOAuth(t, s)
	if s.phase != shikiOAuthError {
		t.Fatalf("phase = %v, want error", s.phase)
	}
	if view := s.View().Content; !strings.Contains(view, "invalid_grant") {
		t.Fatalf("error view must show the cause, got:\n%s", view)
	}
	if len(w.recorded()) != 0 {
		t.Fatalf("a failed exchange must write nothing")
	}
	if _, ok := cmdMsg(updateCmd(t, s, esc())).(popMsg); !ok {
		t.Fatalf("esc on error must pop to the setup menu")
	}
}

// TestShikiOAuthScreenEscCancelsWait pins: Esc while waiting for the
// browser callback cancels the resolve context and pops.
func TestShikiOAuthScreenEscCancelsWait(t *testing.T) {
	canceled := make(chan struct{}, 1)
	flow := &fakeShikiOAuth{
		url: "https://shikimori.io/oauth/authorize",
		resolveF: func(ctx context.Context) (ShikiOAuthResult, error) {
			<-ctx.Done()
			canceled <- struct{}{}
			return ShikiOAuthResult{}, ctx.Err()
		},
	}
	deps := oauthDeps(config.Shikimori{Enabled: true, ClientID: "cid", ClientSecret: "csec"},
		&fakeSettingsWriter{}, &fakeShikiWhoAmI{}, flow)

	s := newShikiOAuthScreen(deps)
	// Start the flow and arrive at the running phase.
	msg := cmdMsg(s.Init())
	if msg == nil {
		t.Fatal("Init must start the prefilled flow")
	}
	_, cmd := s.Update(msg)
	if cmd == nil {
		t.Fatal("the ready message must schedule the wait")
	}
	// The wait blocks until cancelled: run it in the background like
	// the bubbletea runtime would.
	go func() { _ = cmdMsg(cmd) }()

	if view := s.View().Content; !strings.Contains(view, flow.url) {
		t.Fatalf("running view must show the authorize URL, got:\n%s", view)
	}

	if _, ok := cmdMsg(updateCmd(t, s, esc())).(popMsg); !ok {
		t.Fatalf("esc while waiting must pop")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("esc must cancel the resolve context")
	}
}

// TestShikiOAuthScreenNilSeam pins loud degradation without the flow.
func TestShikiOAuthScreenNilSeam(t *testing.T) {
	deps := shikiSetupDeps(config.Shikimori{Enabled: true, ClientID: "cid", ClientSecret: "csec"},
		&fakeSettingsWriter{}, &fakeShikiWhoAmI{})
	s := newShikiOAuthScreen(deps)
	msg := cmdMsg(s.Init())
	if m, ok := msg.(shikiOAuthReadyMsg); !ok || m.err == nil {
		t.Fatalf("want a start error, got %#v", msg)
	}
	_, _ = s.Update(msg)
	if view := s.View().Content; !strings.Contains(view, "unavailable") {
		t.Fatalf("error view must say the flow is unavailable, got:\n%s", view)
	}
}

// TestShikiSetupMenu pins the mandatory auth selection screen: the
// red warning header, the two auth choices (no Back, no Skip — auth
// is REQUIRED), cookie/oauth pushes, Esc/Ctrl-C quit the app (the
// user cannot bypass authorization).
func TestShikiSetupMenu(t *testing.T) {
	deps := shikiSetupDeps(config.Shikimori{Enabled: true}, &fakeSettingsWriter{}, &fakeShikiWhoAmI{})
	s := NewShikimoriSetup(deps)

	t.Run("view carries warning and choices", func(t *testing.T) {
		view := s.View().Content
		for _, want := range []string{
			"Shikimori is not configured",
			"authorization is required",
			"🔑 Cookie (paste the _kawai_session value from your browser)",
			"🔐 OAuth2 (open the browser to authorize)",
		} {
			if !strings.Contains(view, want) {
				t.Fatalf("setup view must contain %q, got:\n%s", want, view)
			}
		}
		for _, banned := range []string{BackLabel(), "Пропустить"} {
			if strings.Contains(view, banned) {
				t.Fatalf("setup view must NOT contain %q (auth is mandatory), got:\n%s", banned, view)
			}
		}
	})

	t.Run("esc and ctrl+c quit (auth is mandatory)", func(t *testing.T) {
		for _, key := range []tea.KeyPressMsg{esc(), ctrlC()} {
			m := NewShikimoriSetup(deps)
			cmd := updateCmd(t, m, key)
			if !isQuitCmd(cmd) {
				t.Fatalf("interrupt at setup must QUIT (no skip path), got %#v", cmdMsg(cmd))
			}
		}
	})

	t.Run("cookie pushes the cookie screen", func(t *testing.T) {
		m := NewShikimoriSetup(deps)
		m.list.Jump(indexOfSetupChoice(m, "cookie"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != shikiCookieID {
			t.Fatalf("cookie pick must push the cookie screen, got %#v", msg)
		}
	})

	t.Run("oauth pushes the oauth screen", func(t *testing.T) {
		m := NewShikimoriSetup(deps)
		m.list.Jump(indexOfSetupChoice(m, "oauth"))
		msg := cmdMsg(updateCmd(t, m, enter()))
		pm, ok := msg.(pushMsg)
		if !ok || pm.screen.ID() != shikiOAuthID {
			t.Fatalf("oauth pick must push the oauth screen, got %#v", msg)
		}
	})
}

// indexOfSetupChoice finds a choice index in the setup menu.
func indexOfSetupChoice(s *ShikiSetupScreen, id string) int {
	for i, c := range s.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// TestShikimoriNeedsSetup pins the first-run gate: enabled without
// any credential.
func TestShikimoriNeedsSetup(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Shikimori
		want bool
	}{
		{"enabled without credentials", config.Shikimori{Enabled: true}, true},
		{"enabled with session", config.Shikimori{Enabled: true, Session: "s"}, false},
		{"enabled with token", config.Shikimori{Enabled: true, AccessToken: "t"}, false},
		{"disabled", config.Shikimori{Enabled: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShikimoriNeedsSetup(&Deps{ShikiCfg: tc.cfg}); got != tc.want {
				t.Fatalf("ShikimoriNeedsSetup = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("nil deps stays closed", func(t *testing.T) {
		if ShikimoriNeedsSetup(nil) {
			t.Fatal("nil deps must not trigger the setup gate")
		}
	})
}

// TestInitialStack pins the auth gate: the setup screen IS the whole
// stack when Shikimori is unconfigured (auth is mandatory — the root
// menu is not reachable until authorization completes), and the root
// menu opens directly when configured.
func TestInitialStack(t *testing.T) {
	t.Run("needs setup — auth-only stack", func(t *testing.T) {
		deps := &Deps{ShikiCfg: config.Shikimori{Enabled: true}}
		ids := screenIDs(initialStack(deps))
		if len(ids) != 1 || ids[0] != shikiSetupID {
			t.Fatalf("want [shikimori_setup] (auth is mandatory, no root below), got %v", ids)
		}
	})
	t.Run("configured opens at root", func(t *testing.T) {
		deps := &Deps{ShikiCfg: config.Shikimori{Enabled: true, Session: "s"}}
		ids := screenIDs(initialStack(deps))
		if len(ids) != 1 || ids[0] != rootScreenID {
			t.Fatalf("want [root], got %v", ids)
		}
	})
}

// TestNewAppOverlays pins the NewApp overlay variadic: pushed screens
// form the opening stack and Init runs the TOP screen's Init.
func TestNewAppOverlays(t *testing.T) {
	root := &countingScreen{id: "root"}
	overlay := &countingScreen{id: "setup"}
	app := NewApp(root, nil, testLogger(), overlay)

	if len(app.stack) != 2 || app.stack[0].ID() != "root" || app.stack[1].ID() != "setup" {
		t.Fatalf("want [root setup], got %v", screenIDs(app.stack))
	}
	if cmd := app.Init(); cmd != nil {
		t.Fatalf("countingScreen Init is nil; the app must run the TOP screen's Init without error, got %#v", cmdMsg(cmd))
	}
	if view := app.View().Content; !strings.Contains(view, "screen setup") {
		t.Fatalf("only the top screen renders, got %q", view)
	}
}
