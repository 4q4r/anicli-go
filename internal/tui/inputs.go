package tui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// TextPromptConfig parameterizes a one-line text prompt.
type TextPromptConfig struct {
	// ID is the screen identity.
	ID string
	// Title renders above the input.
	Title string
	// Placeholder is the dim hint inside the empty input.
	Placeholder string
	// Initial prefills the answer.
	Initial string
	// Status is an optional bottom hint.
	Status string
	// OnSubmit consumes the resolution (answer string or nav.Back).
	OnSubmit func(resolved any) tea.Cmd
}

// TextPrompt is the I2-compliant single-line input screen (search
// query, episode jump, score entry). Enter submits the trimmed text;
// Esc, Ctrl-C and an empty submit normalize to Back.
type TextPrompt struct {
	cfg   TextPromptConfig
	input textinput.Model
}

// NewTextPrompt builds the prompt with the bubbles textinput.
func NewTextPrompt(cfg TextPromptConfig) *TextPrompt {
	input := textinput.New()
	input.Placeholder = cfg.Placeholder
	input.SetValue(cfg.Initial)
	input.Focus()
	return &TextPrompt{cfg: cfg, input: input}
}

// ID implements Screen.
func (t *TextPrompt) ID() string { return t.cfg.ID }

// Init implements Screen. The bubbles v2 textinput needs no startup
// command for plain usage; the cursor blinks via the virtual cursor
// renderer when the program owns a real one.
func (t *TextPrompt) Init() tea.Cmd { return nil }

// Update implements Screen.
func (t *TextPrompt) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if ok {
		resolved := ResolveText(key, t.input.Value())
		if resolved != nil {
			var cmd tea.Cmd
			if t.cfg.OnSubmit != nil {
				cmd = t.cfg.OnSubmit(resolved)
			}
			return t, cmd
		}
	}
	var cmd tea.Cmd
	t.input, cmd = t.input.Update(msg)
	return t, cmd
}

// View implements Screen.
func (t *TextPrompt) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render(t.cfg.Title))
	b.WriteString("\n\n")
	b.WriteString(t.input.View())
	b.WriteString("\n")
	if t.cfg.Status != "" {
		b.WriteString(theme.StatusLine.Render(t.cfg.Status))
	}
	return tea.NewView(b.String())
}

// Value returns the current input text.
func (t *TextPrompt) Value() string { return t.input.Value() }

// typeText injects runes directly (test affordance mirroring real
// key presses without a terminal).
func (t *TextPrompt) typeText(s string) {
	t.input.SetValue(s)
}

// resolve exposes the resolution seam for tests.
func (t *TextPrompt) resolve(key tea.KeyPressMsg) (any, tea.Cmd) {
	resolved := ResolveText(key, t.input.Value())
	var cmd tea.Cmd
	if resolved != nil && t.cfg.OnSubmit != nil {
		cmd = t.cfg.OnSubmit(resolved)
	}
	return resolved, cmd
}

// CheckList is the checkbox multi-select used by the manual search
// grouping flow: the user marks results belonging to the same title.
// It shares the PinList movement model but toggles with space and
// resolves the checked subset on Enter; Esc/Ctrl-C and an empty
// selection resolve to Back (I2). The cursor ranges over the real
// items only — the trailing Back row is appended by the Menu but never
// parked on.
type CheckList struct {
	title string
	items []Choice
	// itemsLower mirrors items' labels lowercased once at build time —
	// the per-keystroke filter compares against the cache instead of
	// re-lowering all labels on every keystroke (PR82 P2#7).
	itemsLower []string
	checked    map[string]bool
	list       *PinList
	// filter is the type-to-search state (PR78); visible holds the
	// real-item indices of the filtered view (nil = unfiltered —
	// every item shows).
	filter  listFilter
	visible []int
}

// NewCheckList builds the multi-select over items (the trailing Back
// row exists in the underlying menu for nav resolution, but the
// cursor never parks on it).
func NewCheckList(title string, items []Choice) *CheckList {
	menu := NewMenu(title, i18n.T("common.empty"), items...)
	lowered := make([]string, len(items))
	for i, item := range items {
		lowered[i] = strings.ToLower(item.Label)
	}
	return &CheckList{
		title:      title,
		items:      items,
		itemsLower: lowered,
		checked:    make(map[string]bool),
		list:       NewPinList(menu, defaultListHeight),
	}
}

// filterActive reports whether a type-to-search query is engaged (the
// owning screens delegate Esc to the checklist while it is).
func (c *CheckList) filterActive() bool { return c.filter.active() }

// viewLen is the number of rows in the current (possibly filtered)
// view.
func (c *CheckList) viewLen() int {
	if c.visible == nil {
		return len(c.items)
	}
	return len(c.visible)
}

// realIndex maps a view row onto its full item index.
func (c *CheckList) realIndex(viewIdx int) int {
	if c.visible == nil {
		return viewIdx
	}
	return c.visible[viewIdx]
}

// applyFilter rebuilds the underlying PinList over the filtered
// choices, keeping the cursor parked on the same item when it survives
// (viewport follows through the normal PinList windowing).
func (c *CheckList) applyFilter() {
	prev := cursorID(c.list)
	shown := filterChoicesLowered(c.items, c.itemsLower, c.filter.value())
	if !c.filter.active() {
		c.visible = nil
		c.list = NewPinList(NewMenu(c.title, i18n.T("common.empty"), c.items...), defaultListHeight)
		restoreCursor(c.list, prev)
		return
	}
	index := make(map[string]int, len(c.items))
	for i, item := range c.items {
		index[item.ID] = i
	}
	c.visible = make([]int, 0, len(shown))
	choices := make([]Choice, 0, len(shown))
	for _, item := range shown {
		c.visible = append(c.visible, index[item.ID])
		choices = append(choices, item)
	}
	c.list = NewPinList(NewMenu(c.title, i18n.T("common.empty"), choices...), defaultListHeight)
	restoreCursor(c.list, prev)
}

// clampBody keeps the cursor on a real item (never the trailing Back
// row, never out of range of the current view).
func (c *CheckList) clampBody() {
	if c.list.Cursor() >= c.viewLen() {
		c.list.Jump(max(c.viewLen()-1, 0))
	}
}

// MoveDown moves the cursor to the next item, wrapping from the last
// visible item onto the first (PR78 wrap — within the filtered view).
func (c *CheckList) MoveDown() {
	if c.viewLen() == 0 {
		return
	}
	if c.list.Cursor() >= c.viewLen()-1 {
		c.list.Jump(0)
		return
	}
	c.list.MoveDown()
	c.clampBody()
}

// MoveUp moves the cursor to the previous item, wrapping from the
// first visible item onto the last (PR78).
func (c *CheckList) MoveUp() {
	if c.viewLen() == 0 {
		return
	}
	if c.list.Cursor() <= 0 {
		c.list.Jump(c.viewLen() - 1)
		return
	}
	c.list.MoveUp()
}

// Toggle flips the checked state of the current item.
func (c *CheckList) Toggle() {
	idx := c.list.Cursor()
	if idx < 0 || idx >= c.viewLen() {
		return
	}
	item := c.items[c.realIndex(idx)]
	c.checked[item.ID] = !c.checked[item.ID]
}

// Checked reports the checked state of one body item.
func (c *CheckList) Checked(bodyIndex int) bool {
	if bodyIndex < 0 || bodyIndex >= len(c.items) {
		return false
	}
	return c.checked[c.items[bodyIndex].ID]
}

// SelectAll sets every item's checked state.
func (c *CheckList) SelectAll(on bool) {
	for _, item := range c.items {
		c.checked[item.ID] = on
	}
}

// CheckedItems returns the checked items in display order.
func (c *CheckList) CheckedItems() []Choice {
	var out []Choice
	for _, item := range c.items {
		if c.checked[item.ID] {
			out = append(out, item)
		}
	}
	return out
}

// Resolve maps a key press onto the outcome: Esc/Ctrl-C → Back,
// Enter → the checked subset (Back when none checked).
func (c *CheckList) Resolve(key tea.KeyPressMsg) any {
	if IsCancelKey(key) {
		return Back
	}
	if key.Code != tea.KeyEnter {
		return nil
	}
	checked := c.CheckedItems()
	if len(checked) == 0 {
		return Back
	}
	return checked
}

// InvertSelection flips every item's checked state.
func (c *CheckList) InvertSelection() {
	for _, item := range c.items {
		c.checked[item.ID] = !c.checked[item.ID]
	}
}

// checklistBoundRunes are the checklist's own single-letter hotkeys
// (a all/none, i invert, j/k movement, space toggle) — at rest they
// keep their meaning and never start the filter.
var checklistBoundRunes = map[rune]bool{'a': true, 'i': true, 'j': true, 'k': true, ' ': true}

// HandleKey applies filter, movement and toggle keys, reporting
// whether the key was consumed. While the filter is engaged it takes
// every printable key and Backspace; Esc clears it (the owning screen
// routes the NEXT Esc to its own Back).
func (c *CheckList) HandleKey(key tea.KeyPressMsg) bool {
	if consumed, changed := c.filter.consume(key, checklistBoundRunes); consumed {
		if changed {
			c.applyFilter()
		}
		return true
	}
	switch key.Code { //nolint:exhaustive // movement + toggle only
	case tea.KeyDown:
		c.MoveDown()
	case tea.KeyUp:
		c.MoveUp()
	case tea.KeySpace:
		c.Toggle()
	case tea.KeyPgDown, tea.KeyPgUp:
		if !c.list.HandleKey(key) {
			return false
		}
		c.clampBody()
	case 'j':
		c.MoveDown()
	case 'k':
		c.MoveUp()
	case 'a':
		// Toggle between select-all and deselect-all.
		allChecked := len(c.CheckedItems()) == len(c.items)
		c.SelectAll(!allChecked)
	case 'i':
		c.InvertSelection()
	default:
		return false
	}
	return true
}

// Render draws the VISIBLE WINDOW of the list with ● markers on
// checked items (python questionary.checkbox parity: ○ unchecked, ●
// checked). The window comes from the internal PinList — the same
// viewport machinery the sources list scrolls with (PR78: the
// unbounded render dumped every merged row, and the cursor escaped
// below the screen bottom «никогда больше не возвращаясь»). The title
// renders above the window and stays pinned; the type-to-search line
// (when engaged) sits between title and rows; the «ещё N» hint
// mirrors the PinList convention. The cursor never parks on the
// trailing Back row, so the window domain is exactly the list rows.
func (c *CheckList) Render() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render(c.title))
	b.WriteString("\n\n")
	if line := c.filter.render(); line != "" {
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	menu := c.list.Menu()
	lo, hi := c.list.VisibleBody()
	for i := lo; i < hi; i++ {
		item := menu.Items[i]
		marker := "○"
		if c.checked[item.ID] {
			marker = "●"
		}
		if c.list.Cursor() == i {
			b.WriteString(theme.Cursor.Render("▸ " + marker + " " + item.Label))
		} else {
			b.WriteString(theme.Item.Render("  " + marker + " " + item.Label))
		}
		b.WriteString("\n")
	}
	if remaining := c.list.bodyEnd() - hi; remaining > 0 {
		b.WriteString(theme.Dim.Render(i18n.T("common.more", i18n.Vals{"count": strconv.Itoa(remaining)})))
		b.WriteString("\n")
	}
	b.WriteString(theme.StatusLine.Render(i18n.T("inputs.checklist_hint")))
	return b.String()
}
