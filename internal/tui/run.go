package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// initialStack builds the opening screen stack: when Shikimori is
// unconfigured, the auth setup screen is the ONLY screen — the root
// menu is not reachable until authorization completes (search requires
// Shikimori; auth is mandatory). Once authenticated, the startup
// two-way list sync (PR27) runs before the root menu when the sync
// seam is wired.
func initialStack(deps *Deps) []Screen {
	if ShikimoriNeedsSetup(deps) {
		return []Screen{NewShikimoriSetup(deps)}
	}
	if shikiSyncNeeded(deps) {
		return []Screen{NewSyncScreen(deps)}
	}
	return []Screen{NewRootScreen(deps)}
}

// shikiSyncNeeded reports the PR27 startup-sync gate: the tracker is
// authenticated (enabled with a session cookie or an OAuth token) and
// the sync seam is wired — the sync then always runs on startup.
func shikiSyncNeeded(deps *Deps) bool {
	return deps != nil && deps.SyncFull != nil &&
		deps.ShikiCfg.Enabled &&
		(deps.ShikiCfg.Session != "" || deps.ShikiCfg.AccessToken != "")
}

// Run launches the interactive terminal application: the root menu
// with the full service set (plus the PR26 first-run Shikimori setup
// overlay when needed), alt-screen mode, app-context cancellation on
// quit and the §5 exception guarantees.
func Run(ctx context.Context, deps *Deps, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	// PR110 dependency injection: the CLI builds the locale bundle from
	// [general] locale and hands it over via Deps; Run installs exactly
	// what it was given before the first screen renders. nil keeps the
	// i18n package default (embedded en).
	if deps != nil && deps.I18n != nil {
		i18n.SetBundle(deps.I18n)
	}
	stack := initialStack(deps)
	app := NewApp(stack[0], deps, log, stack[1:]...)
	program := tea.NewProgram(app, tea.WithContext(ctx))
	final, err := program.Run()
	if final != nil {
		if finished, ok := final.(App); ok {
			finished.Cancel()
		}
	}
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
