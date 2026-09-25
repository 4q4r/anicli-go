package tui

// PR109 tests: terminal-HEIGHT scoping of the fan-out table. The PR98
// bordered table already compresses WIDTH; this wave pins the row
// window: the table renders at most (terminal height − chrome) data
// rows, tail-follows the in-flight rows during the live fan-out, and
// lets Shift+↑/↓ scroll the settled table for review while the PR62
// checklist below keeps the plain arrows.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestFanoutWindowRowsBudget pins the pure row-window budget: the box
// costs 2R+3 lines, so an unclipped table needs chrome+2·rows+3 ≤
// terminal height; once clipped, the two scroll indicators are part of
// the budget and the window never drops below one row. Height 0 (the
// untracked terminal) means natural mode — every row renders.
func TestFanoutWindowRowsBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		termHeight int
		chrome     int
		rows       int
		want       int
	}{
		{"tall terminal fits every row", 30, 8, 4, 4},
		{"short terminal clips and reserves the indicators", 30, 8, 28, 8},
		{"very short terminal shrinks the window further", 17, 8, 28, 2},
		{"degenerate terminal keeps one visible row", 14, 8, 28, 1},
		{"untracked height means natural mode", 0, 8, 28, 28},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fanoutWindowRows(tc.termHeight, tc.chrome, tc.rows); got != tc.want {
				t.Fatalf("fanoutWindowRows(%d, %d, %d) = %d, want %d",
					tc.termHeight, tc.chrome, tc.rows, got, tc.want)
			}
		})
	}
}

// fanoutTestProviders builds n uniform roster rows («Провайдер 00» …)
// so the top/tail visibility assertions have stable names.
func fanoutTestProviders(n int) []ProviderMeta {
	rows := make([]ProviderMeta, 0, n)
	for i := range n {
		rows = append(rows, ProviderMeta{ID: fmt.Sprintf("p%02d", i), Name: fmt.Sprintf("Провайдер %02d", i)})
	}
	return rows
}

// fanoutBoxDataRows counts the data rows rendered inside the bordered
// box (box lines = 2R+3).
func fanoutBoxDataRows(v string) int {
	box := tableBox(v)
	if len(box) < 5 {
		return 0
	}
	return (len(box) - 3) / 2
}

// fanoutScreenRows counts the full rendered screen height (the view
// string carries no trailing newline).
func fanoutScreenRows(v string) int {
	return strings.Count(v, "\n") + 1
}

// settleAllZero feeds every provider a clean zero-result settle — the
// smallest possible settled screen (no results checklist, the
// «Ничего не найдено» note instead).
func settleAllZero(sp *searchProgress) *searchProgress {
	settlements := make([]providerResultMsg, 0, len(sp.rows))
	for _, row := range sp.rows {
		settlements = append(settlements, providerResultMsg{provider: row})
	}
	return settleSearch(sp, settlements...)
}

// settleAllResults settles every provider with one uniquely-titled
// result, so the settled screen grows the PR62 checklist below.
func settleAllResults(sp *searchProgress) *searchProgress {
	settlements := make([]providerResultMsg, 0, len(sp.rows))
	for _, row := range sp.rows {
		settlements = append(settlements, providerResultMsg{provider: row, results: []contracts.SearchResult{
			{Title: fmt.Sprintf("Тайтл %s", row.ID), URL: "u", SourceID: row.ID},
		}})
	}
	return settleSearch(sp, settlements...)
}

// fanoutViewportScreen builds the fan-out screen over n providers with
// the tracked terminal size already delivered (the runtime always
// sends WindowSizeMsg before the first render).
func fanoutViewportScreen(n, width, height int) *searchProgress {
	fs := newFakeSearch()
	fs.providers = fanoutTestProviders(n)
	sp := NewSearchProgress(hybridDeps(fs, nil, nil, nil), "наруто")
	next, _ := sp.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return next.(*searchProgress)
}

// TestSearchFanoutViewHeightBounded pins the core PR109 contract: at
// 10/30/60 providers the settled screen NEVER exceeds the tracked
// terminal height — the table window shrinks, the top row stays
// visible (review starts at the roster head) and the tail waits behind
// the «▼ ещё N» indicator.
func TestSearchFanoutViewHeightBounded(t *testing.T) {
	t.Parallel()

	for _, n := range []int{10, 30, 60} {
		t.Run(fmt.Sprintf("%d providers", n), func(t *testing.T) {
			t.Parallel()
			const termHeight = 24
			sp := settleAllZero(fanoutViewportScreen(n, 100, termHeight))

			v := sp.View().Content
			if rows := fanoutScreenRows(v); rows > termHeight {
				t.Fatalf("settled screen renders %d rows on a %d-row terminal:\n%s", rows, termHeight, v)
			}
			if got := fanoutBoxDataRows(v); got != 5 {
				t.Fatalf("the window must render the budgeted 5 rows, got %d:\n%s", got, v)
			}
			if !strings.Contains(v, "Провайдер 00") {
				t.Fatalf("the review must start at the roster head:\n%s", v)
			}
			tail := fmt.Sprintf("Провайдер %02d", n-1)
			if strings.Contains(v, tail) {
				t.Fatalf("the tail row must wait behind the window:\n%s", v)
			}
			if !strings.Contains(v, "▼ ещё "+fmt.Sprint(n-5)) {
				t.Fatalf("the below-window indicator must count the hidden rows:\n%s", v)
			}
		})
	}
}

// TestSearchFanoutTailFollowLive pins the live window: while providers
// are still in flight the window shows the TAIL of the table — settled
// rows relocate above it and the in-flight rows stay visible — while
// the counter below keeps tracking the responses. The above-indicator
// counts exactly the rows the window left hidden.
func TestSearchFanoutTailFollowLive(t *testing.T) {
	t.Parallel()

	sp := fanoutViewportScreen(30, 100, 24)
	sp = settleSearch(sp,
		providerResultMsg{provider: sp.rows[0]},
		providerResultMsg{provider: sp.rows[1], err: errors.New("boom")},
		providerResultMsg{provider: sp.rows[2]},
	)

	v := sp.View().Content
	if rows := fanoutScreenRows(v); rows > 24 {
		t.Fatalf("live screen renders %d rows on a 24-row terminal:\n%s", rows, v)
	}
	rendered := fanoutBoxDataRows(v)
	if rendered < 1 {
		t.Fatalf("the live window must render at least one row:\n%s", v)
	}
	if !strings.Contains(v, "Провайдер 29") {
		t.Fatalf("the last in-flight row must stay visible (tail-follow):\n%s", v)
	}
	if strings.Contains(v, "Провайдер 00") || strings.Contains(v, "Провайдер 02") {
		t.Fatalf("settled rows must relocate above the live window:\n%s", v)
	}
	// The above-indicator counts exactly the rows the window left
	// above it; a tail-following window has nothing below it.
	if !strings.Contains(v, "▲ ещё "+fmt.Sprint(30-rendered)) {
		t.Fatalf("the above-window indicator must count the hidden rows (rendered %d):\n%s", rendered, v)
	}
	if strings.Contains(v, "▼ ещё") {
		t.Fatalf("a tail-following window has nothing below it:\n%s", v)
	}
	if !strings.Contains(v, "Ответившие: 2/30 провайдеров") {
		t.Fatalf("the counter must stay visible during the live fan-out:\n%s", v)
	}
}

// TestSearchFanoutSettleScrollReview pins the settled review state:
// the window resets to the roster head, Shift+↑/↓ scroll the full
// table (clamped at both ends, no cursor highlight — the table is
// read-only), and plain arrows stay owned by the results checklist.
// Row-visibility checks scope to the table BOX: the results checklist
// below repeats provider names in its own labels.
func TestSearchFanoutSettleScrollReview(t *testing.T) {
	t.Parallel()

	sp := settleAllResults(fanoutViewportScreen(30, 100, 40))

	v := sp.View().Content
	// The settled window opens at the roster head; its size is the
	// terminal budget's (asserted via the bounding contract, not a
	// hardcoded row count).
	boxText := strings.Join(tableBox(v), "\n")
	w := fanoutBoxDataRows(v)
	if w < 1 {
		t.Fatalf("the settled window must render at least one row:\n%s", v)
	}
	if !strings.Contains(boxText, "Провайдер 00") {
		t.Fatalf("the settled window must reset to the roster head:\n%s", v)
	}
	if strings.Contains(boxText, fmt.Sprintf("Провайдер %02d", w)) {
		t.Fatalf("rows beyond the %d-row window must not render:\n%s", w, v)
	}

	scroll := func(key tea.KeyPressMsg, times int) {
		t.Helper()
		for range times {
			next, _ := sp.Update(key)
			sp = next.(*searchProgress)
		}
	}

	// Past the bottom clamp: the tail row shows, the head hides, and
	// the indicator flips sides.
	scroll(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}, 40)
	v = sp.View().Content
	boxText = strings.Join(tableBox(v), "\n")
	if !strings.Contains(boxText, "Провайдер 29") {
		t.Fatalf("Shift+Down must scroll to the tail row:\n%s", v)
	}
	if strings.Contains(boxText, "Провайдер 00") {
		t.Fatalf("the head row must scroll out of view at the bottom clamp:\n%s", v)
	}
	if !strings.Contains(v, "▲ ещё "+fmt.Sprint(30-w)) || strings.Contains(v, "▼ ещё") {
		t.Fatalf("at the bottom clamp only the above-indicator shows (%d rendered):\n%s", w, v)
	}

	// Two rows back up: the window slides, nothing snaps.
	scroll(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}, 2)
	v = sp.View().Content
	boxText = strings.Join(tableBox(v), "\n")
	if !strings.Contains(boxText, "Провайдер 27") || strings.Contains(boxText, "Провайдер 28") {
		t.Fatalf("Shift+Up must slide the window two rows up:\n%s", v)
	}

	// The table is read-only: no cursor marker inside the box, and a
	// plain Down leaves the box untouched (the checklist owns it).
	for _, line := range tableBox(sp.View().Content) {
		if strings.Contains(line, "▸") {
			t.Fatalf("the review table must not render a cursor:\n%s", sp.View().Content)
		}
	}
	before := strings.Join(tableBox(sp.View().Content), "\n")
	next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	sp = next.(*searchProgress)
	if after := strings.Join(tableBox(sp.View().Content), "\n"); after != before {
		t.Fatalf("a plain Down belongs to the checklist, not the table:\n%s", sp.View().Content)
	}
}

// TestSearchFanoutResizeRebind pins the resize contract: the window
// re-binds to the new height in both directions, and a terminal tall
// enough for the whole table drops the indicators entirely.
func TestSearchFanoutResizeRebind(t *testing.T) {
	t.Parallel()

	sp := settleAllZero(fanoutViewportScreen(10, 100, 40))

	v := sp.View().Content
	if got := fanoutBoxDataRows(v); got != 10 {
		t.Fatalf("a 40-row terminal fits the whole table, got %d rows:\n%s", got, v)
	}
	if strings.Contains(v, "▲ ещё") || strings.Contains(v, "▼ ещё") {
		t.Fatalf("an unclipped table renders no indicators:\n%s", v)
	}

	next, _ := sp.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	sp = next.(*searchProgress)
	v = sp.View().Content
	if rows := fanoutScreenRows(v); rows > 16 {
		t.Fatalf("shrinking to 16 rows must re-bind the window, got %d rows:\n%s", rows, v)
	}
	if got := fanoutBoxDataRows(v); got != 1 {
		t.Fatalf("the degenerate 16-row terminal keeps one table row, got %d:\n%s", got, v)
	}
	if !strings.Contains(v, "▼ ещё 9") {
		t.Fatalf("the clipped table must carry the below-indicator:\n%s", v)
	}

	next, _ = sp.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	sp = next.(*searchProgress)
	v = sp.View().Content
	if got := fanoutBoxDataRows(v); got != 10 {
		t.Fatalf("growing back must restore the full table, got %d rows:\n%s", got, v)
	}
	if strings.Contains(v, "▲ ещё") || strings.Contains(v, "▼ ещё") {
		t.Fatalf("the indicators must disappear once everything fits:\n%s", v)
	}
}

// TestSearchFanoutChecklistReachableBelow pins the PR62 composition:
// with the checklist below the table window, the whole screen —
// counter, checklist title, its key hints included — still fits the
// tracked terminal height.
func TestSearchFanoutChecklistReachableBelow(t *testing.T) {
	t.Parallel()

	sp := settleAllResults(fanoutViewportScreen(30, 100, 36))

	v := sp.View().Content
	if rows := fanoutScreenRows(v); rows > 36 {
		t.Fatalf("settled screen with checklist renders %d rows on a 36-row terminal:\n%s", rows, v)
	}
	for _, want := range []string{
		"Ответившие: 30/30 провайдеров",
		"Выберите провайдеры:",
		"space — отметить",
		"esc — назад",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("the screen must keep %q reachable, got:\n%s", want, v)
		}
	}
}
