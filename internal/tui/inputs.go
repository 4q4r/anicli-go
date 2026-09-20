package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
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
	title   string
	items   []Choice
	checked map[string]bool
	list    *PinList
}

// NewCheckList builds the multi-select over items (the trailing Back
// row exists in the underlying menu for nav resolution, but the
// cursor never parks on it).
func NewCheckList(title string, items []Choice) *CheckList {
	menu := NewMenu(title, "Нет элементов", items...)
	return &CheckList{
		title:   title,
		items:   items,
		checked: make(map[string]bool),
		list:    NewPinList(menu, defaultListHeight),
	}
}

// clampBody keeps the cursor on a real item (never the trailing Back
// row, never out of range).
func (c *CheckList) clampBody() {
	if c.list.Cursor() >= len(c.items) {
		c.list.Jump(max(len(c.items)-1, 0))
	}
}

// MoveDown moves the cursor to the next item, wrapping from the last
// real item onto the first (PR78 wrap — the trailing Back row is never
// part of the checklist cursor domain).
func (c *CheckList) MoveDown() {
	if len(c.items) == 0 {
		return
	}
	if c.list.Cursor() >= len(c.items)-1 {
		c.list.Jump(0)
		return
	}
	c.list.MoveDown()
	c.clampBody()
}

// MoveUp moves the cursor to the previous item, wrapping from the
// first real item onto the last (PR78).
func (c *CheckList) MoveUp() {
	if len(c.items) == 0 {
		return
	}
	if c.list.Cursor() <= 0 {
		c.list.Jump(len(c.items) - 1)
		return
	}
	c.list.MoveUp()
}

// Toggle flips the checked state of the current item.
func (c *CheckList) Toggle() {
	idx := c.list.Cursor()
	if idx < 0 || idx >= len(c.items) {
		return
	}
	c.checked[c.items[idx].ID] = !c.checked[c.items[idx].ID]
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

// HandleKey applies movement and toggle keys, reporting whether the
// key was consumed.
func (c *CheckList) HandleKey(key tea.KeyPressMsg) bool {
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

// Render draws the VISIBLE WINDOW of the list with ● markers on checked
// items (python questionary.checkbox parity: ○ unchecked, ● checked).
// The window comes from the internal PinList — the same viewport
// machinery the sources list scrolls with (PR78: the unbounded render
// dumped every merged row, and the cursor escaped below the screen
// bottom «никогда больше не возвращаясь»). The title renders above the
// window and stays pinned; the «ещё N» hint mirrors the PinList
// convention. The cursor never parks on the trailing Back row, so the
// window domain is exactly the real items.
func (c *CheckList) Render() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render(c.title))
	b.WriteString("\n\n")
	lo, hi := c.list.VisibleBody()
	for i := lo; i < hi; i++ {
		item := c.items[i]
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
	if remaining := len(c.items) - hi; remaining > 0 {
		b.WriteString(theme.Dim.Render(fmt.Sprintf("  … ещё %d", remaining)))
		b.WriteString("\n")
	}
	b.WriteString(theme.StatusLine.Render("space — отметить · a — все/ничего · i — инверт · enter — продолжить · esc — назад"))
	return b.String()
}
