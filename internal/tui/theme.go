package tui

import "charm.land/lipgloss/v2"

// Theme is the shared dark lipgloss palette. Colors follow the dark
// terminal defaults of the Python original's rich styling (cyan titles,
// green success, red errors, yellow warnings, dim hints).
type themeSet struct {
	// Title styles screen headers (cyan bold).
	Title lipgloss.Style
	// Cursor styles the highlighted row.
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
	// Separator draws the divider under the pinned Back row.
	Separator lipgloss.Style
	// StatusLine styles the bottom status/help line.
	StatusLine lipgloss.Style
}

// theme is the package-wide palette instance. Screens read from it;
// tests render through the same styles (lipgloss degrades to plain
// text when the environment has no color support).
var theme = newTheme()

// newTheme builds the palette. Foreground-only styles keep rendering
// deterministic across terminal backgrounds. Title carries a leading
// pad (PR24: the header never rubs against the screen corner; screens
// add the blank line below it).
func newTheme() themeSet {
	return themeSet{
		Title: lipgloss.NewStyle().
			Bold(true).
			PaddingLeft(1).
			Foreground(lipgloss.Color("#56B6C2")),
		Cursor: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")),
		Item: lipgloss.NewStyle(),
		Dim:  lipgloss.NewStyle().Foreground(lipgloss.Color("#7F848E")),
		Success: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#98C379")),
		Error: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#E06C75")),
		Warning: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#E5C07B")),
		Accent: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#C678DD")),
		Separator: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#4B5263")),
		StatusLine: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7F848E")),
	}
}
