package tui

// PR83: the library screens get the PR78 type-to-search machinery and
// the sync hotkey moves from «s» to Ctrl+R (r = refresh) — «s» is
// freed so typing can start with any letter. The PR39 sync contract
// (silent background refresh, in-flight dedup, verdicts) is preserved
// — only the key changes.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// ctrlR presses the library refresh combo.
func ctrlR() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}
}

// typeOn feeds a string to the screen one printable rune per Update.
func typeOn(s Screen, text string) {
	for _, r := range text {
		_, _ = s.Update(tea.KeyPressMsg{Code: r})
	}
}

// TestHistoryFilterOldSKeyNoLongerSyncs (PR83): «s» is freed for
// typing — pressing it starts the filter and must NOT dispatch the
// refresh.
func TestHistoryFilterOldSKeyNoLongerSyncs(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	next, cmd := filter.Update(tea.KeyPressMsg{Code: 's'})
	f := next.(*historyFilter)

	if cmd != nil {
		t.Fatal("pressing s must not start the refresh anymore")
	}
	if sync.calls != 0 {
		t.Fatalf("the sync must not run on s, ran %d times", sync.calls)
	}
	if !f.filter.active() || f.filter.value() != "s" {
		t.Fatalf("s must arm the type-to-search query, got %q (active=%v)",
			f.filter.value(), f.filter.active())
	}
}

// TestHistoryFilterCtrlRWithArmedFilterKeepsQuery: Ctrl+R works while
// a type-to-search query is armed — the refresh must not clear the
// query and the combo must not be eaten by the filter.
func TestHistoryFilterCtrlRWithArmedFilterKeepsQuery(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)
	typeOn(filter, "all")
	f := filter
	if !f.filter.active() {
		t.Fatal("the query must be armed before Ctrl+R")
	}

	_, cmd := f.Update(ctrlR())
	if cmd == nil {
		t.Fatal("Ctrl+R must start the refresh with a filter armed")
	}
	if f.filter.value() != "all" {
		t.Fatalf("the refresh must not clear the query, got %q", f.filter.value())
	}
	if !strings.Contains(f.View().Content, "Search: all") {
		t.Errorf("the filtered view must survive the refresh, got:\n%s", f.View().Content)
	}
}

// TestHistoryFilterTypeToSearchLiveFilter: typing narrows the status
// choices live with the «Search: …» line between the title and the list.
func TestHistoryFilterTypeToSearchLiveFilter(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := newHistoryFilter(deps)

	if got := filter.View().Content; strings.Contains(got, "Поиск:") {
		t.Fatalf("no query — no search line, got:\n%s", got)
	}
	typeOn(filter, "all")

	f := filter
	items := f.list.Menu().Items
	if len(items) != 2 {
		t.Fatalf("items = %v, want the matched choice + the Back row", labelsOf(items))
	}
	if !strings.Contains(items[0].Label, "All [3]") {
		t.Errorf("label = %q, want the narrowed «Все» choice", items[0].Label)
	}
	if v := filter.View().Content; !strings.Contains(v, "Search: all") {
		t.Errorf("view missing the search line:\n%s", v)
	}
}

// TestHistoryFilterEscPhases: the first Esc clears the query, the
// second falls through to Back (the screen pops).
func TestHistoryFilterEscPhases(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := newHistoryFilter(deps)
	typeOn(filter, "all")
	f := filter
	if len(f.list.Menu().Items) != 2 {
		t.Fatalf("armed items = %v, want the matched choice + the Back row", labelsOf(f.list.Menu().Items))
	}

	next, _ := filter.Update(esc())
	f = next.(*historyFilter)
	if f.filter.active() {
		t.Fatal("the first Esc must clear the query")
	}
	if len(f.list.Menu().Items) < 3 {
		t.Fatalf("cleared view must restore all status choices, got %d", len(f.list.Menu().Items))
	}

	// The second Esc falls through: the screen pops off the nav stack
	// (the returned screen is no longer the library filter).
	next2, cmd2 := f.Update(esc())
	if _, still := next2.(*historyFilter); still && cmd2 == nil {
		t.Fatal("the second Esc must fall through to Back (pop)")
	}
}

// TestHistoryFilterEnterPicksFilteredItem: Enter on the narrowed
// choices opens the per-status list of the PICKED status.
func TestHistoryFilterEnterPicksFilteredItem(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := newHistoryFilter(deps)
	typeOn(filter, "Planned")
	f := filter

	if got := len(f.list.Menu().Items); got != 2 {
		t.Fatalf("choices = %d (%v), want the matched row + Back", got, labelsOf(f.list.Menu().Items))
	}
	// The status pick pushes the titles list via a command — the
	// filter screen remains until the runtime processes the push.
	scr, pushCmd := f.Update(enter())
	if _, still := scr.(*historyFilter); !still {
		t.Fatalf("scr = %T, want the filter screen still up (the push rides a command)", scr)
	}
	msg := runCmd(pushCmd)
	pushed, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("Enter cmd = %#v, want a pushMsg", msg)
	}
	nl, ok := pushed.screen.(*historyListScreen)
	if !ok {
		t.Fatalf("pushed screen = %T, want the library titles list", pushed.screen)
	}
	if nl.status != "planned" {
		t.Errorf("picked status = %q, want planned", nl.status)
	}
	if got := len(nl.list.Menu().Items); got != 2 {
		t.Errorf("titles = %d (%v), want the matched record + Back", got, labelsOf(nl.list.Menu().Items))
	}
}

// TestHistoryListTypeToSearch: the titles list narrows live with the
// «Search: …» line.
func TestHistoryListTypeToSearch(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := newHistoryList(deps, "", historyItems())
	typeOn(list, "нару")

	if got := len(list.list.Menu().Items); got != 2 {
		t.Fatalf("rows = %d (%v), want the matched record + Back", got, labelsOf(list.list.Menu().Items))
	}
	if v := list.View().Content; !strings.Contains(v, "Search: нару") {
		t.Errorf("view missing the search line:\n%s", v)
	}
}

// TestHistoryListEnterCarriesFilteredRecord: Enter on the filtered
// row resolves the record behind it (the real filtered item, not a
// stale pre-filter index).
func TestHistoryListEnterCarriesFilteredRecord(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := newHistoryList(deps, "", historyItems())
	typeOn(list, "нару")

	// The pick pushes the session/rebind flow for Наруто (id 2) — the
	// push rides a command, the list screen stays until the runtime
	// processes it.
	next, pushCmd := list.Update(enter())
	if _, still := next.(*historyListScreen); !still {
		t.Fatalf("Enter scr = %T, want the list still up (the push rides a command)", next)
	}
	if pushCmd == nil {
		t.Fatal("Enter must produce the push command")
	}
	msg := pushCmd()
	pushed, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("Enter cmd = %#v, want a pushMsg", msg)
	}
	// The carried record is the REAL filtered item — Наруто (id 2).
	resumed, ok := pushed.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("pushed screen = %T, want the resumed session", pushed.screen)
	}
	if got := resumed.primary.Title; got != "Наруто" {
		t.Errorf("carried record title = %q, want Наруто", got)
	}
	if resumed.primary.URL != "u2" {
		t.Errorf("carried record URL = %q, want u2", resumed.primary.URL)
	}
}

// TestHistoryListWrapInFilteredSubset: wrap-around composes with the
// filter — movement cycles inside the filtered subset. While the
// query is armed, j/k are QUERY EDITS (the PR78 consume design:
// armed filters take every printable), so movement rides the arrows
// and wraps across the titles + the pinned Back row.
func TestHistoryListWrapInFilteredSubset(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := newHistoryList(deps, "", historyItems())
	typeOn(list, "а") // Ванпанчмен + Наруто (cyrillic «а»)

	if got := len(list.list.Menu().Items); got != 3 {
		t.Fatalf("rows = %d, want the 2 titles + the Back row", got)
	}
	if list.list.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0", list.list.Cursor())
	}
	list.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if list.list.Cursor() != 1 {
		t.Fatalf("cursor after one down = %d, want 1", list.list.Cursor())
	}
	list.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if list.list.Cursor() != 2 {
		t.Fatalf("cursor after two downs = %d, want 2", list.list.Cursor())
	}
	list.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if list.list.Cursor() != 0 {
		t.Fatalf("cursor must wrap inside the filtered subset, got %d", list.list.Cursor())
	}
}

// labelsOf renders menu item labels for assertion messages.
func labelsOf(items []Choice) string {
	out := make([]string, 0, len(items))
	for _, c := range items {
		out = append(out, c.Label)
	}
	return "[" + strings.Join(out, " | ") + "]"
}

// TestHistoryFilterCursorStaysOnRecordWhileNarrowing (PR83 review):
// index-based cursor preservation parks the cursor on the wrong row
// (or «🔙 Back») when a narrowing drops earlier choices. The cursor
// must stay parked on THE SAME choice — the statuses carry stable IDs.
func TestHistoryFilterCursorStaysOnRecordWhileNarrowing(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := newHistoryFilter(deps)
	f := filter

	// Park the cursor on «Dropped» (the last status choice).
	for i, item := range f.list.Menu().Items {
		if item.ID == "dropped" {
			f.list.Jump(i)
			break
		}
	}
	if got := f.list.Menu().Items[f.list.Cursor()].ID; got != "dropped" {
		t.Fatalf("precondition: cursor on %q, want dropped", got)
	}

	typeOn(filter, "drop")
	got := f.list.Menu().Items[f.list.Cursor()]
	if !strings.Contains(got.Label, "Dropped") {
		t.Fatalf("cursor parked on %q after narrowing, want «Dropped»", got.Label)
	}
}

// TestHistoryListCursorStaysOnRecordWhileNarrowing: the titles list —
// cursor on the LAST record, a narrowing that keeps it must keep the
// cursor on the SAME record (index-based preservation shifts it).
func TestHistoryListCursorStaysOnRecordWhileNarrowing(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	l := newHistoryList(deps, "", historyItems())

	l.list.Jump(2) // Bleach — the last of the three records
	if id := cursorID(l.list); id != "3" {
		t.Fatalf("precondition: cursor id = %q, want the Bleach record (3)", id)
	}

	typeOn(l, "ble")
	if got := len(l.list.Menu().Items); got != 2 {
		t.Fatalf("rows = %d, want Bleach + Back", got)
	}
	if l.list.Cursor() != 0 || !strings.Contains(l.list.Menu().Items[0].Label, "Bleach") {
		t.Fatalf("cursor = %d, want it parked on the Bleach row after narrowing",
			l.list.Cursor())
	}
}

// runCmd executes a tea.Cmd and returns its message (nil-safe).
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
