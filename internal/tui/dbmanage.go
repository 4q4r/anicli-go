package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// Database flow screen ids.
const (
	dbMenuID   = "db-menu"
	dbResultID = "db-result"
)

// dbClearedMsg settles one clear operation.
type dbClearedMsg struct {
	action string
	counts string
}

// NewDBMenu builds «🗄️ Управление БД» (python database_menu port):
// three destructive clears plus the pinned Back; every clear demands
// an explicit confirmation screen.
func NewDBMenu(deps *Deps) *MenuScreen {
	return NewMenuScreen(MenuScreenConfig{
		ID:    dbMenuID,
		Title: "Управление базой данных:",
		Choices: []Choice{
			{ID: "clear_skips", Label: "Очистить предсказания таймкодов"},
			{ID: "clear_history", Label: "Очистить всю историю просмотров"},
			{ID: "clear_all", Label: "Полная очистка БД (история + таймкоды)"},
		},
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			id, _ := pick.(string)
			return push(newDBConfirm(deps, id))
		},
	})
}

// newDBConfirm builds the confirmation screen for one clear action.
func newDBConfirm(deps *Deps, action string) *dbConfirm {
	label := map[string]string{
		"clear_skips":   "Очистить предсказания таймкодов",
		"clear_history": "Очистить всю историю просмотров",
		"clear_all":     "Полная очистка БД (история + таймкоды)",
	}[action]
	return &dbConfirm{deps: deps, action: action, label: label}
}

// dbConfirm is the yes/no gate; after a successful clear it turns
// into the result surface (counts + any-key-to-leave, no re-clear).
type dbConfirm struct {
	deps    *Deps
	action  string
	label   string
	cleared string
}

// ID implements Screen.
func (c *dbConfirm) ID() string { return dbResultID }

// Init implements Screen.
func (c *dbConfirm) Init() tea.Cmd { return nil }

// Update implements Screen: enter confirms, esc/Back cancels (I2);
// dbClearedMsg settles the operation with its counts and re-arms
// enter to pop back (I7).
func (c *dbConfirm) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case dbClearedMsg:
		c.cleared = m.counts
		return c, nil
	case tea.KeyPressMsg:
		if c.cleared != "" {
			// Result surface: any key leaves; a second clear never runs.
			return c, pop()
		}
		if IsCancelKey(m) {
			return c, pop()
		}
		if m.Code != tea.KeyEnter {
			return c, nil
		}
		return c, safeCmd(c.ID(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
			defer cancel()
			switch c.action {
			case "clear_skips":
				n, err := c.deps.Database.ClearSkips(ctx)
				if err != nil {
					return errMsg{screen: c.ID(), err: err}
				}
				return dbClearedMsg{action: c.action, counts: fmt.Sprintf("Удалено %d записей о таймкодах.", n)}
			case "clear_history":
				n, err := c.deps.Database.ClearHistory(ctx)
				if err != nil {
					return errMsg{screen: c.ID(), err: err}
				}
				return dbClearedMsg{action: c.action, counts: fmt.Sprintf("Удалено %d записей истории.", n)}
			case "clear_all":
				h, sk, err := c.deps.Database.ClearAll(ctx)
				if err != nil {
					return errMsg{screen: c.ID(), err: err}
				}
				return dbClearedMsg{action: c.action,
					counts: fmt.Sprintf("Удалено %d записей истории и %d записей о таймкодах.", h, sk)}
			default:
				return errMsg{screen: c.ID(), err: fmt.Errorf("неизвестное действие %q", c.action)}
			}
		})
	default:
		return c, nil
	}
}

// View implements Screen.
func (c *dbConfirm) View() tea.View {
	if c.cleared != "" {
		body := theme.Title.Render("Готово") + "\n\n" +
			theme.Success.Render(c.cleared) +
			"\n\n" + theme.StatusLine.Render("любая клавиша — назад")
		return tea.NewView(body)
	}
	body := theme.Title.Render("Подтверждение") + "\n\n" +
		theme.Warning.Render(fmt.Sprintf(
			"Вы уверены, что хотите выполнить '%s'? Это действие необратимо.", c.label)) +
		"\n\n" + theme.StatusLine.Render("enter — да · esc — отмена")
	return tea.NewView(body)
}
