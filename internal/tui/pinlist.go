package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// PinList is the menu list component enforcing the viewport half of
// invariant I1 (PR24 layout): the LAST menu item (the Back entry — or
// «🚪 Выход» on the backless root menu) is PINNED — rendered below the
// scrolling body on every page — while items 0..n-1 scroll in a window
// that follows the cursor. The cursor indexes the full menu (last
// index = pinned row), so down-arrow from the last body item lands on
// the pinned row.
//
// It is deliberately hand-rolled instead of reusing the bubbles list:
// the §5 semantics (pinned last row, cursor domain including Back,
// guaranteed visibility) are exactly the contract under test, and a
// pure-logic component keeps them provable without a terminal.
type PinList struct {
	menu    Menu
	height  int // body rows available for items 0..n-2
	cursor  int // index into menu.Items (last index = pinned row)
	offset  int // first visible body index (into Items, < len-1)
	markers map[int]string
}

// NewPinList builds the list. height is the number of body rows
// (excluding the pinned bottom row); <= 0 behaves as 1.
func NewPinList(menu Menu, height int) *PinList {
	if height < 1 {
		height = 1
	}
	return &PinList{menu: menu, height: height, cursor: 0, offset: 0}
}

// SetMarker attaches a marker string (e.g. "★", "✔") to one item index.
func (l *PinList) SetMarker(index int, marker string) {
	if l.markers == nil {
		l.markers = make(map[int]string)
	}
	l.markers[index] = marker
}

// Menu returns the underlying menu (for nav.ResolveKey).
func (l *PinList) Menu() Menu { return l.menu }

// Cursor returns the current menu index.
func (l *PinList) Cursor() int { return l.cursor }

// VisibleBody returns the half-open [lo, hi) window of body indices
// currently rendered (indices are into menu.Items; the pinned last row
// is never part of the window).
func (l *PinList) VisibleBody() (int, int) {
	last := len(l.menu.Items) - 1 // pinned row
	if last < 0 {
		last = 0
	}
	hi := min(l.offset+l.height, last)
	lo := min(l.offset, last)
	return lo, hi
}

// bodyEnd is the exclusive end of the scrolling body (the pinned row
// index).
func (l *PinList) bodyEnd() int { return max(len(l.menu.Items)-1, 0) }

// MoveDown moves the cursor one item down, wrapping at the pinned
// bottom row back onto the first item (PR78: «бесконечный скролл» —
// uniform boundary wrap on every list); the wrap is a no-op on empty
// and single-row menus. follow keeps the cursor inside the window.
func (l *PinList) MoveDown() {
	if len(l.menu.Items) <= 1 {
		return
	}
	if l.cursor >= len(l.menu.Items)-1 {
		l.cursor = 0
	} else {
		l.cursor++
	}
	l.follow()
}

// MoveUp moves the cursor one item up, wrapping at the first item onto
// the pinned bottom row (PR78 wrap; single/empty menus are a no-op).
func (l *PinList) MoveUp() {
	if len(l.menu.Items) <= 1 {
		return
	}
	if l.cursor <= 0 {
		l.cursor = len(l.menu.Items) - 1
	} else {
		l.cursor--
	}
	l.follow()
}

// PageDown moves the cursor one body page down.
func (l *PinList) PageDown() {
	l.cursor = min(l.cursor+l.height, len(l.menu.Items)-1)
	l.follow()
}

// PageUp moves the cursor one body page up, never above position 0.
func (l *PinList) PageUp() {
	l.cursor = max(l.cursor-l.height, 0)
	l.follow()
}

// Jump moves the cursor to index (clamped) — used by "go to episode"
// style flows.
func (l *PinList) Jump(index int) {
	l.cursor = clamp(index, 0, len(l.menu.Items)-1)
	l.follow()
}

// follow adjusts the body offset so the cursor stays visible: the
// pinned row guarantees the LAST cursor; the window [offset,
// offset+height) must contain any cursor inside the body.
func (l *PinList) follow() {
	end := l.bodyEnd()
	if l.cursor >= end {
		return
	}
	// Body indices run [0, end). Keep [offset, offset+height)
	// covering the cursor with minimal movement.
	for l.cursor >= l.offset+l.height {
		l.offset++
	}
	for l.cursor < l.offset && l.offset > 0 {
		l.offset--
	}
}

// HandleKey applies movement keys; it reports whether the key was a
// movement it handled. Enter/Esc/Ctrl-C are NOT handled here — the
// owning screen resolves them through nav.ResolveKey.
func (l *PinList) HandleKey(key tea.KeyPressMsg) bool {
	switch key.Code { //nolint:exhaustive // only movement keys matter
	case tea.KeyDown:
		l.MoveDown()
	case tea.KeyUp:
		l.MoveUp()
	case tea.KeyPgDown, tea.KeyPgUp:
		if key.Code == tea.KeyPgDown {
			l.PageDown()
		} else {
			l.PageUp()
		}
	case 'j':
		l.MoveDown()
	case 'k':
		l.MoveUp()
	default:
		return false
	}
	return true
}

// Render draws the visible body window with a cursor pointer and
// optional per-item markers, then the separator and the pinned bottom
// row (I1: the Back/Exit entry is always the last rendered line).
// Rendering is plain text; the screen layer applies lipgloss styles
// around it.
func (l *PinList) Render() string {
	var b strings.Builder

	if len(l.menu.Items) == 0 {
		// Defensive: a backless menu built from zero choices. Nothing
		// is selectable — the empty-state message (if any) alone.
		if l.menu.EmptyMessage != "" {
			b.WriteString(theme.Dim.Render(l.menu.EmptyMessage))
			b.WriteString("\n")
		}
		return b.String()
	}

	if len(l.menu.Items) == 1 {
		// Lone pinned row (empty menu, I3): the empty-state message
		// renders above it.
		if l.menu.EmptyMessage != "" {
			b.WriteString(theme.Dim.Render(l.menu.EmptyMessage))
			b.WriteString("\n")
		}
		b.WriteString(renderRow(l.menu.Items[0], 0, l.cursor, ""))
		return b.String()
	}

	lo, hi := l.VisibleBody()
	for i := lo; i < hi; i++ {
		b.WriteString(renderRow(l.menu.Items[i], i, l.cursor, l.markers[i]))
	}
	if hi < l.bodyEnd() {
		b.WriteString(theme.Dim.Render(i18n.T("common.more", i18n.Vals{"count": strconv.Itoa(l.bodyEnd() - hi)})))
		b.WriteString("\n")
	}

	// Pinned bottom row: always visible (I1).
	b.WriteString(theme.Separator.Render(strings.Repeat("─", 40)) + "\n")
	b.WriteString(renderRow(l.menu.Items[len(l.menu.Items)-1], len(l.menu.Items)-1, l.cursor, l.markers[len(l.menu.Items)-1]))
	return b.String()
}

// renderRow draws one item line with the pointer and marker.
func renderRow(item Choice, index, cursor int, marker string) string {
	pointer := "  "
	if index == cursor {
		pointer = "▸ "
	}
	text := item.Label
	if marker != "" {
		text = marker + " " + text
	}
	return rowStyle(item, index, cursor).Render(pointer+text) + "\n"
}

// rowStyle picks the row style (PR41 B2): disabled rows render dimmed
// regardless of the cursor so the non-actionable state stays visible.
func rowStyle(item Choice, index, cursor int) lipgloss.Style {
	if item.Disabled {
		return theme.Dim
	}
	if index == cursor {
		return theme.Cursor
	}
	return theme.Item
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
