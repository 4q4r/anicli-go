package tui

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/metadata"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// History flow screen ids.
const (
	historyFilterID = "history-filter"
	historyListID   = "history-list"
	historyRebindID = "history-rebind"
)

// historyStatusChoices pairs storage status keys with localized labels
// plus per-status counts for the filter screen (python
// RUSSIAN_STATUSES).
func historyStatusChoices(items []storage.AnimeProgress) []Choice {
	counts := make(map[string]int)
	for _, it := range items {
		counts[it.ShikimoriStatus]++
	}
	choices := make([]Choice, 0, len(shikiStatuses())+1)
	for _, st := range shikiStatuses() {
		choices = append(choices, Choice{
			ID:    st.Key,
			Label: st.Label + " [" + strconv.Itoa(counts[st.Key]) + "]",
			Value: st.Key,
		})
	}
	choices = append(choices, Choice{ID: "all", Label: i18n.T("history.all", i18n.Vals{"count": strconv.Itoa(len(items))}), Value: ""})
	return choices
}

// FilterHistory filters history rows by status key; "" means all.
func FilterHistory(items []storage.AnimeProgress, status string) []storage.AnimeProgress {
	out := make([]storage.AnimeProgress, 0, len(items))
	for _, it := range items {
		if status == "" || it.ShikimoriStatus == status {
			out = append(out, it)
		}
	}
	return out
}

// historyFilterHint is the static key hint on the library screen (the
// binding itself stays silent — see historyFilter.Update; PR110: the
// const became a function for the same init-order reason as BackLabel).
func historyFilterHint() string { return i18n.T("history.refresh_hint") }

// historyRefreshMsg settles one background library refresh (Ctrl+R
// on the history filter screen): the reloaded snapshot, or the error
// that must surface on the status line.
type historyRefreshMsg struct {
	items []storage.AnimeProgress
	err   error
}

// historyFilter is the library screen («📜 Списки» → status filter,
// the per-status counts surface). The PR39 background refresh rides
// ON TOP of the generic menu: the wrapper owns the items snapshot and
// the refresh lifecycle, the embedded MenuScreen keeps the §5
// rendering and navigation unchanged (the rebindProgress embedding
// pattern).
type historyFilter struct {
	*MenuScreen
	deps  *Deps
	items []storage.AnimeProgress
	// filter is the PR83 type-to-search over the status choices (the
	// sync hotkey moved to Ctrl+R, freeing the letter for typing).
	filter listFilter
	// status mirrors the wrapped screen's bottom line (the hint, or a
	// refresh error): applyRefresh needs it to notice that a fresh
	// success must supersede a prior failure.
	status string
	// refreshing dedups Ctrl+R while a check is in flight. It is cleared
	// by the refresh COMMAND itself — not by the message handler — so
	// a settled result dropped while the user navigated elsewhere
	// cannot wedge the key; the atomic keeps the flag race-clean
	// between the command goroutine and the update loop.
	refreshing atomic.Bool
}

// NewHistoryFilter builds the status filter screen — always the FIRST
// step of «📜 Списки» (python history_menu), with Back (I1), the
// empty state when history is empty (I3) and the Ctrl+R silent
// background list refresh (PR39, re-keyed in PR83).
func NewHistoryFilter(deps *Deps) Screen { return newHistoryFilter(deps) }

// newHistoryFilter builds the wrapper; the exported constructor hides
// the concrete type (the newHistoryList/NewHistoryList pattern).
func newHistoryFilter(deps *Deps) *historyFilter {
	items, err := loadHistory(deps)
	if err != nil {
		items = nil
	}
	h := &historyFilter{deps: deps, items: items, status: historyFilterHint()}
	h.render(historyFilterHint())
	return h
}

// render rebuilds the wrapped menu for the current snapshot, query
// and status line. Cursor preservation is ID-based (cursorID/
// restoreCursor — the buildEpisodeList precedent): the cursor stays
// parked on the same choice when a narrowing keeps it.
func (h *historyFilter) render(status string) {
	var prev string
	if h.MenuScreen != nil {
		prev = cursorID(h.list)
	}
	cfg := h.config(status)
	cfg.Choices = filterChoices(cfg.Choices, h.filter.value())
	h.MenuScreen = NewMenuScreen(cfg)
	restoreCursor(h.list, prev)
}

// rebuild re-renders for a changed type-to-search query.
func (h *historyFilter) rebuild() { h.render(h.status) }

// config renders the wrapped menu config for the current snapshot;
// status is the bottom hint (or error) line.
func (h *historyFilter) config(status string) MenuScreenConfig {
	emptyMsg := ""
	if len(h.items) == 0 {
		emptyMsg = i18n.T("history.empty")
	}
	return MenuScreenConfig{
		ID:       historyFilterID,
		Title:    i18n.T("history.filter_title"),
		EmptyMsg: emptyMsg,
		Choices:  historyStatusChoices(h.items),
		Status:   status,
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			key, _ := pick.(string)
			return push(newHistoryList(h.deps, key, h.items))
		},
	}
}

// Update implements Screen: Ctrl+R dispatches the silent background
// refresh (a check already running makes it a no-op — no second
// fetch, no UI hint); the settled message applies the verdict;
// everything else is the wrapped menu.
func (h *historyFilter) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case historyRefreshMsg:
		h.applyRefresh(m)
		return h, nil
	case tea.KeyPressMsg:
		// PR83: the refresh combo is Ctrl+R («s» is freed for typing).
		// Modifier combos never enter the type-to-search query, so the
		// refresh works with a filter armed.
		if m.Code == 'r' && m.Mod == tea.ModCtrl {
			if !h.refreshing.CompareAndSwap(false, true) {
				return h, nil // a check is already running: silent no-op
			}
			return h, safeCmd(historyFilterID, h.refreshCmd())
		}
		// PR83 type-to-search: printable keys narrow the choices live;
		// the first Esc clears, the second falls through to Back.
		if consumed, changed := h.filter.consume(m, pinListBoundRunes); consumed {
			if changed {
				h.rebuild()
			}
			return h, nil
		}
	}
	next, cmd := h.MenuScreen.Update(msg)
	if next == Screen(h.MenuScreen) {
		return h, cmd
	}
	return next, cmd
}

// refreshCmd runs ONE background check pass: the two-way sync (the
// startup-sync seam reused as-is — one pass covers every list at
// once), then a fresh history reload. The verdict rides back as a
// single message; the in-flight flag clears HERE, in the command
// goroutine, so the re-arm never depends on the message reaching this
// screen. The command owns its timeout context (see the App.ctx note)
// and SyncFull's progress callback stays nil: no progress surfaces.
func (h *historyFilter) refreshCmd() tea.Cmd {
	deps := h.deps
	return func() tea.Msg {
		defer h.refreshing.Store(false)
		if deps == nil || deps.SyncFull == nil {
			return historyRefreshMsg{err: errSyncUnavailable}
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncBudget)
		defer cancel()
		if _, err := deps.SyncFull(ctx, nil); err != nil {
			return historyRefreshMsg{err: err}
		}
		items, err := loadHistory(deps)
		if err != nil {
			return historyRefreshMsg{err: err}
		}
		return historyRefreshMsg{items: items}
	}
}

// applyRefresh applies one settled check: an error lands on the status
// line (fail loud) and keeps the rendered data; identical data keeps
// the screen untouched (zero visual noise) unless an error was
// showing — a fresh success supersedes a stale failure; changed data
// re-renders the counts («досчитались») with the cursor preserved.
func (h *historyFilter) applyRefresh(m historyRefreshMsg) {
	if m.err != nil {
		h.deps.logger().Error("tui: history refresh failed",
			"screen", historyFilterID, "error", m.err)
		h.swap(h.items, i18n.T("history.refresh_failed", i18n.Vals{"err": m.err.Error()}))
		return
	}
	if reflect.DeepEqual(h.items, m.items) {
		// Identical data: zero visual noise — EXCEPT that a fresh
		// success supersedes a prior failure (the stale error line
		// must not outlive the check that disproved it).
		if h.status != historyFilterHint() {
			h.swap(h.items, historyFilterHint())
		}
		return
	}
	h.swap(m.items, historyFilterHint())
}

// swap rebuilds the wrapped menu screen for a new snapshot + status
// line, preserving the cursor position.
func (h *historyFilter) swap(items []storage.AnimeProgress, status string) {
	h.items = items
	h.status = status
	h.render(status)
}

// loadHistory loads the full history once for the whole flow.
func loadHistory(deps *Deps) ([]storage.AnimeProgress, error) {
	if deps == nil || deps.History == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	return deps.History.List(ctx)
}

// historyBadge renders the row prefix: [⚠] for needs-correction,
// else the status initial (python prefix logic).
func historyBadge(it storage.AnimeProgress) string {
	if it.NeedsCorrection {
		return "[⚠]"
	}
	for _, st := range shikiStatuses() {
		if st.Key == it.ShikimoriStatus {
			first := []rune(st.Label)[:1]
			return "[" + strings.ToUpper(string(first)) + "]"
		}
	}
	return "[?]"
}

// statusLabel renders the localized label of a status key.
func statusLabel(key string) string {
	for _, st := range shikiStatuses() {
		if st.Key == key {
			return st.Label
		}
	}
	return key
}

// View composes the «Поиск: …» line above the list (the PR78 shape)
// and keeps the status line (the refresh hint/verdict) below.
func (h *historyFilter) View() tea.View {
	body := theme.Title.Render(h.title) + "\n\n" +
		filterLineAbove(h.filter, h.list.Render())
	if h.status != "" {
		body += "\n" + theme.StatusLine.Render(h.status)
	}
	return tea.NewView(body)
}

// historyListScreen is the library titles list with the PR83
// type-to-search (the rebindProgress embedding pattern: the wrapper
// owns the query state and rebuild, the embedded MenuScreen keeps the
// rendering and pick routing).
type historyListScreen struct {
	*MenuScreen
	deps   *Deps
	status string
	items  []storage.AnimeProgress
	filter listFilter
}

// rebuild re-renders the list for the current status snapshot and
// type-to-search query. Cursor preservation is ID-based (cursorID/
// restoreCursor — the buildEpisodeList precedent): the cursor stays
// parked on the same record when a narrowing keeps it.
func (l *historyListScreen) rebuild() {
	prev := cursorID(l.list)
	l.MenuScreen = l.build()
	restoreCursor(l.list, prev)
}

// build assembles the wrapped menu for the current state. The rows
// are the FilterHistory(status) subset narrowed by the live query;
// Value keeps the REAL record pointer.
func (l *historyListScreen) build() *MenuScreen {
	filtered := FilterHistory(l.items, l.status)
	choices := make([]Choice, 0, len(filtered))
	for i := range filtered {
		it := filtered[i]
		ep := it.CurrentEpisode
		if it.TotalEpisodes > 0 {
			ep += "/" + strconv.Itoa(it.TotalEpisodes)
		}
		label := historyBadge(it) + " " + it.Title + " " + i18n.T("history.episode_suffix", i18n.Vals{"ep": ep})
		choices = append(choices, Choice{
			ID:    strconv.Itoa(int(it.ID)),
			Label: label,
			Value: &filtered[i],
		})
	}
	choices = filterChoices(choices, l.filter.value())
	emptyMsg := ""
	if len(choices) == 0 {
		emptyMsg = i18n.T("history.list_empty")
	}
	return NewMenuScreen(MenuScreenConfig{
		ID:       historyListID,
		Title:    i18n.T("history.list_title", i18n.Vals{"status": statusLabel(l.status)}),
		EmptyMsg: emptyMsg,
		Choices:  choices,
		OnPick:   l.onPick,
	})
}

// onPick routes the picked record (unchanged PR62 semantics).
func (l *historyListScreen) onPick(pick any) tea.Cmd {
	if pick == Back {
		return pop()
	}
	rec, ok := pick.(*storage.AnimeProgress)
	if !ok {
		return pop()
	}
	if rec.NeedsCorrection || rec.SourceID == "" || rec.SourceURL == "" {
		// Placeholder (PR30): the record carries no usable
		// source — the provider fan-out binds it (PR62 #2
		// persists the pick).
		return push(newRebindProgress(l.deps, rec))
	}
	// PR62 #3: bound records skip the search — the fan-out ran
	// ONCE when the binding was made; re-entry resumes the
	// stored source directly (python's saved-single-source
	// resume), «🔗 Перепривязать» re-runs the fan-out.
	primary := contracts.SearchResult{
		Title:    derefStr(rec.BoundTitle, rec.Title),
		URL:      rec.SourceURL,
		SourceID: rec.SourceID,
	}
	if rec.Poster != nil {
		primary.Poster = *rec.Poster
	}
	return push(newResumedSession(l.deps, primary, []contracts.SearchResult{primary}, *rec))
}

// Update: PR83 type-to-search (the first Esc clears, the second pops)
// over the embedded menu.
func (l *historyListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if key, isKey := msg.(tea.KeyPressMsg); isKey {
		if consumed, changed := l.filter.consume(key, pinListBoundRunes); consumed {
			if changed {
				l.rebuild()
			}
			return l, nil
		}
	}
	next, cmd := l.MenuScreen.Update(msg)
	if next == Screen(l.MenuScreen) {
		return l, cmd
	}
	return next, cmd
}

// View composes the «Поиск: …» line above the list (the PR78 shape).
func (l *historyListScreen) View() tea.View {
	return tea.NewView(theme.Title.Render(l.title) + "\n\n" +
		filterLineAbove(l.filter, l.list.Render()))
}

// newHistoryList builds the filtered history list (constructor is
// unexported: the filter screen owns the items snapshot).
func newHistoryList(deps *Deps, status string, all []storage.AnimeProgress) *historyListScreen {
	l := &historyListScreen{deps: deps, status: status, items: all}
	l.MenuScreen = l.build()
	return l
}

// newHistoryListFromFiltered builds the titles list from PRE-FILTERED
// rows (the tests' shape); the wrapper's rebuild keeps the status
// pass-through (FilterHistory on already-matching rows is a no-op).
func newHistoryListFromFiltered(deps *Deps, status string, filtered []storage.AnimeProgress) *historyListScreen {
	return newHistoryList(deps, status, filtered)
}

// derefStr falls back when the pointer is nil.
func derefStr(p *string, fallback string) string {
	if p != nil && *p != "" {
		return *p
	}
	return fallback
}

// rebindProgress is the CATALOG search screen (PR30): picking an
// anime from «Списки» lands here directly — the record is already
// bound to Shikimori, so there is no rebind prompt and no «Привязать?»
// confirmation. The record's canonical Shikimori title (Title as
// fallback) seeds the query variants plus the metadata alternative
// names; the embedded searchProgress renders the live provider table
// and, once every row settled, the grouped results BELOW the table.
// A pick resumes the session (I6) — see searchProgress.Update.
type rebindProgress struct {
	*searchProgress
	rec *storage.AnimeProgress
}

func newRebindProgress(deps *Deps, rec *storage.AnimeProgress) *rebindProgress {
	query := derefStr(rec.ShikimoriTitle, rec.Title)
	sp := NewSearchProgress(deps, query)
	sp.logTag = "rebind"
	sp.titleOverride = i18n.T("search.providers_query", i18n.Vals{"query": query})
	// The record rides along as the resume payload: a group pick
	// enters the session restored to the saved episode and dubs.
	sp.resume = rec
	return &rebindProgress{searchProgress: sp, rec: rec}
}

// ID implements Screen.
func (r *rebindProgress) ID() string { return historyRebindID + "-search" }

// Init implements Screen: log the start, then enrich the variants from
// the record's canonical Shikimori title when the record is bound
// (canonical + metadata aliases — the Shikimori-first flow shape);
// unbound records fan out on the bare query (the record title).
// Commands own their timeout contexts — see the App.ctx note.
func (r *rebindProgress) Init() tea.Cmd {
	if r.deps != nil && r.deps.Log != nil {
		r.deps.Log.Info("rebind: starting",
			"query", r.query, "providers", len(r.rows), "record", r.rec.ID)
	}
	if canonical := derefStr(r.rec.ShikimoriTitle, ""); canonical != "" {
		r.enriching = true
		return tea.Batch(r.spin.Tick, safeCmd(r.ID(), func() tea.Msg {
			return resolveRebindVariants(r.deps, canonical, r.query)
		}))
	}
	return r.startFanOut()
}

// resolveRebindVariants seeds the rebind variant set from the record's
// canonical title plus its metadata alternative names (PR29): the same
// shape as resolveSearchVariants with the record playing the Shikimori
// match. Errors degrade to the bare set — enrichment is a bonus.
func resolveRebindVariants(deps *Deps, canonical, query string) searchVariantsMsg {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	aliases := []string{canonical}
	if deps != nil && deps.Metadata != nil {
		if more, err := deps.Metadata.SearchAlternativeTitles(ctx, canonical); err == nil {
			aliases = append(aliases, more...)
		}
	}
	return searchVariantsMsg{variants: metadata.QueryVariants(query, aliases)}
}

// Update implements Screen: the whole surface — spinner ticks,
// variant settling, provider rows, the settled below-table results and
// the group pick — lives on the embedded searchProgress; this
// override only keeps the wrapper identity on the stack (the
// shikiFanOutScreen pattern).
func (r *rebindProgress) Update(msg tea.Msg) (Screen, tea.Cmd) {
	next, cmd := r.searchProgress.Update(msg)
	if next == Screen(r.searchProgress) {
		return r, cmd
	}
	return next, cmd
}

// RehydrateGroup applies the two-strategy match over similarity
// groups (python _rehydrate_group): exact (source_id, url) first,
// then best-similar against the canonical title.
func RehydrateGroup(groups [][]contracts.SearchResult, rec storage.AnimeProgress) []contracts.SearchResult {
	if got := FindExactGroup(groups, rec.SourceID, rec.SourceURL); got != nil {
		return got
	}
	canonical := derefStr(rec.ShikimoriTitle, rec.Title)
	target := derefStr(rec.BoundTitle, rec.Title)
	// Prefer matching the bound title when present, canonical as a
	// secondary attempt (python compares against the shikimori title
	// but stores bound similarity for this purpose).
	if got := FindBestSimilarGroup(groups, target, 0.6); got != nil {
		return got
	}
	return FindBestSimilarGroup(groups, canonical, 0.6)
}

// GroupByTitle clusters results by pairwise title similarity above
// threshold (a thin deterministic grouper standing in for the Python
// SemanticGrouper in the rebind flow; the primary search flow uses
// the manual checkbox grouping per the TUI spec).
func GroupByTitle(results []contracts.SearchResult, threshold float64) [][]contracts.SearchResult {
	// Threshold ≥ 1 can never be exceeded (ratio ≤ 1.0): everything
	// isolates. (The original loop proved the same by never matching.)
	// !(t < 1.0) — not t >= 1.0 — so a NaN threshold isolates too
	// instead of falling into the pairwise path where only exact
	// matches would group (PR82 final round, hardening).
	if !(threshold < 1.0) {
		groups := make([][]contracts.SearchResult, 0, len(results))
		for _, res := range results {
			groups = append(groups, []contracts.SearchResult{res})
		}
		return groups
	}

	// Constant-factor cuts over the original pairwise loop, semantics
	// identical (PR82 P1#2): the head's lowered title is computed once
	// per group (was: once per comparison); each result's lowered title
	// once per result; an exact lowered match short-circuits (ratio is
	// exactly 1.0); and the SequenceMatcher length bound
	// ratio ≤ 2·min(|a|,|b|)/(|a|+|b|) skips pairs that mathematically
	// cannot exceed the threshold. Group membership and intra-group
	// ordering are unchanged — the first-grouped-head-wins scan order
	// and the append order are the original ones.
	type group struct {
		headLower string
		items     []contracts.SearchResult
	}
	var groups []group
	for _, res := range results {
		resLower := strings.ToLower(res.Title)
		placed := false
		for gi := range groups {
			// Exact lowered match: the ratio is exactly 1.0, and the
			// threshold < 1.0 early return guarantees it groups — no
			// SequenceMatcher run needed.
			if groups[gi].headLower == resLower {
				groups[gi].items = append(groups[gi].items, res)
				placed = true
				break
			}
			// Length bound: ratio ≤ 2·min/(|a|+|b|); pairs under the
			// bound mathematically cannot exceed the threshold.
			if lowerBoundAllows(groups[gi].headLower, resLower, threshold) &&
				providers.SimilarityRatio(resLower, groups[gi].headLower) > threshold {
				groups[gi].items = append(groups[gi].items, res)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, group{headLower: resLower, items: []contracts.SearchResult{res}})
		}
	}
	out := make([][]contracts.SearchResult, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.items)
	}
	return out
}

// lowerBoundAllows reports whether the SequenceMatcher ratio of a and b
// CAN exceed threshold: ratio = 2M/(|a|+|b|) with M ≤ min(|a|,|b|) gives
// the tight length bound. Exact-match pairs are handled by the caller.
func lowerBoundAllows(a, b string, threshold float64) bool {
	la, lb := len([]rune(a)), len([]rune(b))
	if la == 0 && lb == 0 {
		return true // ratio 1.0
	}
	return 2*float64(min(la, lb))/float64(la+lb) > threshold
}
