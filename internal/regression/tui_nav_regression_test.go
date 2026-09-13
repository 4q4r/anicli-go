package regression

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/tui"
)

// navInvariant documents one §5 TUI navigation invariant and the bug
// class its assertion kills. The table re-executes each invariant
// through the exported tui surface so a regression in the nav core
// fails here BY NAME — the same checks live as unit tests in
// internal/tui (nav_test.go TestI1..TestI4); this table is the
// cross-package checklist that keeps the invariant set itself honest
// (dropping or weakening an invariant shows up as a missing row).
type navInvariant struct {
	id   string
	name string
	live func(t *testing.T)
}

func escKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEsc} }
func ctrlCKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}
func enterKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

var navInvariants = []navInvariant{
	{
		id:   "I1",
		name: "Back is always prepended at position 0 of every menu (and visible in empty menus)",
		live: func(t *testing.T) {
			menu := tui.NewMenu("Заголовок", "пусто",
				tui.Choice{ID: "a", Label: "A"},
				tui.Choice{ID: "b", Label: "B"},
			)
			if len(menu.Items) == 0 || menu.Items[0].ID != tui.BackID ||
				menu.Items[0].Label != tui.BackLabel || menu.Items[0].Value != tui.Back {
				t.Fatalf("I1: menu items must start with the Back entry, got %+v", menu.Items)
			}

			empty := tui.NewMenu("Заголовок", "ничего нет")
			if len(empty.Items) != 1 || empty.Items[0].ID != tui.BackID {
				t.Fatalf("I1: empty menu must still hold exactly the Back row, got %+v", empty.Items)
			}
			if want := tui.BackLabel + "\nничего нет"; empty.RenderItems() != want {
				t.Fatalf("I1: empty render = %q, want %q", empty.RenderItems(), want)
			}
		},
	},
	{
		id:   "I2",
		name: "Esc, Ctrl-C and empty input normalize to Back — never an exit, never a crash",
		live: func(t *testing.T) {
			menu := tui.NewMenu("Заголовок", "", tui.Choice{ID: "a", Label: "A"})
			for _, key := range []tea.KeyPressMsg{escKey(), ctrlCKey()} {
				if got := tui.ResolveKey(menu, 1, key); got != tui.Back {
					t.Fatalf("I2: cancel key %v must resolve to Back, got %v", key.Code, got)
				}
			}
			// Enter on an out-of-range cursor must clamp to Back, not panic.
			if got := tui.ResolveKey(menu, 99, enterKey()); got != tui.Back {
				t.Fatalf("I2: out-of-range cursor must clamp to Back, got %v", got)
			}
			// Text prompt: empty enter and cancel keys normalize to Back.
			if got := tui.ResolveText(enterKey(), "   "); got != tui.Back {
				t.Fatalf("I2: empty text answer must normalize to Back, got %v", got)
			}
			if got := tui.ResolveText(escKey(), "запрос"); got != tui.Back {
				t.Fatalf("I2: Esc must normalize to Back even with text, got %v", got)
			}
			if got := tui.ResolveText(ctrlCKey(), "запрос"); got != tui.Back {
				t.Fatalf("I2: Ctrl-C must normalize to Back even with text, got %v", got)
			}
			// Root exception: ONLY Ctrl-C exits at the root menu; Esc does not.
			if !tui.RootInterruptExits(ctrlCKey()) {
				t.Fatal("I2: Ctrl-C at root must signal app exit")
			}
			if tui.RootInterruptExits(escKey()) {
				t.Fatal("I2: Esc at root must NOT signal app exit")
			}
		},
	},
	{
		id:   "I3",
		name: "An empty choice list is legal: Back renders alone plus the empty-state message",
		live: func(t *testing.T) {
			menu := tui.NewMenu("Заголовок", "Ничего не найдено")
			if got := tui.ResolveKey(menu, 0, enterKey()); got != tui.Back {
				t.Fatalf("I3: enter on an empty menu must resolve the Back row, got %v", got)
			}
			if want := tui.BackLabel + "\nНичего не найдено"; menu.RenderItems() != want {
				t.Fatalf("I3: empty render = %q, want %q", menu.RenderItems(), want)
			}
		},
	},
	{
		id:   "I4",
		name: "Exactly one Back sentinel value exists, compared by identity",
		live: func(t *testing.T) {
			menu := tui.NewMenu("Заголовок", "")
			// Every resolution path yields THE SAME pointer.
			a := tui.ResolveKey(menu, 0, escKey())
			b := tui.ResolveText(ctrlCKey(), "")
			c := menu.Items[0].Value
			if a != tui.Back || b != tui.Back || c != tui.Back {
				t.Fatalf("I4: all Back resolutions must be the same sentinel, got %p %p %p (want %p)",
					a, b, c, tui.Back)
			}
			// A user payload equal to nothing else resolves by value,
			// never colliding with the sentinel.
			menu2 := tui.NewMenu("Заголовок", "", tui.Choice{ID: "x", Label: "X", Value: 42})
			if got := tui.ResolveKey(menu2, 1, enterKey()); got != 42 {
				t.Fatalf("I4: choice payload must resolve intact, got %v", got)
			}
		},
	},
}

// TestTUINavInvariants runs the full I1-I4 regression checklist. Each
// row names the invariant it pins; a failure reports the invariant id.
func TestTUINavInvariants(t *testing.T) {
	if len(navInvariants) != 4 {
		t.Fatalf("the §5 invariant set is exactly I1-I4 (got %d rows) — update this table only together with the spec", len(navInvariants))
	}
	seen := map[string]bool{}
	for _, inv := range navInvariants {
		if seen[inv.id] {
			t.Fatalf("duplicate invariant row %s", inv.id)
		}
		seen[inv.id] = true
		t.Run(inv.id+"-"+inv.name, func(t *testing.T) {
			inv.live(t)
		})
	}
	for _, id := range []string{"I1", "I2", "I3", "I4"} {
		if !seen[id] {
			t.Fatalf("invariant %s missing from the regression table", id)
		}
	}
}
