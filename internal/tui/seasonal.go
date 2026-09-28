package tui

// Seasonal calendar screen (PR114): the current season's airing anime
// grouped by broadcast weekday, with ←/→ season browsing, the PR78
// type-to-search filter and Enter falling into the normal catalog
// episode flow (the same searchProgress the «Списки» rebind uses).
//
// Data flows through Deps.Seasonal — production wires the
// Shikimori-first adapter with the MAL fallback (seasonal_real.go);
// tests inject fakes.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// seasonalScreenID is the screen identity; headerIDPrefix marks the
// day-group header rows (PR41 B2 disabled rows).
const (
	seasonalScreenID = "seasonal"
	headerIDPrefix   = "hdr:"
)

// seasonOrder is the calendar order of the season names.
var seasonOrder = []string{"winter", "spring", "summer", "fall"}

// weekdayOrder is the Monday-first rendering order of the day groups.
var weekdayOrder = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

// seasonalLoadedMsg settles one season fetch. year+season stale-guard
// the message: a flip to another season while the fetch was in flight
// makes the late settle a no-op.
type seasonalLoadedMsg struct {
	year   int
	season string
	rows   []SeasonalRow
	err    error
}

// seasonOf maps a date onto its calendar-quarter season (Jan-Mar
// winter, Apr-Jun spring, Jul-Sep summer, Oct-Dec fall — the task's
// detection table).
func seasonOf(t time.Time) (int, string) {
	switch t.Month() {
	case time.January, time.February, time.March:
		return t.Year(), seasonOrder[0]
	case time.April, time.May, time.June:
		return t.Year(), seasonOrder[1]
	case time.July, time.August, time.September:
		return t.Year(), seasonOrder[2]
	default:
		return t.Year(), seasonOrder[3]
	}
}

// shiftSeason walks delta seasons forward (or backward), wrapping the
// year at the winter boundary.
func shiftSeason(year int, season string, delta int) (int, string) {
	idx := 0
	for i, s := range seasonOrder {
		if s == season {
			idx = i
			break
		}
	}
	total := idx + delta
	next := ((total % len(seasonOrder)) + len(seasonOrder)) % len(seasonOrder)
	return year + (total-next)/len(seasonOrder), seasonOrder[next]
}

// weekdayKey is the stable i18n suffix of a weekday.
func weekdayKey(wd time.Weekday) string {
	return strings.ToLower(wd.String())
}

// weekdayLabel resolves a day group's localized name (full literal
// keys — the i18n source scanner greps them).
func weekdayLabel(wd time.Weekday) string {
	switch wd {
	case time.Monday:
		return i18n.T("seasonal.day_monday")
	case time.Tuesday:
		return i18n.T("seasonal.day_tuesday")
	case time.Wednesday:
		return i18n.T("seasonal.day_wednesday")
	case time.Thursday:
		return i18n.T("seasonal.day_thursday")
	case time.Friday:
		return i18n.T("seasonal.day_friday")
	case time.Saturday:
		return i18n.T("seasonal.day_saturday")
	default:
		return i18n.T("seasonal.day_sunday")
	}
}

// seasonLabel resolves a season's localized header («Осень 2026»).
func seasonLabel(year int, season string) string {
	vals := i18n.Vals{"year": strconv.Itoa(year)}
	switch season {
	case "winter":
		return i18n.T("seasonal.season_winter", vals)
	case "spring":
		return i18n.T("seasonal.season_spring", vals)
	case "summer":
		return i18n.T("seasonal.season_summer", vals)
	default:
		return i18n.T("seasonal.season_fall", vals)
	}
}

// seasonalScreen is the calendar surface.
type seasonalScreen struct {
	deps *Deps
	// now is the injected clock (tests pin it; season detection and
	// the "current season" check read it).
	now     time.Time
	year    int
	season  string
	loading bool
	loadErr error
	rows    []SeasonalRow
	filter  listFilter
	list    *PinList
	// status scopes the transient verdict line (PR41 B2 header
	// picks) — see surfaceStatus.
	surfaceStatus
}

// NewSeasonalScreen builds the calendar on the current season.
//
//nolint:revive // internal screen type
func NewSeasonalScreen(deps *Deps) *seasonalScreen {
	return newSeasonalScreenAt(deps, time.Now())
}

// newSeasonalScreenAt is the clock-injected constructor (tests).
//
//nolint:revive // internal screen constructor
func newSeasonalScreenAt(deps *Deps, now time.Time) *seasonalScreen {
	year, season := seasonOf(now)
	s := &seasonalScreen{deps: deps, now: now, year: year, season: season, loading: true}
	s.buildList()
	return s
}

// ID implements Screen.
func (s *seasonalScreen) ID() string { return seasonalScreenID }

// Init implements Screen: the season fetch starts immediately.
func (s *seasonalScreen) Init() tea.Cmd { return s.fetchCmd() }

// isCurrentSeason reports whether the viewed season is "now" — the
// one place the ongoing-only filter applies (browsed seasons keep
// their released/anounced titles).
func (s *seasonalScreen) isCurrentSeason() bool {
	year, season := seasonOf(s.now)
	return year == s.year && season == s.season
}

// fetchCmd schedules one Seasonal fetch under the standard lookup
// budget. Commands own their timeout contexts — see the App.ctx
// divergence note.
func (s *seasonalScreen) fetchCmd() tea.Cmd {
	deps, year, season, ongoing := s.deps, s.year, s.season, s.isCurrentSeason()
	return safeCmd(seasonalScreenID, func() tea.Msg {
		if deps == nil || deps.Seasonal == nil {
			return seasonalLoadedMsg{year: year, season: season,
				err: errors.New(i18n.T("seasonal.unavailable"))}
		}
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		rows, err := deps.Seasonal.Season(ctx, year, season, ongoing)
		return seasonalLoadedMsg{year: year, season: season, rows: rows, err: err}
	})
}

// seasonLabel renders the localized season header («Осень 2026»).
func (s *seasonalScreen) seasonLabel() string {
	return seasonLabel(s.year, s.season)
}

// Update implements Screen: fetch settles (stale-guarded) and key
// routing.
func (s *seasonalScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case seasonalLoadedMsg:
		if m.year != s.year || m.season != s.season {
			return s, nil // stale settle — the user already flipped
		}
		s.loading = false
		s.loadErr = m.err
		s.rows = m.rows
		s.filter.clear()
		s.buildList()
		return s, nil
	case tea.KeyPressMsg:
		return s.handleKey(m)
	default:
		return s, nil
	}
}

// handleKey routes the calendar's keys: ←/→ browse seasons, the PR78
// filter narrows the list live, Esc clears the filter first and pops
// second, Enter resolves rows into the catalog flow and headers into
// a status answer (PR41 B2).
func (s *seasonalScreen) handleKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch key.Code {
	case tea.KeyLeft, tea.KeyRight:
		delta := 1
		if key.Code == tea.KeyLeft {
			delta = -1
		}
		s.year, s.season = shiftSeason(s.year, s.season, delta)
		s.loading = true
		s.loadErr = nil
		s.rows = nil
		s.filter.clear()
		s.buildList()
		return s, s.fetchCmd()
	}

	if IsCancelKey(key) {
		if !s.loading && s.filter.active() {
			s.filter.clear()
			s.buildList()
			return s, nil
		}
		return s, pop()
	}

	if s.loading {
		return s, nil // nothing to browse yet
	}

	if consumed, changed := s.filter.consume(key, pinListBoundRunes); consumed {
		if changed {
			s.buildList()
		}
		return s, nil
	}
	if s.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		return s, pop()
	}
	if id, ok := resolved.(string); ok && strings.HasPrefix(id, headerIDPrefix) {
		s.setStatus(i18n.T("seasonal.header_hint"))
		return s, nil
	}
	row, ok := resolved.(*SeasonalRow)
	if !ok {
		return s, nil
	}
	return s, push(NewSearchProgress(s.deps, row.Title))
}

// seasonDayGroup is one weekday bucket of the calendar.
type seasonDayGroup struct {
	key   string
	label string
	rows  []SeasonalRow
}

// groups buckets the loaded rows into Monday-first weekday groups
// with the unscheduled tail. Wire order is preserved inside a group
// (popularity rank from the source).
func (s *seasonalScreen) groups() []seasonDayGroup {
	byDay := make(map[time.Weekday][]SeasonalRow, len(weekdayOrder))
	var unscheduled []SeasonalRow
	for _, r := range s.rows {
		if r.HasWeekday {
			byDay[r.Weekday] = append(byDay[r.Weekday], r)
		} else {
			unscheduled = append(unscheduled, r)
		}
	}
	var out []seasonDayGroup
	for _, wd := range weekdayOrder {
		if rows := byDay[wd]; len(rows) > 0 {
			out = append(out, seasonDayGroup{key: weekdayKey(wd), label: weekdayLabel(wd), rows: rows})
		}
	}
	if len(unscheduled) > 0 {
		out = append(out, seasonDayGroup{key: "other", label: i18n.T("seasonal.day_other"), rows: unscheduled})
	}
	return out
}

// buildList renders the grouped list, applying the live filter PER
// GROUP so a day header stays exactly when one of its rows still
// matches.
func (s *seasonalScreen) buildList() {
	prev := cursorID(s.list)
	emptyMsg := ""
	if s.loadErr == nil {
		emptyMsg = i18n.T("seasonal.empty")
	}
	var choices []Choice
	for _, g := range s.groups() {
		rows := filterSeasonalRows(g.rows, s.filter.value())
		if len(rows) == 0 {
			continue
		}
		choices = append(choices, Choice{
			ID:       headerIDPrefix + g.key,
			Label:    "— " + g.label + " —",
			Disabled: true,
		})
		for i := range rows {
			choices = append(choices, Choice{
				ID:    "row:" + rows[i].ID,
				Label: seasonalRowLabel(rows[i]),
				Value: &rows[i],
			})
		}
	}
	s.list = NewPinList(NewMenu(i18n.T("seasonal.title"), emptyMsg, choices...), defaultListHeight)
	restoreCursor(s.list, prev)
}

// filterSeasonalRows keeps the rows whose title contains the query
// case-insensitively (the filterChoices semantics over SeasonalRow).
func filterSeasonalRows(rows []SeasonalRow, query string) []SeasonalRow {
	if strings.TrimSpace(query) == "" {
		return rows
	}
	q := strings.ToLower(query)
	out := make([]SeasonalRow, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Title), q) {
			out = append(out, r)
		}
	}
	return out
}

// seasonalRowLabel renders one calendar row: title, the aired/total
// episode counter (total 0 renders "?") and the score when scored.
func seasonalRowLabel(r SeasonalRow) string {
	total := strconv.Itoa(r.Episodes)
	if r.Episodes == 0 {
		total = "?"
	}
	eps := strconv.Itoa(r.EpisodesAired) + "/" + total
	if r.Score != "" {
		return i18n.T("seasonal.row", i18n.Vals{"title": r.Title, "eps": eps, "score": r.Score})
	}
	return i18n.T("seasonal.row_noscore", i18n.Vals{"title": r.Title, "eps": eps})
}

// View implements Screen.
func (s *seasonalScreen) View() tea.View {
	header := theme.Title.Render(s.seasonLabel()) + "\n\n"
	var body string
	switch {
	case s.loading:
		body = header + theme.Dim.Render(
			i18n.T("seasonal.loading", i18n.Vals{"title": s.seasonLabel()}))
	case s.loadErr != nil:
		body = header + theme.Error.Render(
			i18n.T("seasonal.error", i18n.Vals{"err": s.loadErr.Error()})) + "\n\n" +
			s.list.Render()
	default:
		body = header + filterLineAbove(s.filter, s.list.Render())
	}
	if s.statusVisible() {
		body += "\n" + theme.StatusLine.Render(s.status)
	} else {
		body += "\n" + theme.StatusLine.Render(i18n.T("seasonal.hint"))
	}
	return tea.NewView(body)
}
