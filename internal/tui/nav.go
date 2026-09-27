// Package tui is the interactive terminal face of anicli-go: a
// bubbletea v2 screen-stack application porting the flow structure of
// the Python CLI (anicli-py anicli/cli/*) onto the Go core services.
//
// Navigation is governed by the §5 TUI invariants (PR24 layout):
//
//	I1 — the Back entry is always appended as the LAST item of every
//	     menu and is always visible, pinned at the BOTTOM of the
//	     viewport (PR24: it used to sit at position 0 / the top);
//	     the ROOT menu carries no Back at all — its last item is
//	     «🚪 Выход», which exits the app;
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

	"github.com/an0nx/anicli-go/internal/i18n"
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

// BackLabel is the display label of the Back entry (PR110: resolved
// through i18n; the const became a function because package vars
// freeze the pre-Init default).
func BackLabel() string { return i18n.T("nav.back") }

// Choice is one selectable menu entry.
type Choice struct {
	// ID is a stable identifier for logic (the Back entry uses BackID).
	ID string
	// Label is the rendered, user-facing RU text.
	Label string
	// Value is the payload resolved by ResolveKey; nil means the ID is
	// the payload.
	Value any
	// Disabled renders the row dimmed and non-actionable (PR41 B2):
	// resolving it is the OWNER SCREEN's job — it must answer with a
	// status-line reason instead of acting (never silently nothing).
	Disabled bool
}

// Menu is a navigation prompt: the caller's choices with the Back
// entry appended as the LAST item (I1) and an optional empty-state
// message rendered when no other choices exist (I3).
type Menu struct {
	// Title is the prompt header.
	Title string
	// Items always ends with the Back entry at the last index.
	Items []Choice
	// EmptyMessage is shown instead of the (absent) choices when the
	// menu was built from an empty list.
	EmptyMessage string
}

// NewMenu builds a prompt from choices, appending Back as the LAST
// item (I1 — pinned at the bottom of the viewport by PinList). An
// empty choices slice is legal (I3): the menu then consists of the
// Back entry alone plus the empty-state message.
func NewMenu(title, emptyMessage string, choices ...Choice) Menu {
	items := make([]Choice, 0, len(choices)+1)
	items = append(items, choices...)
	items = append(items, Choice{ID: BackID, Label: BackLabel(), Value: Back})
	return Menu{Title: title, Items: items, EmptyMessage: emptyMessage}
}

// NewMenuWithoutBack builds a prompt with NO Back entry — the root
// menu shape (I1 root exception): the caller's choices alone, the
// last of which («🚪 Выход») occupies the pinned bottom slot.
func NewMenuWithoutBack(title, emptyMessage string, choices ...Choice) Menu {
	return Menu{Title: title, Items: append([]Choice(nil), choices...), EmptyMessage: emptyMessage}
}

// RenderItems renders the plain-text item labels, one per line, in
// menu order (choices first, Back LAST, I1). An empty menu renders
// the empty-state message above the Back row — the same layout
// PinList.Render uses for empty menus.
func (m Menu) RenderItems() string {
	if len(m.Items) == 1 && m.EmptyMessage != "" {
		return m.EmptyMessage + "\n" + BackLabel()
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
// cursor sits on the trailing Back row or when the choice list is
// empty (I3). Any other key returns nil meaning "not resolved, keep
// waiting".
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

// surfaceStatus is the transient status line of a multi-surface
// screen (PR64 #1): every surface switch bumps the generation, a
// verdict records the generation of the surface that set it, and
// visible() gates rendering — a verdict never survives onto another
// surface, and back-navigation cannot resurrect it (generations are
// monotonic, a value is never reused). The file logger keeps the
// full verdict history instead. Screens embed this type; the fields
// promote unchanged.
type surfaceStatus struct {
	// status is the transient verdict line (play verdicts, sync
	// notes, warnings).
	status string
	// statusGen is the surface generation that set status.
	statusGen int
	// surfaceGen is the CURRENT surface generation.
	surfaceGen int
}

// bumpSurface invalidates the status line on a surface switch: the
// next surface starts clean (PR64 #1).
func (s *surfaceStatus) bumpSurface() { s.surfaceGen++ }

// setStatus records a transient verdict scoped to the CURRENT surface.
func (s *surfaceStatus) setStatus(msg string) {
	s.status = msg
	s.statusGen = s.surfaceGen
}

// statusVisible reports whether the current surface owns the status.
func (s *surfaceStatus) statusVisible() bool {
	return s.status != "" && s.statusGen == s.surfaceGen
}

// RootInterruptExits reports whether an interrupt key at the ROOT menu
// means "exit the application": the I2 root exception applies to
// Ctrl-C only; Esc at root merely normalizes to Back (which at root
// means "stay").
func RootInterruptExits(key tea.KeyPressMsg) bool {
	return key.Code == 'c' && key.Mod == tea.ModCtrl
}
