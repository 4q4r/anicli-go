package tui

// PR109: terminal-height scoping of the fan-out table. The PR98 table
// compresses WIDTH; this file owns the row WINDOW: how many provider
// rows fit the tracked terminal height, which slice of the row list is
// visible (tail-follow during the live fan-out, Shift+↑/↓ review
// scroll once settled) and the «▲/▼ ещё N» indicators.

// fanoutIndicatorLines is the vertical budget of the two scroll
// indicators («▲ ещё N» above, «▼ ещё N» below) reserved whenever the
// table is clipped. Only the sides with hidden rows actually render.
const fanoutIndicatorLines = 2

// fanoutWindowRows returns how many provider rows the table may
// render. The bordered box costs 2R+3 lines (top/header/sep + R rows
// with separators + bottom), so an unclipped table needs
// chrome+2·rows+3 ≤ termHeight; a clipped one also carries the
// indicators. The window never drops below one row — a terminal too
// short even for that overflows (the same degenerate acceptance as the
// PR98 width floors). termHeight ≤ 0 (untracked) means natural mode:
// every row renders.
func fanoutWindowRows(termHeight, chrome, rows int) int {
	if termHeight <= 0 || rows <= 0 {
		return rows
	}
	if chrome+2*rows+3 <= termHeight {
		return rows
	}
	return max(1, (termHeight-chrome-3-fanoutIndicatorLines)/2)
}

// fanoutWindowSlice clamps the first-visible offset against the row
// count and the window size, then returns the half-open [lo, hi) slice
// of rows to render.
func fanoutWindowSlice(total, visible, offset int) (lo, hi int) {
	lo = clamp(offset, 0, max(total-visible, 0))
	return lo, min(lo+visible, total)
}
