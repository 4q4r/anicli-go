package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR78 type-to-search: the BIG content lists (provider-results
// checklist, session episode list, offline download file list) filter
// live as you type — any printable keystroke opens the search line
// between the header and the list and narrows it case-insensitively.
// Backspace edits, Esc clears, the second Esc is the normal Back;
// Enter picks from the FILTERED list (the real item, never a copy);
// the screen's single-letter hotkeys are never silently shadowed —
// they keep their meaning and do not trigger the filter.

func escKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEsc} }

func typeRunes(s Screen, text string) Screen {
	for _, r := range text {
		next, _ := s.Update(tea.KeyPressMsg{Code: r})
		s = next
	}
	return s
}

// sixtyFourResults builds the owner's screenshot-sized merged set.
func sixtyFourResults() []contracts.SearchResult {
	results := make([]contracts.SearchResult, 0, 64)
	for i := range 64 {
		results = append(results, contracts.SearchResult{
			Title:    fmt.Sprintf("Тайтл %02d", i),
			URL:      fmt.Sprintf("u%02d", i),
			SourceID: "animego",
		})
	}
	return results
}

// TestChecklistTypeToFilter drives the settled fan-out checklist
// (fixture-level: provider settlements, no network) through a live
// filter session.
func TestChecklistTypeToFilter(t *testing.T) {
	fs, deps := checklistTestDeps()
	sp := NewSearchProgress(deps, "запрос")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: sixtyFourResults()},
		providerResultMsg{provider: fs.providers[1], results: nil})

	// Typing "07" opens the line and narrows the list to one row.
	sp = typeRunes(sp, "07").(*searchProgress)
	view := sp.View().Content
	if !strings.Contains(view, "Поиск: 07") {
		t.Fatalf("the search line must render with the query, got:\n%s", view)
	}
	if !strings.Contains(view, "Тайтл 07") {
		t.Fatalf("the matching row must stay visible:\n%s", view)
	}
	if strings.Contains(view, "Тайтл 63") {
		t.Fatalf("non-matching rows must disappear while filtering:\n%s", view)
	}
	lines := strings.Count(view, "\n")
	if lines > defaultListHeight+20 {
		t.Fatalf("filtered view rendered %d lines, want a bounded viewport", lines)
	}
	// The paste for the report: what the owner sees after "07".
	t.Logf("filtered view (%d lines):\n%s", lines, view)

	// Esc clears the filter first (unfiltered view, no pop)...
	next, cmd := sp.Update(escKey())
	sp = next.(*searchProgress)
	if cmd != nil {
		t.Fatalf("the first Esc must clear the filter, not pop")
	}
	view = sp.View().Content
	if strings.Contains(view, "Поиск:") {
		t.Fatalf("the search line must disappear after Esc:\n%s", view)
	}
	if !strings.Contains(view, "Тайтл 00") {
		t.Fatalf("clearing must restore the unfiltered top of the list:\n%s", view)
	}

	// ...the second Esc is the normal Back.
	_, cmd = sp.Update(escKey())
	if msg := firstMsg(cmd); msg != (popMsg{}) {
		t.Fatalf("the second Esc must pop, got %#v", msg)
	}
}

// TestChecklistNonMatchingFilter: input with no matches degrades
// gracefully — an empty list inside a bounded view, movement is a
// safe no-op.
func TestChecklistNonMatchingFilter(t *testing.T) {
	fs, deps := checklistTestDeps()
	sp := NewSearchProgress(deps, "запрос")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: sixtyFourResults()},
		providerResultMsg{provider: fs.providers[1], results: nil})

	sp = typeRunes(sp, "йцукен").(*searchProgress)
	view := sp.View().Content
	if !strings.Contains(view, "Поиск: йцукен") {
		t.Fatalf("the query must render, got:\n%s", view)
	}
	if strings.Contains(view, "Тайтл 0") {
		t.Fatalf("no rows must match a junk query:\n%s", view)
	}
	if lines := strings.Count(view, "\n"); lines > defaultListHeight+20 {
		t.Fatalf("empty-result view rendered %d lines", lines)
	}
	next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	next, _ = next.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	next, cmd := next.Update(escKey())
	sp = next.(*searchProgress)
	if cmd != nil || sp.resultCheck.filterActive() {
		t.Fatalf("the first Esc must clear the junk filter in place")
	}
	_, cmd = sp.Update(escKey())
	if firstMsg(cmd) != (popMsg{}) {
		t.Fatalf("the second Esc must pop")
	}
}

// TestChecklistEnterPicksFilteredRealItem: Enter resolves the checked
// row as the REAL underlying item, not a copy.
func TestChecklistEnterPicksFilteredRealItem(t *testing.T) {
	items := make([]Choice, 0, 5)
	for i := range 5 {
		items = append(items, Choice{
			ID:    fmt.Sprintf("i%d", i),
			Label: fmt.Sprintf("Строка %d", i),
			Value: fmt.Sprintf("v%d", i),
		})
	}
	c := NewCheckList("Т", items)
	for _, r := range "3" {
		c.HandleKey(tea.KeyPressMsg{Code: r})
	}
	if !c.filterActive() {
		t.Fatalf("typing must engage the filter")
	}
	c.Toggle()
	checked := c.CheckedItems()
	if len(checked) != 1 {
		t.Fatalf("exactly one item must be checked, got %d", len(checked))
	}
	if got, ok := checked[0].Value.(string); !ok || got != "v3" {
		t.Fatalf("the picked item must be the real filtered item (v3), got %#v", checked[0].Value)
	}
	if got := c.Resolve(enter()); len(got.([]Choice)) != 1 {
		t.Fatalf("Resolve(enter) must carry the checked real item, got %#v", got)
	}
}

// TestChecklistFilterWrapCompose: the PR78 cursor wrap operates within
// the FILTERED subset, and toggles mark the REAL item.
func TestChecklistFilterWrapCompose(t *testing.T) {
	items := make([]Choice, 0, 20)
	for i := range 20 {
		items = append(items, Choice{
			ID:    fmt.Sprintf("i%02d", i),
			Label: fmt.Sprintf("Строка %02d", i),
			Value: i,
		})
	}
	c := NewCheckList("Т", items)
	for _, r := range "1" {
		c.HandleKey(tea.KeyPressMsg{Code: r})
	}
	// "1" matches Строка 1 and 10..19 → 11 visible rows.
	if got := c.viewLen(); got != 11 {
		t.Fatalf("filter '1' must leave 11 rows, got %d", got)
	}
	// Wrap down through the whole filtered subset lands back on row 0.
	for range 11 {
		c.MoveDown()
	}
	if c.list.Cursor() != 0 {
		t.Fatalf("wrap must stay within the filtered subset, got cursor %d", c.list.Cursor())
	}
	// Wrap up from the first visible row lands on the last filtered row
	// and toggling marks the REAL item (Строка 19, full index 19).
	c.MoveUp()
	if c.list.Cursor() != 10 {
		t.Fatalf("up from the first filtered row must wrap to the last, got %d", c.list.Cursor())
	}
	c.Toggle()
	if !c.Checked(19) {
		t.Fatalf("the toggle must mark the real item at full index 19")
	}
	// Clearing restores the unfiltered view.
	c.HandleKey(escKey())
	if c.filterActive() || c.viewLen() != 20 {
		t.Fatalf("Esc must clear the filter and restore all rows")
	}
}

// TestChecklistHotkeysUnshadowed: at rest the bound letters keep
// their hotkey meaning and do NOT start a filter.
func TestChecklistHotkeysUnshadowed(t *testing.T) {
	items := make([]Choice, 0, 3)
	for i := range 3 {
		items = append(items, Choice{ID: fmt.Sprintf("i%d", i), Label: fmt.Sprintf("Строка %d", i)})
	}
	c := NewCheckList("Т", items)
	c.HandleKey(tea.KeyPressMsg{Code: 'a'})
	if got := len(c.CheckedItems()); got != 3 {
		t.Fatalf("'a' must select all, got %d checked", got)
	}
	if c.filterActive() {
		t.Fatalf("'a' must not start the filter (bound hotkey)")
	}
	view := c.Render()
	if strings.Contains(view, "Поиск:") {
		t.Fatalf("no search line may appear from a bound hotkey:\n%s", view)
	}
}

// TestEpisodeListTypeToFilter: the session episode list filters live
// and Enter picks the real episode.
func TestEpisodeListTypeToFilter(t *testing.T) {
	s := newSessionForTests(t)
	idx := sessionActionIndex(s, "jump")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateEpisodeList {
		t.Fatalf("jump must open the episode list, got %v", ss.state)
	}

	// 'j' is a movement hotkey: it moves the cursor and must NOT open
	// the filter.
	next, _ = ss.Update(tea.KeyPressMsg{Code: 'j'})
	ss = next.(*sessionScreen)
	if ss.episodeFilter.active() {
		t.Fatalf("'j' is a bound movement key, it must not start the filter")
	}
	if strings.Contains(ss.View().Content, "Поиск:") {
		t.Fatalf("no search line from a bound movement key")
	}

	// Typing "3" narrows to episode 3; Enter picks the real episode.
	ss = typeRunes(ss, "3").(*sessionScreen)
	view := ss.View().Content
	if !strings.Contains(view, "Поиск: 3") {
		t.Fatalf("the search line must render:\n%s", view)
	}
	if !strings.Contains(view, "Серия 3") || strings.Contains(view, "Серия 1\n") {
		t.Fatalf("the list must be narrowed to the match:\n%s", view)
	}
	// Esc inside the filter clears first; a second Esc leaves the
	// list (back to the menu — the episode list's Back, no pop).
	ss = typeRunes(ss, "2").(*sessionScreen)
	next, cmd := ss.Update(escKey())
	ss = next.(*sessionScreen)
	if cmd != nil || ss.episodeFilter.active() {
		t.Fatalf("the first Esc must clear the filter in place")
	}
	if !strings.Contains(ss.View().Content, "Серия 1") {
		t.Fatalf("clearing must restore the unfiltered list")
	}
	next, cmd = ss.Update(escKey())
	ss = next.(*sessionScreen)
	if cmd != nil {
		t.Fatalf("the second Esc must go back to the menu without popping the screen")
	}
	if ss.state != sessionStateMenu {
		t.Fatalf("the second Esc must return to the menu, got %v", ss.state)
	}

	// Re-open and pick from a filtered list: Enter resolves the REAL
	// episode the filter left visible.
	idx = sessionActionIndex(ss, "jump")
	ss.list.Jump(idx)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss = typeRunes(ss, "3").(*sessionScreen)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.currentEpisode() != "3" {
		t.Fatalf("enter must pick the filtered real episode, got %q", ss.currentEpisode())
	}
	if ss.state != sessionStateMenu {
		t.Fatalf("after the pick the menu returns, got %v", ss.state)
	}
}

// TestOfflineListTypeToFilter: the download file list filters and
// picks the real local episode.
func TestOfflineListTypeToFilter(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}, Playback: &fakePlayback{}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])

	s = typeRunes(s, "2").(*offlineSession)
	view := s.View().Content
	if !strings.Contains(view, "Поиск: 2") {
		t.Fatalf("the search line must render:\n%s", view)
	}
	if !strings.Contains(view, "Эп. 2") {
		t.Fatalf("the matching episode must stay visible:\n%s", view)
	}
	if strings.Contains(view, "Эп. 1\n") {
		t.Fatalf("non-matching episodes must disappear:\n%s", view)
	}

	// Esc clears first (in place); the second Esc pops the session
	// (the picker's Back).
	next, cmd := s.Update(escKey())
	s = next.(*offlineSession)
	if cmd != nil || s.episodeFilter.active() {
		t.Fatalf("the first Esc must clear the filter in place")
	}
	if !strings.Contains(s.View().Content, "Эп. 1") {
		t.Fatalf("clearing must restore the unfiltered list")
	}
	_, cmd = s.Update(escKey())
	if firstMsg(cmd) != (popMsg{}) {
		t.Fatalf("the second Esc must pop the offline session")
	}
}

// TestOfflineEnterPicksFilteredRealEpisode: Enter on a filtered picker
// resolves the real local episode.
func TestOfflineEnterPicksFilteredRealEpisode(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}, Playback: &fakePlayback{}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])

	s = typeRunes(s, "2").(*offlineSession)
	next, _ := s.Update(enter())
	ss := next.(*offlineSession)
	if ss.current != "2" {
		t.Fatalf("enter must pick the filtered real episode, got %q", ss.current)
	}
}
