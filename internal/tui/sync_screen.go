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
	"strings"
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
// The PR39 background library refresh reuses it: one check pass is
// the same shape of work.
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

// syncProgressMsg carries a live progress update from the running sync.
type syncProgressMsg struct {
	progress shikimori.SyncProgress
}

// SyncScreen runs the PR27 startup two-way sync and hands over to the
// root menu.
type SyncScreen struct {
	deps       *Deps
	spin       spinner.Model
	phase      syncPhase
	result     *shikimori.SyncResult
	err        error
	progress   *shikimori.SyncProgress // last received live progress
	progressCh chan shikimori.SyncProgress
}

// NewSyncScreen builds the screen over the injected sync seam.
func NewSyncScreen(deps *Deps) *SyncScreen {
	return &SyncScreen{
		deps:       deps,
		spin:       spinner.New(spinner.WithSpinner(spinner.Meter)),
		phase:      syncPhaseRunning,
		progressCh: make(chan shikimori.SyncProgress, 20),
	}
}

// ID implements Screen.
func (s *SyncScreen) ID() string { return syncScreenID }

// Init implements Screen: the spinner blink, the sync command and the
// progress poller.
func (s *SyncScreen) Init() tea.Cmd {
	return tea.Batch(s.spin.Tick, safeCmd(syncScreenID, s.runCmd()), s.pollProgress())
}

// pollProgress reads one progress update from the channel and returns
// it as a message; the Update handler re-arms after each delivery.
func (s *SyncScreen) pollProgress() tea.Cmd {
	ch := s.progressCh
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil // channel closed: sync finished
		}
		return syncProgressMsg{progress: p}
	}
}

// runCmd executes the startup sync under its budget; progress updates
// are written to the shared channel for the poller to deliver.
func (s *SyncScreen) runCmd() tea.Cmd {
	deps := s.deps
	ch := s.progressCh
	return func() tea.Msg {
		defer close(ch)
		if deps == nil || deps.SyncFull == nil {
			return syncDoneMsg{err: errSyncUnavailable}
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncBudget)
		defer cancel()
		result, err := deps.SyncFull(ctx, func(p shikimori.SyncProgress) {
			select {
			case ch <- p:
			default: // channel full: drop rather than block the sync
			}
		})
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
	case syncProgressMsg:
		if s.phase == syncPhaseRunning {
			p := m.progress
			s.progress = &p
		}
		return s, s.pollProgress() // re-arm for the next update
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

// View implements Screen: Python-parity sync UI — title, magenta
// spinner line for the rates fetch, cyan headline + Unicode progress
// bar for the metadata/pull phases, then the summary.
func (s *SyncScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render("Синхронизация с Shikimori")...)
	b = append(b, '\n', '\n')
	switch s.phase {
	case syncPhaseRunning:
		if s.progress == nil {
			b = append(b, theme.Accent.Render(s.spin.View()+" Загрузка списков Shikimori…")...)
		} else {
			switch s.progress.Phase {
			case "rates":
				b = append(b, theme.Accent.Render(s.spin.View()+" Загрузка списков Shikimori…")...)
			case "pull":
				b = append(b, theme.Accent.Render(s.spin.View()+" Сопоставление локальных записей")...)
				b = append(b, '\n', '\n')
				b = append(b, renderProgressBar(s.progress.Done, s.progress.Total, 30)...)
			case "new":
				b = append(b, theme.Success.Render(fmt.Sprintf("Найдено %d новых аниме. Загрузка метаданных…", s.progress.Total))...)
				b = append(b, '\n', '\n')
				b = append(b, renderProgressBar(s.progress.Done, s.progress.Total, 30)...)
			case "push":
				b = append(b, theme.Warning.Render(s.spin.View()+" Отправка отложенных изменений")...)
				b = append(b, '\n', '\n')
				b = append(b, renderProgressBar(s.progress.Done, s.progress.Total, 30)...)
			default:
				b = append(b, theme.Accent.Render(s.spin.View()+" "+s.progress.Message)...)
			}
		}
	case syncPhaseDone:
		r := s.result
		if r == nil {
			r = &shikimori.SyncResult{}
		}
		b = append(b, theme.Success.Render("✓ Синхронизация завершена")...)
		b = append(b, '\n', '\n')
		b = append(b, fmt.Sprintf("Обновлено: %d · Добавлено: %d · Отправлено: %d",
			r.Updated, r.Created, r.Pushed)...)
		if r.Conflicts > 0 {
			b = append(b, '\n')
			b = append(b, theme.Warning.Render(fmt.Sprintf(
				"⚠ Конфликтов: %d (статус — с Shikimori, прогресс — локальный)", r.Conflicts))...)
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

// renderProgressBar draws a horizontal Unicode progress bar:
// ████████░░░░░░░░░░░░░░░░░░░░ 45%
func renderProgressBar(done, total, width int) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	pct := done * 100 / total
	if pct > 100 {
		pct = 100
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return theme.Accent.Render(bar) + " " + fmt.Sprintf("%d%%", pct)
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
