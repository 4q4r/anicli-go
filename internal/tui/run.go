package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"log/slog"
)

// initialStack builds the opening screen stack (PR26): the root menu,
// with the Shikimori first-run setup screen pushed on top when the
// integration is enabled but unconfigured. The setup screen pops on
// skip/completion, so the root menu is what remains either way.
func initialStack(deps *Deps) []Screen {
	root := NewRootScreen(deps)
	if ShikimoriNeedsSetup(deps) {
		return []Screen{root, NewShikimoriSetup(deps)}
	}
	return []Screen{root}
}

// Run launches the interactive terminal application: the root menu
// with the full service set (plus the PR26 first-run Shikimori setup
// overlay when needed), alt-screen mode, app-context cancellation on
// quit and the §5 exception guarantees.
func Run(ctx context.Context, deps *Deps, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
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
