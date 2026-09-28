package tui

// The PR26 first-run Shikimori setup flow: when the integration is
// enabled but carries neither a session cookie nor an OAuth token,
// Run() pushes the selection screen (Cookie / OAuth2 / Пропустить) on
// top of the root menu. Each sub-screen persists its result through
// Deps.SettingsWriter and verifies the credentials through
// Deps.ShikiWhoAmI before greeting the user.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// shikiVerifyBudget bounds one whoami verification round-trip: the
// configured search timeout, else 30s.
func (d *Deps) shikiVerifyBudget() time.Duration {
	if d != nil && d.SearchTimeout > 0 {
		return d.SearchTimeout
	}
	return 30 * time.Second
}

// ShikiUser is the whoami identity the setup flows greet the user
// with (PR26).
type ShikiUser struct {
	// ID is the Shikimori user id.
	ID int64
	// Nickname is the display name; "" when the reply carried none
	// (callers render the id fallback).
	Nickname string
}

// ShikiOAuthResult is the settled outcome of the OAuth2 code
// exchange.
type ShikiOAuthResult struct {
	// AccessToken and RefreshToken are the issued OAuth2 pair.
	AccessToken, RefreshToken string
	// ExpiresAt is the unix timestamp when AccessToken expires.
	ExpiresAt int64
}

// Loud-degradation sentinels for the nil-seam cases (embedded builds
// and tests that wire no persistence): the screens surface these
// instead of pretending success.
var (
	errShikiSettingsUnavailable = errors.New("settings persistence unavailable (build without a settings.toml writer)")
	errShikiWhoAmIUnavailable   = errors.New("credential verification unavailable (build without the whoami probe)")
	errShikiOAuthUnavailable    = errors.New("OAuth2 flow unavailable (build without authorization)")
)

// shikiSavedMsg settles both setup flows' persist+verify step.
type shikiSavedMsg struct {
	// persistErr is set when the settings write failed (nothing was
	// verified in that case).
	persistErr error
	// user and verifyErr carry the whoami answer; a verify failure
	// after a successful persist renders as a warning, not an error
	// (CLI-auth parity).
	user      ShikiUser
	verifyErr error
}

// shikiUserLine renders the whoami result: the nickname (or the id
// fallback) on success, the failure warning otherwise.
func shikiUserLine(user ShikiUser, err error) string {
	if err != nil {
		return theme.Warning.Render(i18n.T("shiki.verify_failed", i18n.Vals{"err": err.Error()}))
	}
	if user.Nickname != "" {
		return theme.Success.Render(i18n.T("shiki.user_nick", i18n.Vals{"nick": user.Nickname}))
	}
	return theme.Success.Render(i18n.T("shiki.user_id", i18n.Vals{"id": strconv.FormatInt(user.ID, 10)}))
}

// shikiSaveAndVerify persists section and verifies it with one whoami
// round-trip (nil seams degrade loudly). Shared by the cookie and
// OAuth flows.
func shikiSaveAndVerify(deps *Deps, section config.Shikimori) tea.Cmd {
	return func() tea.Msg {
		if deps.SettingsWriter == nil {
			return shikiSavedMsg{persistErr: errShikiSettingsUnavailable}
		}
		if err := deps.SettingsWriter(section); err != nil {
			return shikiSavedMsg{persistErr: err}
		}
		if deps.ShikiWhoAmI == nil {
			return shikiSavedMsg{verifyErr: errShikiWhoAmIUnavailable}
		}
		ctx, cancel := context.WithTimeout(context.Background(), deps.shikiVerifyBudget())
		defer cancel()
		user, err := deps.ShikiWhoAmI(ctx, section)
		return shikiSavedMsg{user: user, verifyErr: err}
	}
}

// shikiSetupPhase is one lifecycle stage of the setup sub-screens.
type shikiSetupPhase int

const (
	// shikiPhaseInput collects user text.
	shikiPhaseInput shikiSetupPhase = iota
	// shikiPhaseBusy runs the persist/verify or OAuth step.
	shikiPhaseBusy
	// shikiPhaseDone shows the settled result (Enter continues to the
	// root menu).
	shikiPhaseDone
	// shikiPhaseError shows a failure (Esc returns to the setup
	// menu).
	shikiPhaseError
)

// shikiCookieScreen walks the cookie path: paste _kawai_session →
// persist [shikimori] session → whoami → greeting (PR26).
type shikiCookieScreen struct {
	deps     *Deps
	input    textinput.Model
	phase    shikiSetupPhase
	settled  shikiSavedMsg
	failLine string // error text of the error phase
}

// shikiCookieID is the screen identity.
const shikiCookieID = "shikimori_cookie"

// newShikiCookieScreen builds the cookie input phase.
func newShikiCookieScreen(deps *Deps) *shikiCookieScreen {
	input := textinput.New()
	input.Placeholder = i18n.T("shiki.cookie_placeholder")
	input.Focus()
	return &shikiCookieScreen{deps: deps, input: input, phase: shikiPhaseInput}
}

// ID implements Screen.
func (s *shikiCookieScreen) ID() string { return shikiCookieID }

// Init implements Screen.
func (s *shikiCookieScreen) Init() tea.Cmd { return nil }

// Update implements Screen: the input phase resolves through
// ResolveText (Esc/Ctrl-C/empty → Back); the settled phases continue
// or return per their verdict.
func (s *shikiCookieScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch s.phase {
	case shikiPhaseInput:
		if key, ok := msg.(tea.KeyPressMsg); ok {
			resolved := ResolveText(key, s.input.Value())
			switch v := resolved.(type) {
			case *backToken:
				return s, pop()
			case string:
				// Persist first, verify after (CLI-auth parity: the
				// cookie survives a whoami hiccup).
				section := s.deps.ShikiCfg
				section.Enabled = true
				section.Session = v
				s.phase = shikiPhaseBusy
				return s, shikiSaveAndVerify(s.deps, section)
			}
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	case shikiPhaseBusy:
		if settled, ok := msg.(shikiSavedMsg); ok {
			s.settled = settled
			if settled.persistErr != nil {
				s.phase = shikiPhaseError
				s.failLine = i18n.T("shiki.settings_save_failed", i18n.Vals{"err": settled.persistErr.Error()})
			} else {
				s.phase = shikiPhaseDone
			}
		}
		return s, nil
	case shikiPhaseDone:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, replace(afterAuthScreen(s.deps))
		}
		return s, nil
	default: // shikiPhaseError
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, pop()
		}
		return s, nil
	}
}

// View implements Screen.
func (s *shikiCookieScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render(i18n.T("shiki.cookie_title"))...)
	b = append(b, '\n', '\n')
	switch s.phase {
	case shikiPhaseInput:
		b = append(b, i18n.T("shiki.cookie_prompt")...)
		b = append(b, s.input.View()...)
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("shiki.cookie_save_hint"))...)
	case shikiPhaseBusy:
		b = append(b, theme.Dim.Render(i18n.T("shiki.cookie_busy"))...)
	case shikiPhaseDone:
		b = append(b, theme.Success.Render(i18n.T("shiki.cookie_done"))...)
		b = append(b, '\n', '\n')
		b = append(b, shikiUserLine(s.settled.user, s.settled.verifyErr)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.enter_continue"))...)
	default:
		b = append(b, theme.Error.Render(s.failLine)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.esc_back"))...)
	}
	return tea.NewView(string(b))
}

// shikiOAuthWait bounds the whole browser round-trip: five minutes
// from the authorize URL to the code (CLI auth parity).
const shikiOAuthWait = 5 * time.Minute

// shikiOAuthReadyMsg settles the flow start: the authorize URL plus
// the blocking resolve, or the start failure.
type shikiOAuthReadyMsg struct {
	authURL string
	resolve func(ctx context.Context) (ShikiOAuthResult, error)
	err     error
}

// shikiOAuthTokensMsg settles the wait+exchange step.
type shikiOAuthTokensMsg struct {
	set ShikiOAuthResult
	err error
}

// shikiOAuthPhase is the OAuth sub-screen lifecycle.
type shikiOAuthPhase int

const (
	// shikiOAuthInputID collects the application client_id.
	shikiOAuthInputID shikiOAuthPhase = iota
	// shikiOAuthInputSecret collects the application client_secret.
	shikiOAuthInputSecret
	// shikiOAuthRunning shows the authorize URL and waits for the
	// browser callback.
	shikiOAuthRunning
	// shikiOAuthSaving persists the tokens and verifies them.
	shikiOAuthSaving
	// shikiOAuthDone greets the user (Enter continues to root).
	shikiOAuthDone
	// shikiOAuthError shows the failure (Esc returns to the setup
	// menu).
	shikiOAuthError
)

// shikiOAuthScreen walks the OAuth2 path (PR26): optional client
// credential inputs → loopback flow → token persistence → greeting.
// The flow machinery itself is injected through Deps.ShikiOAuth (the
// CLI reuses the `anicli shikimori auth` loopback server + exchange).
type shikiOAuthScreen struct {
	deps *Deps
	// idInput and secretInput collect the application credentials
	// when the config carries none.
	idInput     textinput.Model
	secretInput textinput.Model
	// clientID and clientSecret are the credentials the flow runs
	// with.
	clientID, clientSecret string
	phase                  shikiOAuthPhase
	authURL                string
	resolve                func(ctx context.Context) (ShikiOAuthResult, error)
	cancel                 context.CancelFunc
	settled                shikiSavedMsg
	failLine               string
}

// shikiOAuthID is the screen identity.
const shikiOAuthID = "shikimori_oauth"

// newShikiOAuthScreen builds the OAuth sub-screen. Fully configured
// application credentials skip the inputs and start the flow on Init.
func newShikiOAuthScreen(deps *Deps) *shikiOAuthScreen {
	s := &shikiOAuthScreen{deps: deps, phase: shikiOAuthInputID}
	s.idInput = textinput.New()
	s.idInput.Placeholder = "client_id (https://shikimori.io/apps)"
	s.idInput.SetValue(deps.ShikiCfg.ClientID)
	s.idInput.Focus()
	s.secretInput = textinput.New()
	s.secretInput.Placeholder = "client_secret"
	s.secretInput.SetValue(deps.ShikiCfg.ClientSecret)
	if deps.ShikiCfg.ClientID != "" && deps.ShikiCfg.ClientSecret != "" {
		s.clientID, s.clientSecret = deps.ShikiCfg.ClientID, deps.ShikiCfg.ClientSecret
		s.phase = shikiOAuthRunning
	}
	return s
}

// ID implements Screen.
func (s *shikiOAuthScreen) ID() string { return shikiOAuthID }

// Init implements Screen: a fully prefilled flow starts immediately.
func (s *shikiOAuthScreen) Init() tea.Cmd {
	if s.phase == shikiOAuthRunning {
		return s.startCmd()
	}
	return nil
}

// startCmd launches the injected flow (nil seam degrades loudly).
func (s *shikiOAuthScreen) startCmd() tea.Cmd {
	deps := s.deps
	clientID, clientSecret := s.clientID, s.clientSecret
	return func() tea.Msg {
		if deps.ShikiOAuth == nil {
			return shikiOAuthReadyMsg{err: errShikiOAuthUnavailable}
		}
		url, resolve, err := deps.ShikiOAuth(clientID, clientSecret, 0)
		return shikiOAuthReadyMsg{authURL: url, resolve: resolve, err: err}
	}
}

// waitCmd blocks on the resolve under the flow budget; Esc cancels.
func (s *shikiOAuthScreen) waitCmd() tea.Cmd {
	resolve := s.resolve
	ctx, cancel := context.WithTimeout(context.Background(), shikiOAuthWait)
	s.cancel = cancel
	return func() tea.Msg {
		defer cancel()
		set, err := resolve(ctx)
		return shikiOAuthTokensMsg{set: set, err: err}
	}
}

// sectionFromTokens composes the persisted section: tokens + used
// application credentials, the session cookie explicitly cleared —
// bearer wins the mode dispatch anyway, and an env-provided cookie
// must never bake into the settings file.
func (s *shikiOAuthScreen) sectionFromTokens(set ShikiOAuthResult) config.Shikimori {
	section := s.deps.ShikiCfg
	section.Enabled = true
	section.Session = ""
	section.AccessToken = set.AccessToken
	section.RefreshToken = set.RefreshToken
	section.TokenExpiresAt = set.ExpiresAt
	section.ClientID = s.clientID
	section.ClientSecret = s.clientSecret
	return section
}

// Update implements Screen.
func (s *shikiOAuthScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch s.phase {
	case shikiOAuthInputID, shikiOAuthInputSecret:
		return s.updateInputs(msg)
	case shikiOAuthRunning:
		return s.updateRunning(msg)
	case shikiOAuthSaving:
		if settled, ok := msg.(shikiSavedMsg); ok {
			s.settled = settled
			if settled.persistErr != nil {
				s.phase = shikiOAuthError
				s.failLine = i18n.T("shiki.tokens_save_failed", i18n.Vals{"err": settled.persistErr.Error()})
			} else {
				s.phase = shikiOAuthDone
			}
		}
		return s, nil
	case shikiOAuthDone:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, replace(afterAuthScreen(s.deps))
		}
		return s, nil
	default: // shikiOAuthError
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, pop()
		}
		return s, nil
	}
}

// updateInputs handles the two credential prompts.
func (s *shikiOAuthScreen) updateInputs(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		if s.phase == shikiOAuthInputID {
			s.idInput, cmd = s.idInput.Update(msg)
		} else {
			s.secretInput, cmd = s.secretInput.Update(msg)
		}
		return s, cmd
	}
	active := &s.idInput
	if s.phase == shikiOAuthInputSecret {
		active = &s.secretInput
	}
	resolved := ResolveText(key, active.Value())
	switch v := resolved.(type) {
	case *backToken:
		return s, pop()
	case string:
		if s.phase == shikiOAuthInputID {
			s.clientID = v
			s.phase = shikiOAuthInputSecret
			s.idInput.Blur()
			s.secretInput.Focus()
			return s, nil
		}
		s.clientSecret = v
		s.phase = shikiOAuthRunning
		return s, s.startCmd()
	}
	var cmd tea.Cmd
	if s.phase == shikiOAuthInputID {
		s.idInput, cmd = s.idInput.Update(msg)
	} else {
		s.secretInput, cmd = s.secretInput.Update(msg)
	}
	return s, cmd
}

// updateRunning handles the started flow: URL delivery schedules the
// wait, the settled tokens schedule the persist, Esc cancels.
func (s *shikiOAuthScreen) updateRunning(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case shikiOAuthReadyMsg:
		if m.err != nil {
			s.phase = shikiOAuthError
			s.failLine = i18n.T("shiki.oauth_start_failed", i18n.Vals{"err": m.err.Error()})
			return s, nil
		}
		s.authURL = m.authURL
		s.resolve = m.resolve
		return s, s.waitCmd()
	case shikiOAuthTokensMsg:
		if m.err != nil {
			s.phase = shikiOAuthError
			s.failLine = i18n.T("shiki.oauth_finish_failed", i18n.Vals{"err": m.err.Error()})
			return s, nil
		}
		s.phase = shikiOAuthSaving
		return s, shikiSaveAndVerify(s.deps, s.sectionFromTokens(m.set))
	case tea.KeyPressMsg:
		if IsCancelKey(m) {
			if s.cancel != nil {
				s.cancel()
			}
			return s, pop()
		}
	}
	return s, nil
}

// View implements Screen.
func (s *shikiOAuthScreen) View() tea.View {
	var b []byte
	oauthTitle := i18n.T("shiki.oauth_title")
	switch s.phase {
	case shikiOAuthInputID, shikiOAuthInputSecret:
		b = append(b, theme.Title.Render(oauthTitle)...)
		b = append(b, '\n', '\n')
		if s.phase == shikiOAuthInputID {
			b = append(b, i18n.T("shiki.oauth_client_id_prompt")...)
			b = append(b, s.idInput.View()...)
		} else {
			b = append(b, i18n.T("shiki.oauth_client_secret_prompt")...)
			b = append(b, s.secretInput.View()...)
		}
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.enter_next_esc_back"))...)
	case shikiOAuthRunning:
		b = append(b, theme.Title.Render(oauthTitle)...)
		b = append(b, '\n', '\n')
		b = append(b, i18n.T("shiki.oauth_open_url")...)
		if s.authURL != "" {
			b = append(b, theme.Accent.Render(s.authURL)...)
			b = append(b, '\n', '\n')
		}
		b = append(b, theme.Dim.Render(i18n.T("shiki.oauth_waiting"))...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.esc_cancel"))...)
	case shikiOAuthSaving:
		b = append(b, theme.Title.Render(oauthTitle)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Dim.Render(i18n.T("shiki.oauth_saving"))...)
	case shikiOAuthDone:
		b = append(b, theme.Title.Render(oauthTitle)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Success.Render(i18n.T("shiki.oauth_done"))...)
		b = append(b, '\n', '\n')
		b = append(b, shikiUserLine(s.settled.user, s.settled.verifyErr)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.enter_continue"))...)
	default:
		b = append(b, theme.Title.Render(oauthTitle)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Error.Render(s.failLine)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.esc_back"))...)
	}
	return tea.NewView(string(b))
}

// shikiSetupID is the provider selection screen identity (PR112: the
// screen grew from the Shikimori-only method menu into the tracker
// provider menu; the identity stays — it is the first-run gate).
const shikiSetupID = "shikimori_setup"

// shikiMethodID is the Shikimori method submenu identity (PR112 split
// out of the old single menu: the provider menu now sits above it).
const shikiMethodID = "shikimori_method"

// ShikiSetupScreen is the PR112 provider selection menu: the red
// warning header plus one row per tracker with its live authorization
// status (Shikimori, MyAnimeList). It is backless (no «Назад» row) —
// Esc/I2 normalize to the same exit semantics as before (a pop, never
// an app exit): only the root menu's Ctrl-C quits. Shikimori
// authorization stays mandatory (the hybrid search requires it);
// MyAnimeList is the optional second tracker.
type ShikiSetupScreen struct {
	deps *Deps
	list *PinList
}

// shikiAuthed reports the shikimori authorization from the deps
// snapshot.
func shikiAuthed(cfg config.Shikimori) bool {
	return cfg.Session != "" || cfg.AccessToken != ""
}

// malAuthed reports the MAL authorization from the deps snapshot.
func malAuthed(cfg config.MAL) bool {
	return cfg.AccessToken != ""
}

// providerChoiceLabel composes one menu row: the tracker name plus the
// live authorization marker.
func providerChoiceLabel(name string, authed bool) string {
	if authed {
		return name + " · " + i18n.T("setup.auth_ok")
	}
	return name + " · " + i18n.T("setup.auth_no")
}

// NewShikimoriSetup builds the provider selection screen with live
// authorization markers from the deps snapshots.
func NewShikimoriSetup(deps *Deps) *ShikiSetupScreen {
	menu := NewMenuWithoutBack(
		i18n.T("shiki.setup_title"),
		i18n.T("setup.pick_provider"),
		Choice{ID: "shikimori", Label: providerChoiceLabel(
			i18n.T("setup.shiki_choice"), shikiAuthed(deps.ShikiCfg))},
		Choice{ID: "myanimelist", Label: providerChoiceLabel(
			i18n.T("setup.mal_choice"), malAuthed(deps.MALCfg))},
	)
	return &ShikiSetupScreen{deps: deps, list: NewPinList(menu, defaultListHeight)}
}

// ID implements Screen.
func (s *ShikiSetupScreen) ID() string { return shikiSetupID }

// Init implements Screen.
func (s *ShikiSetupScreen) Init() tea.Cmd { return nil }

// Update implements Screen: movement drives the list, Enter resolves
// the pick. Shikimori auth is MANDATORY — Esc/Ctrl-C exits the app
// (the search requires Shikimori for hybrid enrichment; skipping is
// not allowed).
func (s *ShikiSetupScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	if s.list.HandleKey(key) {
		return s, nil
	}
	if IsCancelKey(key) {
		return s, quit()
	}
	resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), key)
	switch pick := resolved.(type) {
	case nil:
		return s, nil
	case *backToken:
		// The menu is backless; backToken shouldn't appear. Treat as
		// cancel → exit (auth is mandatory).
		return s, quit()
	case string:
		switch pick {
		case "shikimori":
			return s, push(newShikiMethodScreen(s.deps))
		case "myanimelist":
			return s, push(newMALOAuthScreen(s.deps))
		}
	}
	return s, nil
}

// View implements Screen: red warning header, the subtitle, the list.
func (s *ShikiSetupScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Error.Render(i18n.T("shiki.setup_warning"))...)
	b = append(b, '\n', '\n')
	b = append(b, theme.Item.Render(i18n.T("setup.pick_provider"))...)
	b = append(b, '\n', '\n')
	b = append(b, s.list.Render()...)
	b = append(b, '\n')
	b = append(b, theme.StatusLine.Render(i18n.T("setup.hint"))...)
	return tea.NewView(string(b))
}

// shikiMethodScreen is the Shikimori method submenu (PR112): the two
// auth paths of the Shikimori provider, reached from the provider
// menu. Esc pops back to it (unlike the backless provider gate).
type shikiMethodScreen struct {
	deps *Deps
	list *PinList
}

// newShikiMethodScreen builds the method submenu.
func newShikiMethodScreen(deps *Deps) *shikiMethodScreen {
	menu := NewMenu(
		i18n.T("shiki.setup_title"),
		"",
		Choice{ID: "cookie", Label: i18n.T("shiki.setup_cookie_choice")},
		Choice{ID: "oauth", Label: i18n.T("shiki.setup_oauth_choice")},
	)
	return &shikiMethodScreen{deps: deps, list: NewPinList(menu, defaultListHeight)}
}

// ID implements Screen.
func (s *shikiMethodScreen) ID() string { return shikiMethodID }

// Init implements Screen.
func (s *shikiMethodScreen) Init() tea.Cmd { return nil }

// Update implements Screen.
func (s *shikiMethodScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	if s.list.HandleKey(key) {
		return s, nil
	}
	if IsCancelKey(key) {
		return s, pop()
	}
	resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), key)
	switch pick := resolved.(type) {
	case nil:
		return s, nil
	case *backToken:
		return s, pop()
	case string:
		switch pick {
		case "cookie":
			return s, push(newShikiCookieScreen(s.deps))
		case "oauth":
			return s, push(newShikiOAuthScreen(s.deps))
		}
	}
	return s, nil
}

// View implements Screen.
func (s *shikiMethodScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Error.Render(i18n.T("shiki.setup_warning"))...)
	b = append(b, '\n', '\n')
	b = append(b, theme.Item.Render(i18n.T("shiki.setup_pick_method"))...)
	b = append(b, '\n', '\n')
	b = append(b, s.list.Render()...)
	b = append(b, '\n')
	b = append(b, theme.StatusLine.Render(i18n.T("common.esc_back"))...)
	return tea.NewView(string(b))
}

// newRootAfterAuth builds the root menu with the Shikimori notice
// stripped: the user just completed authentication, so the startup
// warning is stale and must not render on the root screen.
func newRootAfterAuth(deps *Deps) *rootScreen {
	filtered := make([]string, 0, len(deps.StartupNotices))
	for _, n := range deps.StartupNotices {
		if strings.Contains(n, "Shikimori") {
			continue
		}
		filtered = append(filtered, n)
	}
	d := *deps
	d.StartupNotices = filtered
	return NewRootScreen(&d)
}

// ShikimoriNeedsSetup reports the PR26 first-run gate: the tracker
// integration is enabled but carries neither a session cookie nor an
// OAuth token (the same condition the doctor startup notice warns
// about).
func ShikimoriNeedsSetup(deps *Deps) bool {
	return deps != nil &&
		deps.ShikiCfg.Enabled &&
		deps.ShikiCfg.Session == "" &&
		deps.ShikiCfg.AccessToken == ""
}
