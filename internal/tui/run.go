package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"log/slog"
)

// Run launches the interactive terminal application: the root menu
// with the full service set, alt-screen mode, app-context
// cancellation on quit and the §5 exception guarantees.
func Run(ctx context.Context, deps *Deps, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	app := NewApp(NewRootScreen(deps), deps, log)
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
