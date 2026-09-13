package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// PinList is the menu list component enforcing the viewport half of
// invariant I1: menu position 0 (the Back entry) is PINNED — rendered
// above the scrolling body on every page — while items 1..n scroll in
// a window that follows the cursor. The cursor indexes the full menu
// (0 = Back), so up-arrow from the first body item lands on Back.
//
// It is deliberately hand-rolled instead of reusing the bubbles list:
// the §5 semantics (pinned position 0, cursor domain including Back,
// guaranteed visibility) are exactly the contract under test, and a
// pure-logic component keeps them provable without a terminal.
type PinList struct {
	menu    Menu
	height  int // body rows available for items 1..n
	cursor  int // index into menu.Items (0 = pinned Back)
	offset  int // first visible body index (into Items, >= 1)
	markers map[int]string
}

// NewPinList builds the list. height is the number of body rows
// (excluding the pinned Back row); <= 0 behaves as 1.
func NewPinList(menu Menu, height int) *PinList {
	if height < 1 {
		height = 1
	}
	return &PinList{menu: menu, height: height, cursor: 0, offset: 1}
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
// currently rendered (indices are into menu.Items).
func (l *PinList) VisibleBody() (int, int) {
	last := len(l.menu.Items)
	hi := min(l.offset+l.height, last)
	lo := min(l.offset, last)
	return lo, hi
}

// MoveDown moves the cursor one item down, clamping at the end and
// keeping the cursor inside the visible window.
func (l *PinList) MoveDown() {
	if l.cursor < len(l.menu.Items)-1 {
		l.cursor++
	}
	l.follow()
}

// MoveUp moves the cursor one item up, clamping at the pinned Back
// row (position 0).
func (l *PinList) MoveUp() {
	if l.cursor > 0 {
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
// pinned row guarantees cursor 0; the window must contain any cursor
// >= 1.
func (l *PinList) follow() {
	if l.cursor <= 0 {
		return
	}
	// Body indices run [1, len). Keep [offset, offset+height)
	// covering the cursor with minimal movement.
	for l.cursor >= l.offset+l.height {
		l.offset++
	}
	for l.cursor < l.offset && l.offset > 1 {
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

// Render draws the pinned Back row, then the visible body window with
// a cursor pointer and optional per-item markers. Rendering is plain
// text; the screen layer applies lipgloss styles around it.
func (l *PinList) Render() string {
	var b strings.Builder

	// Pinned Back row: always visible (I1).
	b.WriteString(renderRow(l.menu.Items[0], 0, l.cursor, l.markers[0]))
	b.WriteString(theme.Separator.Render(strings.Repeat("─", 40)) + "\n")

	if len(l.menu.Items) == 1 {
		if l.menu.EmptyMessage != "" {
			b.WriteString(theme.Dim.Render(l.menu.EmptyMessage))
			b.WriteString("\n")
		}
		return b.String()
	}

	lo, hi := l.VisibleBody()
	if lo < 1 {
		lo = 1
	}
	for i := lo; i < hi; i++ {
		b.WriteString(renderRow(l.menu.Items[i], i, l.cursor, l.markers[i]))
	}
	if hi < len(l.menu.Items) {
		b.WriteString(theme.Dim.Render(fmt.Sprintf("  … ещё %d", len(l.menu.Items)-hi)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderRow draws one item line with the pointer and marker.
func renderRow(item Choice, index, cursor int, marker string) string {
	pointer := "  "
	style := theme.Item
	if index == cursor {
		pointer = "▸ "
		style = theme.Cursor
	}
	text := item.Label
	if marker != "" {
		text = marker + " " + text
	}
	return style.Render(pointer + text)
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
