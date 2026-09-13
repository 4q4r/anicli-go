package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestTextPromptResolution: the text prompt (search input, episode
// jump, score entry) resolves Enter/Esc/Ctrl-C per I2.
func TestTextPromptResolution(t *testing.T) {
	t.Run("typed text returned on enter", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "🔎 Поиск:"})
		tp.typeText("Наруто")
		pick, cmd := tp.resolve(enter())
		if pick == Back || pick != "Наруто" {
			t.Fatalf("want typed answer, got %#v", pick)
		}
		_ = cmd
	})

	t.Run("empty enter normalizes to Back", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "🔎 Поиск:"})
		pick, _ := tp.resolve(enter())
		if pick != Back {
			t.Fatalf("empty enter must be Back, got %#v", pick)
		}
	})

	t.Run("esc normalizes to Back", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "🔎 Поиск:"})
		tp.typeText("что-то")
		pick, _ := tp.resolve(esc())
		if pick != Back {
			t.Fatalf("esc must discard input and be Back, got %#v", pick)
		}
	})

	t.Run("ctrl+c normalizes to Back", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "T"})
		pick, _ := tp.resolve(ctrlC())
		if pick != Back {
			t.Fatalf("ctrl+c must be Back, got %#v", pick)
		}
	})

	t.Run("typing appends runes", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "T"})
		for _, r := range "ван панч" {
			tp.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		if got := tp.Value(); got != "ван панч" {
			t.Fatalf("want %q, got %q", "ван панч", got)
		}
	})

	t.Run("view shows the prompt title and hint", func(t *testing.T) {
		tp := NewTextPrompt(TextPromptConfig{ID: "q", Title: "🔎 Поиск:"})
		v := tp.View().Content
		if !strings.Contains(v, "🔎 Поиск:") {
			t.Fatalf("title missing: %q", v)
		}
	})
}

// TestCheckboxMultiSelect: the manual-grouping component.
func TestCheckboxMultiSelect(t *testing.T) {
	items := []Choice{
		{ID: "r1", Label: "Наруто [animego]"},
		{ID: "r2", Label: "Наруто [anilib]"},
		{ID: "r3", Label: "Bleach [animego]"},
	}

	t.Run("space toggles the item under the cursor", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		cb.MoveDown() // to r1
		cb.Toggle()
		if !cb.Checked(0) {
			t.Fatalf("r1 must be checked after toggle")
		}
		cb.Toggle()
		if cb.Checked(0) {
			t.Fatalf("r1 must be unchecked after second toggle")
		}
	})

	t.Run("enter returns the checked subset in order", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		cb.MoveDown()
		cb.Toggle() // r1
		cb.MoveDown()
		cb.Toggle() // r3... wait cursor is now r2 -> toggling r2
		cb.Toggle() // untoggle
		cb.MoveDown()
		cb.Toggle() // r3
		got := cb.CheckedItems()
		if len(got) != 2 || got[0].ID != "r1" || got[1].ID != "r3" {
			t.Fatalf("want [r1 r3], got %v", idsOf(got))
		}
	})

	t.Run("esc returns Back", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		if got := cb.Resolve(esc()); got != Back {
			t.Fatalf("esc must be Back, got %#v", got)
		}
	})

	t.Run("enter with none checked returns Back (empty selection legal)", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		if got := cb.Resolve(enter()); got != Back {
			t.Fatalf("empty selection must be Back, got %#v", got)
		}
	})

	t.Run("render shows check markers", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		cb.MoveDown()
		cb.Toggle()
		v := cb.Render()
		if !strings.Contains(v, "✔") {
			t.Fatalf("checked marker missing:\n%s", v)
		}
	})

	t.Run("select all helper", func(t *testing.T) {
		cb := NewCheckList("Группировка", items)
		cb.MoveDown()
		cb.SelectAll(true)
		got := cb.CheckedItems()
		if len(got) != 3 {
			t.Fatalf("select-all must check every item, got %v", idsOf(got))
		}
	})
}

func idsOf(cs []Choice) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}
