package tui

// The PR27 startup sync screen: after authentication (first-run setup
// or an already-configured session) and before the root menu, the app
// runs the two-way Shikimori list sync with a spinner surface, then a
// short summary — or the yellow network warning — before the root menu
// takes over.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/shikimori"
)

// syncScreenID is the startup sync screen identity.
const syncScreenID = "shikimori_sync"

// syncBudget bounds the whole startup two-way sync: the rates fetch,
// the metadata chunks of a possibly-large first list and the dirty
// replay (commands own their timeout contexts — see the App.ctx note).
const syncBudget = 5 * time.Minute

// syncSummaryHold is how long the settled summary stays on screen
// before the root menu auto-advances.
const syncSummaryHold = 2 * time.Second

// syncDoneMsg settles the startup sync command.
type syncDoneMsg struct {
	result *shikimori.SyncResult
	err    error
}

// syncAdvanceMsg fires after the summary hold.
type syncAdvanceMsg struct{}

// errSyncUnavailable is the loud-degradation sentinel for a screen
// built without the sync seam (embedded builds).
var errSyncUnavailable = errors.New("синхронизация недоступна (сборка без запуска синхронизации)")

// syncPhase is the sync screen lifecycle.
type syncPhase int

const (
	// syncPhaseRunning shows the spinner while the sync works.
	syncPhaseRunning syncPhase = iota
	// syncPhaseDone shows the summary until key press or the hold.
	syncPhaseDone
	// syncPhaseFailed shows the warning until key press.
	syncPhaseFailed
)

// SyncScreen runs the PR27 startup two-way sync and hands over to the
// root menu.
type SyncScreen struct {
	deps   *Deps
	spin   spinner.Model
	phase  syncPhase
	result *shikimori.SyncResult
	err    error
}

// NewSyncScreen builds the screen over the injected sync seam.
func NewSyncScreen(deps *Deps) *SyncScreen {
	return &SyncScreen{
		deps:  deps,
		spin:  spinner.New(spinner.WithSpinner(spinner.Dot)),
		phase: syncPhaseRunning,
	}
}

// ID implements Screen.
func (s *SyncScreen) ID() string { return syncScreenID }

// Init implements Screen: the spinner blink plus the sync command.
func (s *SyncScreen) Init() tea.Cmd {
	return tea.Batch(s.spin.Tick, safeCmd(syncScreenID, s.runCmd()))
}

// runCmd executes the startup sync under its budget (nil seam degrades
// loudly).
func (s *SyncScreen) runCmd() tea.Cmd {
	deps := s.deps
	return func() tea.Msg {
		if deps == nil || deps.SyncFull == nil {
			return syncDoneMsg{err: errSyncUnavailable}
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncBudget)
		defer cancel()
		result, err := deps.SyncFull(ctx)
		return syncDoneMsg{result: result, err: err}
	}
}

// syncAdvanceCmd holds the summary for the configured time, then fires
// the auto-advance (a blocking command mirrors the oauth waitCmd
// pattern; the message is ignored once the screen was replaced).
func syncAdvanceCmd() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(syncSummaryHold)
		return syncAdvanceMsg{}
	}
}

// Update implements Screen: the spinner blinks while running; the
// settled verdict renders until a key press (and, on success, the hold
// timer) replaces the screen with the root menu.
func (s *SyncScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case spinner.TickMsg:
		if s.phase != syncPhaseRunning {
			return s, nil // stop the blink once settled
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(m)
		return s, cmd
	case syncDoneMsg:
		s.result, s.err = m.result, m.err
		if m.err != nil {
			s.phase = syncPhaseFailed
			return s, nil
		}
		s.phase = syncPhaseDone
		return s, syncAdvanceCmd()
	case syncAdvanceMsg:
		if s.phase != syncPhaseDone {
			return s, nil
		}
		return s, replace(newRootAfterAuth(s.deps))
	case tea.KeyPressMsg:
		if s.phase == syncPhaseRunning {
			return s, nil
		}
		return s, replace(newRootAfterAuth(s.deps))
	default:
		return s, nil
	}
}

// View implements Screen.
func (s *SyncScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render("Списки Shikimori")...)
	b = append(b, '\n', '\n')
	switch s.phase {
	case syncPhaseRunning:
		b = append(b, theme.Accent.Render(s.spin.View()+" Синхронизация с Shikimori…")...)
	case syncPhaseDone:
		r := s.result
		if r == nil {
			r = &shikimori.SyncResult{}
		}
		b = append(b, theme.Success.Render("✓ Синхронизация завершена")...)
		b = append(b, '\n', '\n')
		b = append(b, fmt.Sprintf("Синхронизировано: %d обновлено, %d добавлено, %d отправлено",
			r.Updated, r.Created, r.Pushed)...)
		if r.Conflicts > 0 {
			b = append(b, '\n')
			b = append(b, theme.Warning.Render(fmt.Sprintf(
				"Конфликтов: %d (статус — с Shikimori, прогресс — локальный)", r.Conflicts))...)
		}
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("любая клавиша — продолжить")...)
	default: // syncPhaseFailed
		b = append(b, theme.Warning.Render("⚠ Синхронизация не удалась: "+s.err.Error())...)
		b = append(b, '\n', '\n')
		b = append(b, theme.Dim.Render("Локальный список не изменился; отложенные изменения уйдут при следующем запуске")...)
		b = append(b, '\n', '\n')
		b = append(b, theme.StatusLine.Render("любая клавиша — продолжить")...)
	}
	return tea.NewView(string(b))
}

// afterAuthScreen picks the opening screen after the first-run auth
// completes (PR27): the startup sync when the seam is wired, else the
// root menu directly.
func afterAuthScreen(deps *Deps) Screen {
	if deps != nil && deps.SyncFull != nil {
		return NewSyncScreen(deps)
	}
	return newRootAfterAuth(deps)
}
