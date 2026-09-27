package tui

// The PR112 MyAnimeList setup flow: the PKCE OAuth2 sub-screen reached
// from the provider menu. It persists its result through
// Deps.MALSettingsWriter ([mal] section) and verifies the credentials
// through Deps.MALWhoAmI before greeting the user — the same flow
// shape as the Shikimori OAuth screen, against the MAL wire contract
// (the token pair rides one loopback redirect; the code verifier
// answers the plain PKCE challenge).

import (
	"context"
	"strconv"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// MALUser is the whoami identity the MAL setup flow greets the user
// with.
type MALUser struct {
	// ID is the MyAnimeList user id.
	ID int64
	// Name is the display name; "" when the reply carried none
	// (callers render the id fallback).
	Name string
}

// MALOAuthResult is the settled outcome of the MAL OAuth2 code
// exchange.
type MALOAuthResult struct {
	// AccessToken and RefreshToken are the issued OAuth2 pair.
	AccessToken, RefreshToken string
	// ExpiresAt is the unix timestamp when AccessToken expires.
	ExpiresAt int64
}

// malSavedMsg settles the MAL persist+verify step. section carries the
// persisted section so the main goroutine can refresh the deps
// snapshot (the provider menu re-renders the authorization status from
// it).
type malSavedMsg struct {
	section config.MAL
	// persistErr is set when the settings write failed (nothing was
	// verified in that case).
	persistErr error
	// user and verifyErr carry the whoami answer; a verify failure
	// after a successful persist renders as a warning, not an error
	// (shiki flow parity).
	user      MALUser
	verifyErr error
}

// malUserLine renders the whoami result.
func malUserLine(user MALUser, err error) string {
	if err != nil {
		return theme.Warning.Render(i18n.T("mal.verify_failed", i18n.Vals{"err": err.Error()}))
	}
	if user.Name != "" {
		return theme.Success.Render(i18n.T("mal.user_name", i18n.Vals{"name": user.Name}))
	}
	return theme.Success.Render(i18n.T("mal.user_id", i18n.Vals{"id": strconv.FormatInt(user.ID, 10)}))
}

// malSaveAndVerify persists the [mal] section and verifies it with one
// whoami round-trip (nil seams degrade loudly).
func malSaveAndVerify(deps *Deps, section config.MAL) tea.Cmd {
	return func() tea.Msg {
		if deps.MALSettingsWriter == nil {
			return malSavedMsg{section: section, persistErr: errShikiSettingsUnavailable}
		}
		if err := deps.MALSettingsWriter(section); err != nil {
			return malSavedMsg{section: section, persistErr: err}
		}
		if deps.MALWhoAmI == nil {
			return malSavedMsg{section: section, verifyErr: errShikiWhoAmIUnavailable}
		}
		ctx, cancel := context.WithTimeout(context.Background(), deps.shikiVerifyBudget())
		defer cancel()
		user, err := deps.MALWhoAmI(ctx, section)
		return malSavedMsg{section: section, user: user, verifyErr: err}
	}
}

// malOAuthPhase is the MAL OAuth sub-screen lifecycle.
type malOAuthPhase int

const (
	// malOAuthInputID collects the application client_id.
	malOAuthInputID malOAuthPhase = iota
	// malOAuthInputSecret collects the application client_secret.
	malOAuthInputSecret
	// malOAuthRunning shows the authorize URL and waits for the
	// browser callback.
	malOAuthRunning
	// malOAuthSaving persists the tokens and verifies them.
	malOAuthSaving
	// malOAuthDone greets the user (Enter pops back to the provider
	// menu, which now renders the fresh ✓ marker).
	malOAuthDone
	// malOAuthError shows the failure (Esc returns to the provider
	// menu).
	malOAuthError
)

// malOAuthReadyMsg settles the flow start: the authorize URL plus the
// blocking resolve, or the start failure.
type malOAuthReadyMsg struct {
	authURL string
	resolve func(ctx context.Context) (MALOAuthResult, error)
	err     error
}

// malOAuthTokensMsg settles the wait+exchange step.
type malOAuthTokensMsg struct {
	set MALOAuthResult
	err error
}

// malOAuthScreen walks the MAL PKCE OAuth2 path: optional client
// credential inputs → loopback flow → token persistence → greeting.
// The flow machinery itself is injected through Deps.MALOAuth (the CLI
// reuses the loopback server shared with the Shikimori flow).
type malOAuthScreen struct {
	deps *Deps
	// idInput and secretInput collect the application credentials
	// when the config carries none.
	idInput     textinput.Model
	secretInput textinput.Model
	// clientID and clientSecret are the credentials the flow runs
	// with.
	clientID, clientSecret string
	phase                  malOAuthPhase
	authURL                string
	resolve                func(ctx context.Context) (MALOAuthResult, error)
	cancel                 context.CancelFunc
	settled                malSavedMsg
	failLine               string
}

// malOAuthID is the screen identity.
const malOAuthID = "myanimelist_oauth"

// newMALOAuthScreen builds the MAL OAuth sub-screen. Fully configured
// application credentials skip the inputs and start the flow on Init.
func newMALOAuthScreen(deps *Deps) *malOAuthScreen {
	s := &malOAuthScreen{deps: deps, phase: malOAuthInputID}
	s.idInput = textinput.New()
	s.idInput.Placeholder = i18n.T("mal.oauth_client_id_prompt")
	s.idInput.SetValue(deps.MALCfg.ClientID)
	s.idInput.Focus()
	s.secretInput = textinput.New()
	s.secretInput.Placeholder = i18n.T("mal.oauth_client_secret_prompt")
	s.secretInput.SetValue(deps.MALCfg.ClientSecret)
	if deps.MALCfg.ClientID != "" && deps.MALCfg.ClientSecret != "" {
		s.clientID, s.clientSecret = deps.MALCfg.ClientID, deps.MALCfg.ClientSecret
		s.phase = malOAuthRunning
	}
	return s
}

// ID implements Screen.
func (s *malOAuthScreen) ID() string { return malOAuthID }

// Init implements Screen: a fully prefilled flow starts immediately.
func (s *malOAuthScreen) Init() tea.Cmd {
	if s.phase == malOAuthRunning {
		return s.startCmd()
	}
	return nil
}

// startCmd launches the injected flow (nil seam degrades loudly).
func (s *malOAuthScreen) startCmd() tea.Cmd {
	deps := s.deps
	clientID, clientSecret := s.clientID, s.clientSecret
	return func() tea.Msg {
		if deps.MALOAuth == nil {
			return malOAuthReadyMsg{err: errShikiOAuthUnavailable}
		}
		url, resolve, err := deps.MALOAuth(clientID, clientSecret, 0)
		return malOAuthReadyMsg{authURL: url, resolve: resolve, err: err}
	}
}

// waitCmd blocks on the resolve under the flow budget; Esc cancels.
func (s *malOAuthScreen) waitCmd() tea.Cmd {
	resolve := s.resolve
	ctx, cancel := context.WithTimeout(context.Background(), shikiOAuthWait)
	s.cancel = cancel
	return func() tea.Msg {
		defer cancel()
		set, err := resolve(ctx)
		return malOAuthTokensMsg{set: set, err: err}
	}
}

// sectionFromTokens composes the persisted [mal] section: tokens + used
// application credentials, enabled.
func (s *malOAuthScreen) sectionFromTokens(set MALOAuthResult) config.MAL {
	section := s.deps.MALCfg
	section.Enabled = true
	section.AccessToken = set.AccessToken
	section.RefreshToken = set.RefreshToken
	section.TokenExpiresAt = set.ExpiresAt
	section.ClientID = s.clientID
	section.ClientSecret = s.clientSecret
	return section
}

// Update implements Screen.
func (s *malOAuthScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch s.phase {
	case malOAuthInputID, malOAuthInputSecret:
		return s.updateInputs(msg)
	case malOAuthRunning:
		return s.updateRunning(msg)
	case malOAuthSaving:
		if settled, ok := msg.(malSavedMsg); ok {
			s.settled = settled
			if settled.persistErr != nil {
				s.phase = malOAuthError
				s.failLine = i18n.T("mal.settings_save_failed", i18n.Vals{"err": settled.persistErr.Error()})
			} else {
				// Refresh the deps snapshot on the main goroutine so
				// the provider menu renders the fresh status.
				s.deps.MALCfg = settled.section
				s.phase = malOAuthDone
			}
		}
		return s, nil
	case malOAuthDone:
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, replace(NewShikimoriSetup(s.deps))
		}
		return s, nil
	default: // malOAuthError
		if _, ok := msg.(tea.KeyPressMsg); ok {
			return s, pop()
		}
		return s, nil
	}
}

// updateInputs handles the two credential prompts.
func (s *malOAuthScreen) updateInputs(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		if s.phase == malOAuthInputID {
			s.idInput, cmd = s.idInput.Update(msg)
		} else {
			s.secretInput, cmd = s.secretInput.Update(msg)
		}
		return s, cmd
	}
	active := &s.idInput
	if s.phase == malOAuthInputSecret {
		active = &s.secretInput
	}
	resolved := ResolveText(key, active.Value())
	switch v := resolved.(type) {
	case *backToken:
		return s, pop()
	case string:
		if s.phase == malOAuthInputID {
			s.clientID = v
			s.phase = malOAuthInputSecret
			s.idInput.Blur()
			s.secretInput.Focus()
			return s, nil
		}
		s.clientSecret = v
		s.phase = malOAuthRunning
		return s, s.startCmd()
	}
	var cmd tea.Cmd
	if s.phase == malOAuthInputID {
		s.idInput, cmd = s.idInput.Update(msg)
	} else {
		s.secretInput, cmd = s.secretInput.Update(msg)
	}
	return s, cmd
}

// updateRunning handles the started flow: URL delivery schedules the
// wait, the settled tokens schedule the persist, Esc cancels.
func (s *malOAuthScreen) updateRunning(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case malOAuthReadyMsg:
		if m.err != nil {
			s.phase = malOAuthError
			s.failLine = i18n.T("mal.oauth_start_failed", i18n.Vals{"err": m.err.Error()})
			return s, nil
		}
		s.authURL = m.authURL
		s.resolve = m.resolve
		return s, s.waitCmd()
	case malOAuthTokensMsg:
		if m.err != nil {
			s.phase = malOAuthError
			s.failLine = i18n.T("mal.oauth_finish_failed", i18n.Vals{"err": m.err.Error()})
			return s, nil
		}
		s.phase = malOAuthSaving
		return s, malSaveAndVerify(s.deps, s.sectionFromTokens(m.set))
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
func (s *malOAuthScreen) View() tea.View {
	var b []byte
	title := i18n.T("mal.oauth_title")
	switch s.phase {
	case malOAuthInputID, malOAuthInputSecret:
		b = append(b, theme.Title.Render(title)...)
		b = append(b, '\n', '\n')
		if s.phase == malOAuthInputID {
			b = append(b, i18n.T("mal.oauth_client_id_prompt")...)
			b = append(b, s.idInput.View()...)
		} else {
			b = append(b, i18n.T("mal.oauth_client_secret_prompt")...)
			b = append(b, s.secretInput.View()...)
		}
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.enter_next_esc_back"))...)
	case malOAuthRunning:
		b = append(b, theme.Title.Render(title)...)
		b = append(b, '\n', '\n')
		b = append(b, i18n.T("mal.oauth_open_url")...)
		if s.authURL != "" {
			b = append(b, theme.Accent.Render(s.authURL)...)
			b = append(b, '\n', '\n')
		}
		b = append(b, theme.Dim.Render(i18n.T("mal.oauth_waiting"))...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.esc_cancel"))...)
	case malOAuthSaving:
		b = append(b, theme.Title.Render(title)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Dim.Render(i18n.T("mal.oauth_saving"))...)
	case malOAuthDone:
		b = append(b, theme.Title.Render(title)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Success.Render(i18n.T("mal.oauth_done"))...)
		b = append(b, '\n', '\n')
		b = append(b, malUserLine(s.settled.user, s.settled.verifyErr)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.enter_continue"))...)
	default:
		b = append(b, theme.Title.Render(title)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Error.Render(s.failLine)...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render(i18n.T("common.esc_back"))...)
	}
	return tea.NewView(string(b))
}
