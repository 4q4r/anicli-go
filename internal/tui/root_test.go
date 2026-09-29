package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newTestDeps builds empty deps (screens must tolerate nil services
// until a flow actually calls one).
func newTestDeps() *Deps { return &Deps{} }

// TestRootMenuContents: the root menu shows its entries (PR40 removes
// the PR35 «🧲 Торренты» entry; PR113 adds «▶ Продолжить» and PR114
// adds «📅 Сезон» after the four feature items). Root shows NO «Назад»
// row: «🚪 Exit» takes its place as the pinned BOTTOM row (PR24).
func TestRootMenuContents(t *testing.T) {
	root := NewRootScreen(newTestDeps())
	view := root.View().Content
	for _, want := range []string{
		"📜 Lists",
		"📂 Downloads",
		"🗄️ Database management",
		"🛠 Check",
		"▶ Continue: —",
		"📅 Season",
		"🚪 Exit",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("root view must contain %q, got:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Торренты") {
		t.Fatalf("root view must NOT contain the removed «Торренты» entry (PR40), got:\n%s", view)
	}
	if strings.Contains(view, "Поиск") {
		t.Fatalf("root view must NOT contain the removed free-text search entry, got:\n%s", view)
	}
	if strings.Contains(view, BackLabel()) {
		t.Fatalf("root view must NOT contain the Back row %q (Выход replaces it), got:\n%s", BackLabel(), view)
	}
	// Exactly six entries (PR113b: the «Продолжить» row left the menu
	// — it renders as the special header above the list, followed by a
	// separator line; PR114's «Сезон» stays after the feature items).
	if items := root.list.Menu().Items; len(items) != 6 {
		t.Fatalf("root menu must hold 6 items, got %d: %+v", len(items), items)
	}
	// Выход is the LAST item, rendered below every other entry.
	exitIdx := strings.LastIndex(view, "🚪 Exit")
	listsIdx := strings.LastIndex(view, "📜 Lists")
	if exitIdx < listsIdx {
		t.Fatalf("Выход must render below the other root entries, got:\n%s", view)
	}
}

// TestRootExitIsLastItem: the exit entry is the trailing menu item and
// is reachable at the list's end.
func TestRootExitIsLastItem(t *testing.T) {
	root := NewRootScreen(newTestDeps())
	items := root.list.Menu().Items
	if len(items) == 0 || items[len(items)-1].ID != "exit" {
		t.Fatalf("root menu must end with the exit item, got %+v", items)
	}
}

// TestRootExitAndInterrupts: I2 root exception — Выход quits, Ctrl-C
// quits, Esc stays.
func TestRootExitAndInterrupts(t *testing.T) {
	newRoot := func() *rootScreen { return NewRootScreen(newTestDeps()) }

	t.Run("enter on Выход quits", func(t *testing.T) {
		root := newRoot()
		idx := indexOfChoice(root.MenuScreen, "exit")
		root.list.Jump(idx)
		_, cmd := root.Update(enter())
		if !isQuitCmd(cmd) {
			t.Fatalf("Выход must quit, got %v", cmd)
		}
	})

	t.Run("ctrl+c at root quits (I2 exception)", func(t *testing.T) {
		root := newRoot()
		_, cmd := root.Update(ctrlC())
		if !isQuitCmd(cmd) {
			t.Fatalf("ctrl+c at root must quit")
		}
	})

	t.Run("esc at root stays (no quit, no crash)", func(t *testing.T) {
		root := newRoot()
		next, cmd := root.Update(esc())
		if isQuitCmd(cmd) {
			t.Fatalf("esc at root must not quit")
		}
		if next.ID() != rootScreenID {
			t.Fatalf("esc at root must stay on root, got %q", next.ID())
		}
	})

	t.Run("back pick at root stays", func(t *testing.T) {
		root := newRoot()
		// Esc resolves the Back sentinel; at root Back means "stay".
		next, cmd := root.Update(esc())
		if isQuitCmd(cmd) {
			t.Fatalf("Back at root must not quit")
		}
		if next.ID() != rootScreenID {
			t.Fatalf("Back at root must stay on root, got %q", next.ID())
		}
	})
}

// TestRootNavigation: root menu entries push their flow screens.
func TestRootNavigation(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"lists", historyFilterID},
		{"downloads", offlineTitlesID},
		{"db", dbMenuID},
		{"check", healthID},
		{"season", seasonalScreenID},
	}
	for _, tc := range cases {
		t.Run(tc.id+" pushes "+tc.want, func(t *testing.T) {
			root := NewRootScreen(newTestDeps())
			idx := indexOfChoice(root.MenuScreen, tc.id)
			root.list.Jump(idx)
			_, cmd := root.Update(enter())
			if cmd == nil {
				t.Fatalf("%s must schedule navigation", tc.id)
			}
			msg := cmd()
			pm, ok := msg.(pushMsg)
			if !ok {
				t.Fatalf("%s must push a screen, got %#v", tc.id, msg)
			}
			if pm.screen.ID() != tc.want {
				t.Fatalf("want screen %q, got %q", tc.want, pm.screen.ID())
			}
		})
	}
}

// TestMenuScreenBackPops: a generic submenu resolves Back into a pop.
func TestMenuScreenBackPops(t *testing.T) {
	m := NewMenuScreen(MenuScreenConfig{
		ID:       "submenu",
		Title:    "Подменю",
		EmptyMsg: "пусто",
		Choices:  []Choice{{ID: "a", Label: "A"}},
	})
	next, cmd := m.Update(esc())
	if next.ID() != "submenu" {
		t.Fatalf("esc keeps the screen until pop lands, got %q", next.ID())
	}
	if cmd == nil {
		t.Fatalf("esc must schedule a pop")
	}
	msg := cmd()
	if _, ok := msg.(popMsg); !ok {
		t.Fatalf("esc on submenu must pop, got %#v", msg)
	}
}

// helpers

func indexOfChoice(m *MenuScreen, id string) int {
	for i, c := range m.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch cmd().(type) {
	case tea.QuitMsg, quitMsg:
		return true
	default:
		return false
	}
}
