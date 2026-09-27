package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestI1BackAlwaysAppendedLast: invariant I1 (PR24) — MenuPrompt always
// appends the Back entry as the LAST item of the choice list; the
// PinList renders it pinned at the bottom of the viewport.
func TestI1BackAlwaysAppendedLast(t *testing.T) {
	t.Run("three choices get Back at the last index", func(t *testing.T) {
		m := NewMenu("Меню", "",
			Choice{ID: "search", Label: "🔎 Поиск"},
			Choice{ID: "lists", Label: "📜 Lists"},
			Choice{ID: "exit", Label: "🚪 Exit"},
		)
		if len(m.Items) != 4 {
			t.Fatalf("want 4 items (3 + Back), got %d", len(m.Items))
		}
		last := len(m.Items) - 1
		if m.Items[last].ID != BackID {
			t.Fatalf("last position must be Back, got %q", m.Items[last].ID)
		}
		if m.Items[0].ID != "search" || m.Items[last-1].ID != "exit" {
			t.Fatalf("caller choices must keep order before Back: %+v", m.Items)
		}
	})

	t.Run("empty choices still get Back as the lone last item (I3 overlap)", func(t *testing.T) {
		m := NewMenu("Пусто", "Nothing found")
		if len(m.Items) != 1 {
			t.Fatalf("want exactly Back, got %d items", len(m.Items))
		}
		if m.Items[0].ID != BackID {
			t.Fatalf("lone item must be Back, got %q", m.Items[0].ID)
		}
	})

	t.Run("backless menu (root) has no Back entry at all", func(t *testing.T) {
		m := NewMenuWithoutBack("Корень", "",
			Choice{ID: "search", Label: "🔎 Поиск"},
			Choice{ID: "exit", Label: "🚪 Exit"},
		)
		if len(m.Items) != 2 {
			t.Fatalf("backless menu keeps exactly the caller choices, got %d", len(m.Items))
		}
		for i, item := range m.Items {
			if item.ID == BackID {
				t.Fatalf("backless menu must not contain Back (item %d)", i)
			}
		}
	})
}

// TestI3EmptyChoiceListLegal: invariant I3 — an empty choice list is a
// legal menu: Back alone plus an empty-state message.
func TestI3EmptyChoiceListLegal(t *testing.T) {
	t.Run("empty message rendered for empty list", func(t *testing.T) {
		m := NewMenu("Результаты", "List is empty")
		if m.EmptyMessage != "List is empty" {
			t.Fatalf("empty-state message lost: %q", m.EmptyMessage)
		}
		if !strings.Contains(m.RenderItems(), "List is empty") {
			t.Fatalf("rendered menu must show empty-state message, got %q", m.RenderItems())
		}
	})

	t.Run("empty menu renders Back plus empty-state (PinList parity, M15)", func(t *testing.T) {
		m := NewMenu("Результаты", "List is empty")
		r := m.RenderItems()
		if !strings.Contains(r, BackLabel()) {
			t.Fatalf("empty menu must still render the Back row (I1), got %q", r)
		}
		if !strings.Contains(r, "List is empty") {
			t.Fatalf("empty-state message must render alongside Back, got %q", r)
		}
		if strings.Index(r, "List is empty") > strings.Index(r, BackLabel()) {
			t.Fatalf("empty-state message renders above the trailing Back row, got %q", r)
		}
	})

	t.Run("non-empty menu renders choices and no empty message", func(t *testing.T) {
		m := NewMenu("Меню", "никогда",
			Choice{ID: "a", Label: "Первый"},
		)
		if strings.Contains(m.RenderItems(), "никогда") {
			t.Fatalf("empty-state message must not render on non-empty menu")
		}
		if !strings.Contains(m.RenderItems(), "Первый") {
			t.Fatalf("choice label missing in render")
		}
		if !strings.Contains(m.RenderItems(), BackLabel()) {
			t.Fatalf("Back label must render on non-empty menu")
		}
	})
}

// TestI2CancelNormalizesToBack: invariant I2 — Esc, Ctrl-C and empty
// text input resolve to the Back sentinel, never an application exit
// (the root screen converts ctrl+c itself).
func TestI2CancelNormalizesToBack(t *testing.T) {
	menu := NewMenu("Подменю", "",
		Choice{ID: "one", Label: "Один", Value: 1},
		Choice{ID: "two", Label: "Два", Value: 2},
	)

	t.Run("esc returns Back regardless of cursor", func(t *testing.T) {
		if got := ResolveKey(menu, 2, esc()); got != Back {
			t.Fatalf("esc must resolve to Back sentinel, got %#v", got)
		}
	})

	t.Run("ctrl+c returns Back in submenus", func(t *testing.T) {
		if got := ResolveKey(menu, 1, ctrlC()); got != Back {
			t.Fatalf("ctrl+c must resolve to Back sentinel, got %#v", got)
		}
	})

	t.Run("empty typed input in a TEXT prompt returns Back", func(t *testing.T) {
		if got := ResolveText(enter(), "   "); got != Back {
			t.Fatalf("empty text input must resolve to Back sentinel, got %#v", got)
		}
	})

	t.Run("non-empty typed input in a TEXT prompt returns the answer", func(t *testing.T) {
		if got := ResolveText(enter(), "  Наруто  "); got != "Наруто" {
			t.Fatalf("text prompt must return trimmed answer, got %#v", got)
		}
	})

	t.Run("enter on the trailing Back row returns Back", func(t *testing.T) {
		if got := ResolveKey(menu, len(menu.Items)-1, enter()); got != Back {
			t.Fatalf("picking Back must return the sentinel, got %#v", got)
		}
	})

	t.Run("enter on real choice returns its value", func(t *testing.T) {
		if got := ResolveKey(menu, 1, enter()); got != 2 {
			t.Fatalf("want choice value 2, got %#v", got)
		}
	})

	t.Run("plain movement key is not a resolution", func(t *testing.T) {
		if got := ResolveKey(menu, 1, tea.KeyPressMsg{Code: tea.KeyDown}); got != nil {
			t.Fatalf("movement key must not resolve, got %#v", got)
		}
	})
}

// TestI4SingleSentinelIdentityCompare: invariant I4 — Back is one value;
// every Back path yields the identical pointer.
func TestI4SingleSentinelIdentityCompare(t *testing.T) {
	t.Run("cancel and explicit Back pick are the same value", func(t *testing.T) {
		menu := NewMenu("М", "", Choice{ID: "x", Label: "X", Value: "x"})
		byEsc := ResolveKey(menu, 1, esc())
		byEnter := ResolveKey(menu, len(menu.Items)-1, enter())
		byEmptyText := ResolveText(enter(), "")
		if byEsc != Back || byEnter != Back || byEmptyText != Back {
			t.Fatalf("all back paths must be the same sentinel: %v %v %v want %v",
				byEsc, byEnter, byEmptyText, Back)
		}
	})

	t.Run("sentinel never equals a caller value", func(t *testing.T) {
		var asAny any = Back
		if asAny == any("Back") || asAny == any(0) {
			t.Fatalf("sentinel must not collide with ordinary values")
		}
	})

	t.Run("Back is a single stable reference", func(t *testing.T) {
		first, second := Back, Back
		if first != second {
			t.Fatalf("Back must be a single stable value")
		}
	})
}

// TestRootInterruptPolicy: the I2 root exception — only the root menu
// turns ctrl+c into an app exit; Esc at root stays.
func TestRootInterruptPolicy(t *testing.T) {
	t.Run("ctrl+c at root means exit", func(t *testing.T) {
		if !RootInterruptExits(ctrlC()) {
			t.Fatalf("ctrl+c at root must exit")
		}
	})

	t.Run("esc at root does not exit", func(t *testing.T) {
		if RootInterruptExits(esc()) {
			t.Fatalf("esc at root must not exit")
		}
	})

	t.Run("plain key is not an interrupt", func(t *testing.T) {
		if RootInterruptExits(tea.KeyPressMsg{Code: 'q'}) {
			t.Fatalf("plain key must not count as root interrupt")
		}
	})
}

// esc builds the Esc key press.
func esc() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEsc} }

// ctrlC builds the Ctrl-C key press.
func ctrlC() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}

// enter builds the Enter key press.
func enter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
