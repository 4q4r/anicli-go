package tui

import (
	"context"
	"fmt"
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/i18n"
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
		Title: i18n.T("db.title"),
		Choices: []Choice{
			{ID: "clear_skips", Label: dbActionLabel("clear_skips")},
			{ID: "clear_history", Label: dbActionLabel("clear_history")},
			{ID: "clear_all", Label: dbActionLabel("clear_all")},
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

// dbActionKey maps a clear action id to its i18n key (the label map in
// newDBConfirm shares it — one vocabulary, two render sites).
func dbActionKey(action string) string {
	switch action {
	case "clear_skips":
		return "db.clear_skips"
	case "clear_history":
		return "db.clear_history"
	case "clear_all":
		return "db.clear_all"
	}
	return action
}

func dbActionLabel(action string) string { return i18n.T(dbActionKey(action)) }

// newDBConfirm builds the confirmation screen for one clear action.
func newDBConfirm(deps *Deps, action string) *dbConfirm {
	return &dbConfirm{deps: deps, action: action, label: dbActionLabel(action)}
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
				return dbClearedMsg{action: c.action, counts: i18n.T("db.cleared_skips", i18n.Vals{"count": strconv.FormatInt(n, 10)})}
			case "clear_history":
				n, err := c.deps.Database.ClearHistory(ctx)
				if err != nil {
					return errMsg{screen: c.ID(), err: err}
				}
				return dbClearedMsg{action: c.action, counts: i18n.T("db.cleared_history", i18n.Vals{"count": strconv.FormatInt(n, 10)})}
			case "clear_all":
				h, sk, err := c.deps.Database.ClearAll(ctx)
				if err != nil {
					return errMsg{screen: c.ID(), err: err}
				}
				return dbClearedMsg{action: c.action,
					counts: i18n.T("db.cleared_all", i18n.Vals{"history": strconv.FormatInt(h, 10), "skips": strconv.FormatInt(sk, 10)})}
			default:
				return errMsg{screen: c.ID(), err: fmt.Errorf("unknown db action %q", c.action)}
			}
		})
	default:
		return c, nil
	}
}

// View implements Screen.
func (c *dbConfirm) View() tea.View {
	if c.cleared != "" {
		body := theme.Title.Render(i18n.T("db.done")) + "\n\n" +
			theme.Success.Render(c.cleared) +
			"\n\n" + theme.StatusLine.Render(i18n.T("common.any_key_back"))
		return tea.NewView(body)
	}
	body := theme.Title.Render(i18n.T("db.confirm_title")) + "\n\n" +
		theme.Warning.Render(i18n.T("db.confirm_body", i18n.Vals{"action": c.label})) +
		"\n\n" + theme.StatusLine.Render(i18n.T("db.confirm_hint"))
	return tea.NewView(body)
}
