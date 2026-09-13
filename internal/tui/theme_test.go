package tui

import (
	"fmt"
	"image/color"
	"testing"
)

// colorID renders a palette entry as "type(value)" — basic ANSI
// colors format as ansi.BasicColor(<index>), which pins both the
// basic-ANSI-ness and the exact index.
func colorID(c color.Color) string { return fmt.Sprintf("%T(%v)", c, c) }

// TestRichPaletteParity (PR24): the TUI theme colors follow the
// Python original's rich markup — basic ANSI-16 indexes so the look
// stays terminal-adaptive exactly like rich ([green] success, [red]
// errors, [yellow] warnings, [dim] hints, cyan titles/spinners,
// magenta accents).
func TestRichPaletteParity(t *testing.T) {
	cases := []struct {
		name string
		got  color.Color
		want string
	}{
		{"cyan (titles, cursor)", richPalette.cyan, "ansi.BasicColor(6)"},
		{"green (success)", richPalette.green, "ansi.BasicColor(2)"},
		{"red (errors)", richPalette.red, "ansi.BasicColor(1)"},
		{"yellow (warnings)", richPalette.yellow, "ansi.BasicColor(3)"},
		{"magenta (accent)", richPalette.magenta, "ansi.BasicColor(5)"},
		{"dim (hints, status)", richPalette.dim, "ansi.BasicColor(8)"},
	}
	for _, tc := range cases {
		if got := colorID(tc.got); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
