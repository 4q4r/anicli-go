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
	"fmt"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
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
	errShikiSettingsUnavailable = errors.New("сохранение настроек недоступно (сборка без писателя settings.toml)")
	errShikiWhoAmIUnavailable   = errors.New("проверка учётных данных недоступна (сборка без whoami-пробы)")
	errShikiOAuthUnavailable    = errors.New("OAuth2-поток недоступен (сборка без авторизации)")
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
		return theme.Warning.Render("⚠ Проверка не удалась: " + err.Error())
	}
	if user.Nickname != "" {
		return theme.Success.Render("Пользователь Shikimori: " + user.Nickname)
	}
	return theme.Success.Render(fmt.Sprintf("Пользователь Shikimori: id %d", user.ID))
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
	input.Placeholder = "значение cookie из браузера"
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
				s.failLine = "✗ Не удалось сохранить настройки: " + settled.persistErr.Error()
			} else {
				s.phase = shikiPhaseDone
			}
		}
		return s, nil
	case shikiPhaseDone:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, popToRoot()
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
	b = append(b, theme.Title.Render("🔑 Cookie _kawai_session")...)
	b = append(b, '\n', '\n')
	switch s.phase {
	case shikiPhaseInput:
		b = append(b, "Вставьте значение cookie _kawai_session:\n\n"...)
		b = append(b, s.input.View()...)
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render("enter — сохранить и проверить · esc — назад")...)
	case shikiPhaseBusy:
		b = append(b, theme.Dim.Render("⏳ Сохранение и проверка cookie…")...)
	case shikiPhaseDone:
		b = append(b, theme.Success.Render("✓ Cookie сохранён в settings.toml")...)
		b = append(b, '\n', '\n')
		b = append(b, shikiUserLine(s.settled.user, s.settled.verifyErr)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("enter — продолжить")...)
	default:
		b = append(b, theme.Error.Render(s.failLine)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("esc — назад")...)
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
				s.failLine = "✗ Не удалось сохранить токены: " + settled.persistErr.Error()
			} else {
				s.phase = shikiOAuthDone
			}
		}
		return s, nil
	case shikiOAuthDone:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, popToRoot()
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
			s.failLine = "✗ OAuth2 не удалось начать: " + m.err.Error()
			return s, nil
		}
		s.authURL = m.authURL
		s.resolve = m.resolve
		return s, s.waitCmd()
	case shikiOAuthTokensMsg:
		if m.err != nil {
			s.phase = shikiOAuthError
			s.failLine = "✗ OAuth2 не завершён: " + m.err.Error()
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
	switch s.phase {
	case shikiOAuthInputID, shikiOAuthInputSecret:
		b = append(b, theme.Title.Render("🔐 OAuth2 авторизация")...)
		b = append(b, '\n', '\n')
		if s.phase == shikiOAuthInputID {
			b = append(b, "client_id приложения (создайте на https://shikimori.io/apps):\n\n"...)
			b = append(b, s.idInput.View()...)
		} else {
			b = append(b, "client_secret приложения:\n\n"...)
			b = append(b, s.secretInput.View()...)
		}
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render("enter — далее · esc — назад")...)
	case shikiOAuthRunning:
		b = append(b, theme.Title.Render("🔐 OAuth2 авторизация")...)
		b = append(b, '\n', '\n')
		b = append(b, "Откройте в браузере и разрешите доступ:\n\n"...)
		if s.authURL != "" {
			b = append(b, theme.Accent.Render(s.authURL)...)
			b = append(b, '\n', '\n')
		}
		b = append(b, theme.Dim.Render("⏳ Ожидание ответа из браузера (до 5 минут)…")...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("esc — отменить")...)
	case shikiOAuthSaving:
		b = append(b, theme.Title.Render("🔐 OAuth2 авторизация")...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Dim.Render("⏳ Сохранение токенов и проверка…")...)
	case shikiOAuthDone:
		b = append(b, theme.Title.Render("🔐 OAuth2 авторизация")...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Success.Render("✓ OAuth2 токены сохранены в settings.toml (access-токен живёт сутки, продление автоматическое)")...)
		b = append(b, '\n', '\n')
		b = append(b, shikiUserLine(s.settled.user, s.settled.verifyErr)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("enter — продолжить")...)
	default:
		b = append(b, theme.Title.Render("🔐 OAuth2 авторизация")...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Error.Render(s.failLine)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("esc — назад")...)
	}
	return tea.NewView(string(b))
}

// shikiSetupID is the selection screen identity.
const shikiSetupID = "shikimori_setup"

// ShikiSetupScreen is the PR26 first-run selection menu: the red
// warning header plus the three auth choices. It is backless (no
// «Назад» row) — «Пропустить» is the explicit escape and the I2
// interrupt keys normalize to the same skip semantics (a pop), never
// an app exit: only the root menu's Ctrl-C quits.
type ShikiSetupScreen struct {
	deps *Deps
	list *PinList
}

// NewShikimoriSetup builds the selection screen.
func NewShikimoriSetup(deps *Deps) *ShikiSetupScreen {
	menu := NewMenuWithoutBack(
		"⚠ Shikimori не настроен — синхронизация списка отключена",
		"",
		Choice{ID: "cookie", Label: "🔑 Cookie (вставить _kawai_session из браузера)"},
		Choice{ID: "oauth", Label: "🔐 OAuth2 (открыть браузер для авторизации)"},
		Choice{ID: "skip", Label: "⏭  Пропустить (настроить позже: anicli shikimori auth)"},
	)
	return &ShikiSetupScreen{deps: deps, list: NewPinList(menu, defaultListHeight)}
}

// ID implements Screen.
func (s *ShikiSetupScreen) ID() string { return shikiSetupID }

// Init implements Screen.
func (s *ShikiSetupScreen) Init() tea.Cmd { return nil }

// Update implements Screen: movement drives the list, Enter resolves
// the pick, cancel keys normalize to skip (pop).
func (s *ShikiSetupScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	if s.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), key)
	switch pick := resolved.(type) {
	case nil:
		return s, nil
	case *backToken:
		// Esc/Ctrl-C: the same semantics as Пропустить (PR26 keeps
		// the I2 root exception at the ROOT menu only).
		return s, pop()
	case string:
		switch pick {
		case "cookie":
			return s, push(newShikiCookieScreen(s.deps))
		case "oauth":
			return s, push(newShikiOAuthScreen(s.deps))
		default: // "skip"
			return s, pop()
		}
	default:
		return s, nil
	}
}

// View implements Screen: red warning header, the subtitle, the list.
func (s *ShikiSetupScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Error.Render("⚠ Shikimori не настроен — синхронизация списка отключена")...)
	b = append(b, '\n', '\n')
	b = append(b, theme.Item.Render("Выберите способ авторизации:")...)
	b = append(b, '\n', '\n')
	b = append(b, s.list.Render()...)
	b = append(b, '\n')
	b = append(b, theme.StatusLine.Render("enter — выбрать · esc — пропустить")...)
	return tea.NewView(string(b))
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
