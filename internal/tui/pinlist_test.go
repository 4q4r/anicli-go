package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func numberedChoices(n int) []Choice {
	out := make([]Choice, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Choice{ID: itoa(i), Label: "Элемент " + itoa(i)})
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// TestPinListBackRowAlwaysVisible: I1 (PR24) — the Back row (menu LAST
// position) is rendered on every page, at every scroll offset, for any
// cursor, PINNED AT THE BOTTOM of the viewport.
func TestPinListBackRowAlwaysVisible(t *testing.T) {
	t.Run("back label visible on first page, below the items", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(3)...), 10)
		r := m.Render()
		if !strings.Contains(r, BackLabel()) {
			t.Fatalf("Back must render on page 1")
		}
		if strings.Index(r, BackLabel()) < strings.Index(r, "Элемент 3") {
			t.Fatalf("Back must render BELOW the items, got:\n%s", r)
		}
	})

	t.Run("back label still visible when scrolled to the end", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(50)...), 10)
		for range 50 { // exactly to the end (further downs wrap — PR78)
			m.MoveDown()
		}
		if !strings.Contains(m.Render(), BackLabel()) {
			t.Fatalf("Back must render on the last page too (I1: always in viewport)")
		}
		if !strings.Contains(m.Render(), "Элемент 50") {
			t.Fatalf("cursor item must be visible after scrolling")
		}
	})

	t.Run("backless (root) menu pins the last choice at the bottom", func(t *testing.T) {
		m := NewPinList(NewMenuWithoutBack("Корень", "",
			Choice{ID: "a", Label: "Первый"},
			Choice{ID: "exit", Label: "🚪 Exit"},
		), 10)
		r := m.Render()
		if !strings.Contains(r, "🚪 Exit") {
			t.Fatalf("root list must render the exit entry, got:\n%s", r)
		}
		if strings.Contains(r, BackLabel()) {
			t.Fatalf("root list must not render a Back row, got:\n%s", r)
		}
		if strings.Index(r, "🚪 Exit") < strings.Index(r, "Первый") {
			t.Fatalf("the exit entry must render BELOW the body items, got:\n%s", r)
		}
	})
}

// TestPinListCursorAndPaging: cursor movement, clamping and page turns.
// The cursor domain covers the full item list INCLUDING the pinned
// bottom row; it starts on the first body item.
func TestPinListCursorAndPaging(t *testing.T) {
	t.Run("cursor starts on the first body item (0)", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(5)...), 10)
		if m.Cursor() != 0 {
			t.Fatalf("cursor must start at body position 0, got %d", m.Cursor())
		}
	})

	t.Run("move down and back up", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(5)...), 10)
		m.MoveDown()
		m.MoveDown()
		if m.Cursor() != 2 {
			t.Fatalf("want cursor 2, got %d", m.Cursor())
		}
		m.MoveUp()
		if m.Cursor() != 1 {
			t.Fatalf("want cursor 1, got %d", m.Cursor())
		}
	})

	t.Run("move down from the pinned last row wraps to the first item (PR78)", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(3)...), 10)
		for range 3 {
			m.MoveDown()
		}
		if m.Cursor() != 3 {
			t.Fatalf("cursor must reach the pinned row first, got %d", m.Cursor())
		}
		m.MoveDown()
		if m.Cursor() != 0 {
			t.Fatalf("down from the pinned row must wrap to 0, got %d", m.Cursor())
		}
	})

	t.Run("move up from the first item wraps to the pinned last row (PR78)", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(3)...), 10)
		m.MoveUp()
		if m.Cursor() != 3 {
			t.Fatalf("up from the first item must wrap to the pinned row, got %d", m.Cursor())
		}
		m.MoveUp()
		if m.Cursor() != 2 {
			t.Fatalf("up from the pinned row must land on the last body item, got %d", m.Cursor())
		}
	})

	t.Run("wrap is a no-op on single-item and empty menus (PR78)", func(t *testing.T) {
		single := NewPinList(NewMenu("Один", "", numberedChoices(1)...), 10)
		single.MoveDown()
		single.MoveUp()
		if single.Cursor() != 0 {
			t.Fatalf("single-item wrap must be a no-op, got %d", single.Cursor())
		}
		empty := NewPinList(NewMenu("Пусто", "Ничего"), 10)
		empty.MoveDown()
		empty.MoveUp()
		if empty.Cursor() != 0 {
			t.Fatalf("empty-list wrap must be a no-op, got %d", empty.Cursor())
		}
	})

	t.Run("page down then up keeps cursor in range", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(30)...), 10)
		m.PageDown()
		m.PageDown()
		if m.Cursor() <= 10 {
			t.Fatalf("two page downs must move cursor past first page, got %d", m.Cursor())
		}
		m.PageUp()
		if m.Cursor() < 0 || m.Cursor() > 30 {
			t.Fatalf("page up must keep cursor in range, got %d", m.Cursor())
		}
	})

	t.Run("empty menu renders the empty message above Back", func(t *testing.T) {
		m := NewPinList(NewMenu("Пусто", "Nothing found"), 10)
		r := m.Render()
		if !strings.Contains(r, BackLabel()) || !strings.Contains(r, "Nothing found") {
			t.Fatalf("empty menu must render message + Back, got %q", r)
		}
		if strings.Index(r, "Nothing found") > strings.Index(r, BackLabel()) {
			t.Fatalf("empty message must render ABOVE the Back row, got %q", r)
		}
		// Enter on the lone Back resolves through nav.
		menu := m.Menu()
		if got := ResolveKey(menu, m.Cursor(), enter()); got != Back {
			t.Fatalf("enter on empty menu must yield Back, got %#v", got)
		}
	})

	t.Run("viewport height smaller than list pages", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(40)...), 4)
		for range 40 {
			m.MoveDown()
		}
		r := m.Render()
		if !strings.Contains(r, BackLabel()) {
			t.Fatalf("pinned Back missing with tiny viewport")
		}
		if !strings.Contains(r, "Элемент 40") {
			t.Fatalf("cursor at end must be visible with tiny viewport")
		}
	})
}

// TestPinListVisibleRange: the visible body window calculation. The
// body window covers items [0, len-1); the pinned last row is not part
// of the scrolling body.
func TestPinListVisibleRange(t *testing.T) {
	t.Run("body window follows the cursor", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(25)...), 6)
		for range 15 {
			m.MoveDown()
		}
		lo, hi := m.VisibleBody()
		if lo > 15 || hi < 15 {
			t.Fatalf("cursor 15 must be inside body window [%d,%d)", lo, hi)
		}
		if hi-lo > 6 {
			t.Fatalf("body window must not exceed height 6, got %d", hi-lo)
		}
		if hi > 25 {
			t.Fatalf("body window must exclude the pinned last row, got hi=%d", hi)
		}
	})
}

// TestPinListKeyHandling: the key delegation moves the cursor and
// Enter returns the picked value through nav.ResolveKey.
func TestPinListKeyHandling(t *testing.T) {
	t.Run("down key handled", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(5)...), 10)
		if !m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown}) {
			t.Fatalf("down must be handled")
		}
		if m.Cursor() != 1 {
			t.Fatalf("want cursor 1 after down, got %d", m.Cursor())
		}
	})

	t.Run("up key handled", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(5)...), 10)
		m.MoveDown()
		m.MoveDown()
		if !m.HandleKey(tea.KeyPressMsg{Code: tea.KeyUp}) {
			t.Fatalf("up must be handled")
		}
		if m.Cursor() != 1 {
			t.Fatalf("want cursor 1 after up, got %d", m.Cursor())
		}
	})

	t.Run("enter not handled (resolved by the screen)", func(t *testing.T) {
		m := NewPinList(NewMenu("М", "", numberedChoices(5)...), 10)
		if m.HandleKey(enter()) {
			t.Fatalf("enter is a resolution, not a movement")
		}
	})
}
