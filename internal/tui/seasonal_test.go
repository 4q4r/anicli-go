package tui

// Seasonal calendar screen tests (PR114): season detection, day
// grouping, the PR78 type-to-search filter, season browsing arrows
// and the navigation into the normal catalog episode flow. The data
// source is a fake SeasonalService; the screen is clock-injected so
// season detection is deterministic.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fakeSeasonCall records one Season invocation.
type fakeSeasonCall struct {
	year        int
	season      string
	ongoingOnly bool
}

// fakeSeasonal is the deterministic SeasonalService double.
type fakeSeasonal struct {
	rows  []SeasonalRow
	err   error
	calls []fakeSeasonCall
}

// Season implements SeasonalService.
func (f *fakeSeasonal) Season(_ context.Context, year int, season string, ongoingOnly bool) ([]SeasonalRow, error) {
	f.calls = append(f.calls, fakeSeasonCall{year: year, season: season, ongoingOnly: ongoingOnly})
	return f.rows, f.err
}

// seasonalFixtureClock pins "now" inside fall 2026 (2026-10-05; the
// detection table maps Oct-Dec onto fall).
func seasonalFixtureClock() time.Time {
	return time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
}

// seasonalFixtureRows covers every interesting row shape: a Monday
// row with score, a Thursday row, and an unscheduled row without a
// score and an unknown episode total.
func seasonalFixtureRows() []SeasonalRow {
	return []SeasonalRow{
		{ID: "1", Title: "Witch Hat", EpisodesAired: 4, Episodes: 12, Score: "8.61", Weekday: time.Monday, HasWeekday: true},
		{ID: "2", Title: "Dandadan", EpisodesAired: 6, Episodes: 12, Score: "8.9", Weekday: time.Thursday, HasWeekday: true},
		{ID: "3", Title: "Movie Night", EpisodesAired: 0, Episodes: 0, Score: "", HasWeekday: false},
	}
}

// newSeasonalForTest builds the screen on the fixture clock and its
// fake service.
func newSeasonalForTest(rows []SeasonalRow, err error) (*seasonalScreen, *fakeSeasonal) {
	svc := &fakeSeasonal{rows: rows, err: err}
	s := newSeasonalScreenAt(&Deps{Seasonal: svc}, seasonalFixtureClock())
	return s, svc
}

// settleSeasonalInit drives the screen's initial fetch to completion.
func settleSeasonalInit(t *testing.T, s *seasonalScreen) *seasonalScreen {
	t.Helper()
	cmd := s.Init()
	if cmd == nil {
		t.Fatal("Init must schedule the season fetch")
	}
	msg := cmd()
	loaded, ok := msg.(seasonalLoadedMsg)
	if !ok {
		t.Fatalf("fetch command must settle a seasonalLoadedMsg, got %#v", msg)
	}
	next, _ := s.Update(loaded)
	seasonal, ok := next.(*seasonalScreen)
	if !ok {
		t.Fatalf("update must keep the seasonal screen, got %T", next)
	}
	return seasonal
}

// keyMsg builds one key press.
func keyMsg(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// TestSeasonOf: the calendar-quarter season detection (Jan-Mar winter,
// Apr-Jun spring, Jul-Sep summer, Oct-Dec fall).
func TestSeasonOf(t *testing.T) {
	t.Parallel()

	cases := []struct {
		when     time.Time
		wantYear int
		want     string
	}{
		{time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), 2026, "winter"},
		{time.Date(2026, time.March, 31, 23, 59, 0, 0, time.UTC), 2026, "winter"},
		{time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC), 2026, "spring"},
		{time.Date(2026, time.June, 30, 12, 0, 0, 0, time.UTC), 2026, "spring"},
		{time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC), 2026, "summer"},
		{time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC), 2026, "summer"},
		{time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC), 2026, "fall"},
		{time.Date(2026, time.December, 31, 23, 0, 0, 0, time.UTC), 2026, "fall"},
	}
	for _, tc := range cases {
		gotYear, got := seasonOf(tc.when)
		if gotYear != tc.wantYear || got != tc.want {
			t.Errorf("seasonOf(%s) = (%d, %s), want (%d, %s)",
				tc.when.Format("2006-01-02"), gotYear, got, tc.wantYear, tc.want)
		}
	}
}

// TestShiftSeason: the season arithmetic wraps the year at the
// winter/fall boundary in both directions.
func TestShiftSeason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		year, delta int
		season      string
		wantYear    int
		want        string
	}{
		{2026, 1, "fall", 2027, "winter"},
		{2027, -1, "winter", 2026, "fall"},
		{2026, 2, "summer", 2027, "winter"}, // summer -> fall -> next winter
		{2026, 4, "spring", 2027, "spring"},
		{2026, -1, "winter", 2025, "fall"},
		{2026, 0, "fall", 2026, "fall"},
	}
	for _, tc := range cases {
		gotYear, got := shiftSeason(tc.year, tc.season, tc.delta)
		if gotYear != tc.wantYear || got != tc.want {
			t.Errorf("shiftSeason(%d, %s, %+d) = (%d, %s), want (%d, %s)",
				tc.year, tc.season, tc.delta, gotYear, got, tc.wantYear, tc.want)
		}
	}
}

// TestSeasonalScreenRender: after the fetch settles, the view shows
// the season header, day groups in Monday-first order with the
// unscheduled tail, per-row episode counts and scores, and the pinned
// Back row.
func TestSeasonalScreenRender(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(seasonalFixtureRows(), nil)
	s = settleSeasonalInit(t, s)
	view := s.View().Content

	for _, want := range []string{
		"Fall 2026",
		"— Monday —",
		"Witch Hat",
		"[4/12]",
		"8.61",
		"— Thursday —",
		"Dandadan",
		"— Unscheduled —",
		"Movie Night",
		"[0/?]",
		"🔙 Back",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view must contain %q, got:\n%s", want, view)
		}
	}
	// Group order: Monday before Thursday before Unscheduled.
	monday := strings.Index(view, "— Monday —")
	thursday := strings.Index(view, "— Thursday —")
	other := strings.Index(view, "— Unscheduled —")
	if monday >= thursday || thursday >= other {
		t.Fatalf("groups must render Monday-first with the unscheduled tail:\n%s", view)
	}
}

// TestSeasonalScreenDayGrouping: rows group under their weekday header
// regardless of arrival order; multiple rows of one day share the
// header.
func TestSeasonalScreenDayGrouping(t *testing.T) {
	t.Parallel()

	rows := []SeasonalRow{
		{ID: "a", Title: "Sunday Show", Weekday: time.Sunday, HasWeekday: true, Episodes: 12},
		{ID: "b", Title: "Monday One", Weekday: time.Monday, HasWeekday: true, Episodes: 12},
		{ID: "c", Title: "Monday Two", Weekday: time.Monday, HasWeekday: true, Episodes: 24},
	}
	s, _ := newSeasonalForTest(rows, nil)
	s = settleSeasonalInit(t, s)
	view := s.View().Content

	monday := strings.Index(view, "Monday One")
	monday2 := strings.Index(view, "Monday Two")
	if monday < 0 || monday2 < 0 {
		t.Fatalf("both Monday rows must render:\n%s", view)
	}
	if abs(monday-monday2) > 40+len("Monday OneMonday Two") {
		t.Fatalf("same-day rows must sit adjacent under one header:\n%s", view)
	}
	if strings.Count(view, "— Monday —") != 1 {
		t.Fatalf("one Monday header for two rows:\n%s", view)
	}
	if strings.Index(view, "— Monday —") > strings.Index(view, "— Sunday —") {
		t.Fatalf("Monday renders before Sunday (Monday-first week):\n%s", view)
	}
}

// abs is the test-local integer distance.
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestSeasonalScreenFilter: typing narrows rows live (PR78), drops
// group headers whose rows all left, the first Esc clears and the
// second pops.
func TestSeasonalScreenFilter(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(seasonalFixtureRows(), nil)
	s = settleSeasonalInit(t, s)

	// Type "wit": only the Witch Hat row stays, with its Monday
	// header; the Thursday and Unscheduled groups vanish.
	for _, r := range "wit" {
		next, _ := s.Update(keyMsg(r))
		s = next.(*seasonalScreen)
	}
	view := s.View().Content
	if !strings.Contains(view, "Witch Hat") {
		t.Fatalf("matching row must survive the filter:\n%s", view)
	}
	if strings.Contains(view, "Dandadan") || strings.Contains(view, "Movie Night") {
		t.Fatalf("non-matching rows must be filtered out:\n%s", view)
	}
	if strings.Contains(view, "— Thursday —") || strings.Contains(view, "— Unscheduled —") {
		t.Fatalf("only the matching group's header may stay:\n%s", view)
	}

	// First Esc clears the filter.
	next, _ := s.Update(esc())
	s = next.(*seasonalScreen)
	if !strings.Contains(s.View().Content, "Dandadan") {
		t.Fatalf("Esc must restore the full list:\n%s", s.View().Content)
	}

	// Second Esc pops the screen.
	_, cmd := s.Update(esc())
	if cmd == nil {
		t.Fatal("the second Esc must pop")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("the second Esc must resolve to popMsg, got %#v", cmd())
	}
}

// TestSeasonalScreenEnterOpensCatalogFlow: Enter on a row pushes the
// normal searchProgress flow seeded with the row title.
func TestSeasonalScreenEnterOpensCatalogFlow(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(seasonalFixtureRows(), nil)
	s = settleSeasonalInit(t, s)

	// Items: [hdr Monday, Witch Hat, hdr Thursday, Dandadan, hdr
	// Unscheduled, Movie Night, Back] — jump onto the first row.
	s.list.Jump(1)
	_, cmd := s.Update(enter())
	if cmd == nil {
		t.Fatal("Enter on a row must schedule navigation")
	}
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("Enter must push a screen, got %#v", msg)
	}
	if pm.screen.ID() != searchProgressID {
		t.Fatalf("want the catalog search screen %q, got %q", searchProgressID, pm.screen.ID())
	}
}

// TestSeasonalScreenHeaderPickAnswers: Enter on a day header answers
// with a status note instead of navigating (PR41 B2: disabled rows
// never act silently).
func TestSeasonalScreenHeaderPickAnswers(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(seasonalFixtureRows(), nil)
	s = settleSeasonalInit(t, s)

	s.list.Jump(0) // the Monday header
	next, cmd := s.Update(enter())
	if cmd != nil {
		if pm, ok := cmd().(pushMsg); ok {
			t.Fatalf("header pick must not push, got %q", pm.screen.ID())
		}
	}
	seasonal := next.(*seasonalScreen)
	if !seasonal.statusVisible() || seasonal.status == "" {
		t.Fatalf("header pick must answer with a status line, got %q", seasonal.status)
	}
}

// TestSeasonalScreenSeasonBrowsing: the arrows shift the season and
// refetch (←/→), the ongoing filter applies only to the current
// season, and a stale settle is ignored.
func TestSeasonalScreenSeasonBrowsing(t *testing.T) {
	t.Parallel()

	s, svc := newSeasonalForTest(seasonalFixtureRows(), nil)
	s = settleSeasonalInit(t, s)
	if len(svc.calls) != 1 || svc.calls[0] != (fakeSeasonCall{2026, "fall", true}) {
		t.Fatalf("initial fetch = %+v, want (2026, fall, ongoing)", svc.calls)
	}

	// → : winter 2027, browsed (no ongoing filter), loading view.
	next, cmd := s.Update(keyMsg(tea.KeyRight))
	s = next.(*seasonalScreen)
	if s.year != 2027 || s.season != "winter" {
		t.Fatalf("right arrow must land on winter 2027, got %d %s", s.year, s.season)
	}
	if !s.loading {
		t.Fatal("season shift must enter the loading state")
	}
	if !strings.Contains(s.View().Content, "Loading") {
		t.Fatalf("loading view must render the loading line:\n%s", s.View().Content)
	}
	if cmd == nil {
		t.Fatal("season shift must schedule a refetch")
	}

	// A stale settle for the previous season never lands.
	stale := cmd().(seasonalLoadedMsg) // the refetch result for winter 2027
	if stale.year != 2027 {
		t.Fatalf("refetch must target winter 2027, got %d %s", stale.year, stale.season)
	}
	next, _ = s.Update(seasonalLoadedMsg{year: 2026, season: "fall", rows: seasonalFixtureRows()})
	s = next.(*seasonalScreen)
	if s.year != 2027 || strings.Contains(s.View().Content, "Fall 2026") {
		t.Fatalf("stale settle must be ignored:\n%s", s.View().Content)
	}

	// The in-flight winter settle lands.
	next, _ = s.Update(stale)
	s = next.(*seasonalScreen)
	if s.loading {
		t.Fatal("settle must leave the loading state")
	}
	if !strings.Contains(s.View().Content, "Winter 2027") {
		t.Fatalf("view must show the new season header:\n%s", s.View().Content)
	}
	if len(svc.calls) != 2 || svc.calls[1] != (fakeSeasonCall{2027, "winter", false}) {
		t.Fatalf("refetch = %+v, want (2027, winter, browsed)", svc.calls)
	}

	// ← back to fall 2026 (current again → ongoing filter restored).
	next, _ = s.Update(keyMsg(tea.KeyLeft))
	s = next.(*seasonalScreen)
	if s.year != 2026 || s.season != "fall" {
		t.Fatalf("left arrow must land on fall 2026, got %d %s", s.year, s.season)
	}
}

// TestSeasonalScreenEmpty: an empty season renders the I3 empty state
// with the pinned Back row.
func TestSeasonalScreenEmpty(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(nil, nil)
	s = settleSeasonalInit(t, s)
	view := s.View().Content
	if !strings.Contains(view, "No anime") {
		t.Fatalf("empty state must render:\n%s", view)
	}
	if !strings.Contains(view, "🔙 Back") {
		t.Fatalf("Back must stay pinned on the empty season:\n%s", view)
	}
}

// TestSeasonalScreenError: a failed fetch renders the error inline
// (Back keeps working).
func TestSeasonalScreenError(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(nil, errors.New("shiki down"))
	s = settleSeasonalInit(t, s)
	view := s.View().Content
	if !strings.Contains(view, "shiki down") {
		t.Fatalf("the fetch error must render inline:\n%s", view)
	}
	_, cmd := s.Update(esc())
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("Esc must still pop from the error state, got %#v", cmd)
	}
}

// TestSeasonalScreenNoService: without any data source the screen
// reports honest unavailability instead of pretending.
func TestSeasonalScreenNoService(t *testing.T) {
	t.Parallel()

	s := newSeasonalScreenAt(newTestDeps(), seasonalFixtureClock())
	s = settleSeasonalInit(t, s)
	view := s.View().Content
	if !strings.Contains(view, "unavailable") {
		t.Fatalf("the missing-service state must render:\n%s", view)
	}
}

// TestSeasonalScreenLoadingPop: Esc during the initial load pops —
// the fetch settle is stale-guarded, never a crash.
func TestSeasonalScreenLoadingPop(t *testing.T) {
	t.Parallel()

	s, _ := newSeasonalForTest(seasonalFixtureRows(), nil)
	cmd := s.Init()
	_ = cmd
	_, popCmd := s.Update(esc())
	if _, ok := popCmd().(popMsg); !ok {
		t.Fatalf("Esc during loading must pop, got %#v", popCmd)
	}
}

// TestRootSeasonEntry: the root menu carries the 📅 Season entry after
// Check and it pushes the seasonal screen.
func TestRootSeasonEntry(t *testing.T) {
	t.Parallel()

	root := NewRootScreen(newTestDeps())
	items := root.list.Menu().Items
	found := -1
	checkIdx := -1
	for i, c := range items {
		switch c.ID {
		case "season":
			found = i
		case "check":
			checkIdx = i
		}
	}
	if found < 0 {
		t.Fatalf("root menu must carry the season entry: %+v", items)
	}
	if found < checkIdx {
		t.Fatalf("season must render after Check (non-disruptive order): %+v", items)
	}
	if items[len(items)-1].ID != "exit" {
		t.Fatalf("exit must stay the pinned last item: %+v", items)
	}

	root.list.Jump(found)
	_, cmd := root.Update(enter())
	if cmd == nil {
		t.Fatal("season pick must schedule navigation")
	}
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("season pick must push a screen, got %#v", msg)
	}
	if pm.screen.ID() != seasonalScreenID {
		t.Fatalf("want seasonal screen %q, got %q", seasonalScreenID, pm.screen.ID())
	}
}
