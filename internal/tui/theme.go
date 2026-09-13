package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// richPalette is the color map of the Python original's rich markup
// (PR24 color parity): basic ANSI-16 indexes keep the TUI
// terminal-adaptive exactly like rich — [green] success rows, [red]
// errors, [yellow] warnings, [dim] hints, cyan spinners/titles and
// magenta accents render through the user's own terminal theme
// instead of a hard-coded scheme.
var richPalette = struct {
	cyan, green, red, yellow, magenta, dim color.Color
}{
	cyan:    lipgloss.Color("6"),
	green:   lipgloss.Color("2"),
	red:     lipgloss.Color("1"),
	yellow:  lipgloss.Color("3"),
	magenta: lipgloss.Color("5"),
	dim:     lipgloss.Color("8"),
}

// Theme is the shared dark lipgloss palette. Colors follow the Python
// original's rich styling (cyan titles, green success, red errors,
// yellow warnings, dim hints) through basic ANSI indexes.
type themeSet struct {
	// Title styles screen headers (cyan bold).
	Title lipgloss.Style
	// Cursor styles the highlighted row (cyan bold arrow).
	Cursor lipgloss.Style
	// Item styles ordinary rows.
	Item lipgloss.Style
	// Dim styles hints, separators and empty states.
	Dim lipgloss.Style
	// Success styles OK states (green).
	Success lipgloss.Style
	// Error styles failure states (red).
	Error lipgloss.Style
	// Warning styles cautionary states (yellow).
	Warning lipgloss.Style
	// Accent styles live values (magenta).
	Accent lipgloss.Style
	// Separator draws the divider above the pinned bottom row.
	Separator lipgloss.Style
	// StatusLine styles the bottom status/help line.
	StatusLine lipgloss.Style
}

// theme is the package-wide palette instance. Screens read from it;
// tests render through the same styles (lipgloss degrades to plain
// text when the environment has no color support).
var theme = newTheme()

// newTheme builds the palette from the rich parity map. Foreground-only
// styles keep rendering deterministic across terminal backgrounds.
// Title carries a leading pad (PR24: the header never rubs against
// the screen corner; screens add the blank line below it).
func newTheme() themeSet {
	return themeSet{
		Title: lipgloss.NewStyle().
			Bold(true).
			PaddingLeft(1).
			Foreground(richPalette.cyan),
		Cursor: lipgloss.NewStyle().
			Bold(true).
			Foreground(richPalette.cyan),
		Item: lipgloss.NewStyle(),
		Dim:  lipgloss.NewStyle().Foreground(richPalette.dim),
		Success: lipgloss.NewStyle().
			Bold(true).
			Foreground(richPalette.green),
		Error: lipgloss.NewStyle().
			Bold(true).
			Foreground(richPalette.red),
		Warning: lipgloss.NewStyle().
			Bold(true).
			Foreground(richPalette.yellow),
		Accent: lipgloss.NewStyle().
			Foreground(richPalette.magenta),
		Separator: lipgloss.NewStyle().
			Foreground(richPalette.dim),
		StatusLine: lipgloss.NewStyle().
			Foreground(richPalette.dim),
	}
}
