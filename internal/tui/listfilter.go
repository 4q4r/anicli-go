// PR78 type-to-search for the big content lists (the provider-results
// checklist, the session episode list, the offline download file
// list): any printable keystroke opens a search line between the
// header and the list and narrows it with a live case-insensitive
// substring filter. Backspace edits; Esc clears; the second Esc falls
// through to the screen's normal Back. The screen's single-letter
// hotkeys are never silently shadowed — bound runes keep their meaning
// and do not trigger the filter (documented per screen in .sdd).

package tui

import (
	"strings"
	"unicode"

	"github.com/an0nx/anicli-go/internal/i18n"

	tea "charm.land/bubbletea/v2"
)

// pinListBoundRunes are the movement hotkeys every PinList surface
// carries (the vim aliases) — they keep their meaning and never start
// the filter.
var pinListBoundRunes = map[rune]bool{'j': true, 'k': true}

// listFilter is the query state of one content list.
type listFilter struct {
	query []rune
}

// active reports whether a filter query is engaged.
func (f *listFilter) active() bool { return len(f.query) > 0 }

// value returns the current query text.
func (f *listFilter) value() string { return string(f.query) }

// clear drops the query (unfiltered view).
func (f *listFilter) clear() { f.query = nil }

// consume applies one key to the filter. Bound runes never START the
// filter (the hotkey wins at rest); once active, every printable key
// edits the query. Returns consumed (the key was filter input) and
// changed (the query changed — the list must re-filter).
func (f *listFilter) consume(key tea.KeyPressMsg, bound map[rune]bool) (consumed, changed bool) {
	if IsCancelKey(key) {
		if f.active() {
			f.query = nil
			return true, true
		}
		return false, false
	}
	if key.Code == tea.KeyBackspace {
		if !f.active() {
			return false, false
		}
		f.query = f.query[:len(f.query)-1]
		return true, true
	}
	r, ok := printableRune(key)
	if !ok {
		return false, false
	}
	if !f.active() && bound[r] {
		return false, false
	}
	f.query = append(f.query, r)
	return true, true
}

// printableRune reports the key's rune for plain printable presses.
// Special keys live above unicode.MaxRune, control runes (Enter, Esc,
// Backspace, Tab) are not printable, and any modifier combination
// (Ctrl-C, Alt-…) is not text input.
func printableRune(key tea.KeyPressMsg) (rune, bool) {
	if key.Mod != 0 {
		return 0, false
	}
	r := key.Code
	if r <= 0 || r > unicode.MaxRune || !unicode.IsPrint(r) {
		return 0, false
	}
	return r, true
}

// render draws the search input line (empty string while inactive —
// the line appears with the first typed character, per the owner's
// spec).
func (f *listFilter) render() string {
	if !f.active() {
		return ""
	}
	return theme.StatusLine.Render(i18n.T("filter.line", i18n.Vals{"query": f.value()}))
}

// filterLineAbove prepends the filter input line (when active) above a
// rendered list block — the line sits between the header and the list.
func filterLineAbove(f listFilter, list string) string {
	if line := f.render(); line != "" {
		return line + "\n\n" + list
	}
	return list
}

// filterChoices keeps the choices whose label contains query
// case-insensitively; an empty query returns the slice unchanged (the
// SAME items — filtering never copies semantics onto new rows).
func filterChoices(items []Choice, query string) []Choice {
	if strings.TrimSpace(query) == "" {
		return items
	}
	q := strings.ToLower(query)
	out := make([]Choice, 0, len(items))
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Label), q) {
			out = append(out, item)
		}
	}
	return out
}

// filterChoicesLowered is filterChoices over pre-lowered labels (the
// CheckList cache); the matching semantics are identical.
func filterChoicesLowered(items []Choice, lowered []string, query string) []Choice {
	if strings.TrimSpace(query) == "" {
		return items
	}
	q := strings.ToLower(query)
	out := make([]Choice, 0, len(items))
	for i, item := range items {
		if strings.Contains(lowered[i], q) {
			out = append(out, item)
		}
	}
	return out
}

// cursorID returns the ID of the row under the cursor of a rebuilt
// list ("" for the pinned Back row, empty menus and nil lists — the
// first build runs before the list exists).
func cursorID(list *PinList) string {
	if list == nil {
		return ""
	}
	items := list.Menu().Items
	if list.Cursor() >= 0 && list.Cursor() < len(items)-1 {
		return items[list.Cursor()].ID
	}
	return ""
}

// restoreCursor re-parks a rebuilt list's cursor on the item with id
// (falling back to the first row when the id left the view).
func restoreCursor(list *PinList, id string) {
	if id != "" {
		for i, item := range list.Menu().Items {
			if item.ID == id {
				list.Jump(i)
				return
			}
		}
	}
	list.Jump(0)
}
