// Package tui is the interactive terminal face of anicli-go: a
// bubbletea v2 screen-stack application porting the flow structure of
// the Python CLI (anicli-py anicli/cli/*) onto the Go core services.
//
// Navigation is governed by the §5 TUI invariants:
//
//	I1 — the Back entry is always prepended at position 0 of every
//	     menu and is always visible in the viewport;
//	I2 — Esc, Ctrl-C and empty input normalize to the Back sentinel,
//	     never to an application exit and never to a crash; the root
//	     menu is the only place where an interrupt (Ctrl-C) exits the
//	     app, and even there the explicit «Выход» item is the intended
//	     exit path;
//	I3 — an empty choice list is legal: the menu renders Back alone
//	     plus an empty-state message;
//	I4 — exactly one Back sentinel value exists, compared by identity.
package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// backToken is the unexported sentinel type; because it is unexported,
// callers cannot construct a second value that could masquerade as the
// sentinel (I4). The nested field defeats zero-size pointer aliasing
// so identity comparison stays meaningful even in-package.
type backToken struct {
	_ struct{}
}

// Back is THE navigation sentinel (I4): every "go one level up"
// resolution — Esc, Ctrl-C, empty input, an explicit Back pick —
// returns this exact value. Compare with == only.
var Back = &backToken{}

// BackID is the stable menu item id of the Back entry.
const BackID = "__back__"

// BackLabel is the RU display label of the Back entry, matching the
// Python «🔙 Назад» vocabulary.
const BackLabel = "🔙 Назад"

// Choice is one selectable menu entry.
type Choice struct {
	// ID is a stable identifier for logic (the Back entry uses BackID).
	ID string
	// Label is the rendered, user-facing RU text.
	Label string
	// Value is the payload resolved by ResolveKey; nil means the ID is
	// the payload.
	Value any
}

// Menu is a navigation prompt: the caller's choices with the Back
// entry prepended at position 0 (I1) and an optional empty-state
// message rendered when no other choices exist (I3).
type Menu struct {
	// Title is the prompt header.
	Title string
	// Items always starts with the Back entry at index 0.
	Items []Choice
	// EmptyMessage is shown instead of the (absent) choices when the
	// menu was built from an empty list.
	EmptyMessage string
}

// NewMenu builds a prompt from choices, prepending Back at position 0
// (I1). An empty choices slice is legal (I3): the menu then consists
// of the Back entry alone plus the empty-state message.
func NewMenu(title, emptyMessage string, choices ...Choice) Menu {
	items := make([]Choice, 0, len(choices)+1)
	items = append(items, Choice{ID: BackID, Label: BackLabel, Value: Back})
	items = append(items, choices...)
	return Menu{Title: title, Items: items, EmptyMessage: emptyMessage}
}

// RenderItems renders the plain-text item labels, one per line, in
// menu order (Back first, I1). An empty menu keeps the Back row and
// appends the empty-state message below it — the same layout
// PinList.Render uses for empty menus.
func (m Menu) RenderItems() string {
	if len(m.Items) == 1 && m.EmptyMessage != "" {
		return BackLabel + "\n" + m.EmptyMessage
	}
	lines := make([]string, 0, len(m.Items))
	for _, item := range m.Items {
		lines = append(lines, item.Label)
	}
	return strings.Join(lines, "\n")
}

// ResolveKey maps one key press onto a MENU prompt outcome (I2): cancel
// keys (Esc, Ctrl-C) normalize to the Back sentinel; Enter resolves
// the currently highlighted item — which is Back itself when the
// cursor sits on position 0 or when the choice list is empty (I3). Any
// other key returns nil meaning "not resolved, keep waiting".
func ResolveKey(menu Menu, cursor int, key tea.KeyPressMsg) any {
	if IsCancelKey(key) {
		return Back
	}
	if key.Code != tea.KeyEnter {
		return nil
	}
	if cursor < 0 || cursor >= len(menu.Items) {
		// Defensive clamp: an out-of-range cursor is a bug, not a
		// crash — treat it as Back (I2 "never crash").
		return Back
	}
	if menu.Items[cursor].ID == BackID {
		return Back
	}
	if menu.Items[cursor].Value != nil {
		return menu.Items[cursor].Value
	}
	return menu.Items[cursor].ID
}

// ResolveText maps one key press onto a TEXT prompt outcome (I2): Esc
// and Ctrl-C normalize to Back; Enter with a non-empty answer returns
// the trimmed answer; Enter with an empty answer also normalizes to
// Back (the Python `if not query: return` semantics). Any other key
// returns nil meaning "not resolved, keep typing".
func ResolveText(key tea.KeyPressMsg, typed string) any {
	if IsCancelKey(key) {
		return Back
	}
	if key.Code != tea.KeyEnter {
		return nil
	}
	if EmptyInput(typed) {
		return Back
	}
	return strings.TrimSpace(typed)
}

// IsCancelKey reports whether the key is one of the interrupt keys
// that normalize to Back (I2): Esc and Ctrl-C.
func IsCancelKey(key tea.KeyPressMsg) bool {
	if key.Code == tea.KeyEsc {
		return true
	}
	return key.Code == 'c' && key.Mod == tea.ModCtrl
}

// EmptyInput reports whether a typed text answer normalizes to "no
// answer" — the Python `if not query: return` semantics (I2).
func EmptyInput(s string) bool {
	return strings.TrimSpace(s) == ""
}

// RootInterruptExits reports whether an interrupt key at the ROOT menu
// means "exit the application": the I2 root exception applies to
// Ctrl-C only; Esc at root merely normalizes to Back (which at root
// means "stay").
func RootInterruptExits(key tea.KeyPressMsg) bool {
	return key.Code == 'c' && key.Mod == tea.ModCtrl
}
