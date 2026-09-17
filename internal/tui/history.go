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
)

// History flow screen ids.
const (
	historyFilterID = "history-filter"
	historyListID   = "history-list"
	historyRebindID = "history-rebind"
)

// historyStatusChoices pairs storage status keys with RU labels plus
// per-status counts for the filter screen (python RUSSIAN_STATUSES).
func historyStatusChoices(items []storage.AnimeProgress) []Choice {
	counts := make(map[string]int)
	for _, it := range items {
		counts[it.ShikimoriStatus]++
	}
	choices := make([]Choice, 0, len(ruStatuses)+1)
	for _, st := range ruStatuses {
		choices = append(choices, Choice{
			ID:    st.Key,
			Label: st.Label + " [" + strconv.Itoa(counts[st.Key]) + "]",
			Value: st.Key,
		})
	}
	choices = append(choices, Choice{ID: "all", Label: "Все [" + strconv.Itoa(len(items)) + "]", Value: ""})
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
// binding itself stays silent — see historyFilter.Update).
const historyFilterHint = "s — проверить обновления списков"

// historyRefreshMsg settles one background library refresh (key «s»
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
	// refreshing dedups «s» while a check is in flight. It is cleared
	// by the refresh COMMAND itself — not by the message handler — so
	// a settled result dropped while the user navigated elsewhere
	// cannot wedge the key; the atomic keeps the flag race-clean
	// between the command goroutine and the update loop.
	refreshing atomic.Bool
}

// NewHistoryFilter builds the status filter screen — always the FIRST
// step of «📜 Списки» (python history_menu), with Back (I1), the
// empty state when history is empty (I3) and the «s» silent
// background list refresh (PR39).
func NewHistoryFilter(deps *Deps) Screen { return newHistoryFilter(deps) }

// newHistoryFilter builds the wrapper; the exported constructor hides
// the concrete type (the newHistoryList/NewHistoryList pattern).
func newHistoryFilter(deps *Deps) *historyFilter {
	items, err := loadHistory(deps)
	if err != nil {
		items = nil
	}
	h := &historyFilter{deps: deps, items: items}
	h.MenuScreen = NewMenuScreen(h.config(historyFilterHint))
	return h
}

// config renders the wrapped menu config for the current snapshot;
// status is the bottom hint (or error) line.
func (h *historyFilter) config(status string) MenuScreenConfig {
	emptyMsg := ""
	if len(h.items) == 0 {
		emptyMsg = "История пуста"
	}
	return MenuScreenConfig{
		ID:       historyFilterID,
		Title:    "Фильтр списка:",
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

// Update implements Screen: «s» dispatches the silent background
// refresh (a check already running makes it a no-op — no second
// fetch, no UI hint); the settled message applies the verdict;
// everything else is the wrapped menu.
func (h *historyFilter) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case historyRefreshMsg:
		h.applyRefresh(m)
		return h, nil
	case tea.KeyPressMsg:
		if m.Code == 's' {
			if !h.refreshing.CompareAndSwap(false, true) {
				return h, nil // a check is already running: silent no-op
			}
			return h, safeCmd(historyFilterID, h.refreshCmd())
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
// the screen untouched (zero visual noise); changed data re-renders
// the counts («досчитались») with the cursor preserved.
func (h *historyFilter) applyRefresh(m historyRefreshMsg) {
	if m.err != nil {
		h.deps.logger().Error("tui: history refresh failed",
			"screen", historyFilterID, "error", m.err)
		h.swap(h.items, "⚠ Не удалось обновить списки: "+m.err.Error())
		return
	}
	if reflect.DeepEqual(h.items, m.items) {
		return
	}
	h.swap(m.items, historyFilterHint)
}

// swap rebuilds the wrapped menu screen for a new snapshot + status
// line, preserving the cursor position.
func (h *historyFilter) swap(items []storage.AnimeProgress, status string) {
	cursor := h.list.Cursor()
	h.items = items
	h.MenuScreen = NewMenuScreen(h.config(status))
	h.list.Jump(cursor)
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
	for _, st := range ruStatuses {
		if st.Key == it.ShikimoriStatus {
			first := []rune(st.Label)[:1]
			return "[" + strings.ToUpper(string(first)) + "]"
		}
	}
	return "[?]"
}

// statusLabelRU renders the RU label of a status key.
func statusLabelRU(key string) string {
	for _, st := range ruStatuses {
		if st.Key == key {
			return st.Label
		}
	}
	return key
}

// newHistoryList builds the filtered history list (constructor is
// unexported: the filter screen owns the items snapshot).
func newHistoryList(deps *Deps, status string, all []storage.AnimeProgress) *MenuScreen {
	filtered := FilterHistory(all, status)
	emptyMsg := ""
	if len(filtered) == 0 {
		emptyMsg = "Список пуст"
	}
	choices := make([]Choice, 0, len(filtered))
	for i := range filtered {
		it := filtered[i]
		ep := it.CurrentEpisode
		if it.TotalEpisodes > 0 {
			ep += "/" + strconv.Itoa(it.TotalEpisodes)
		}
		label := historyBadge(it) + " " + it.Title + " (Серия " + ep + ")"
		choices = append(choices, Choice{
			ID:    strconv.Itoa(int(it.ID)),
			Label: label,
			Value: &filtered[i],
		})
	}
	return NewMenuScreen(MenuScreenConfig{
		ID:       historyListID,
		Title:    "Список — " + statusLabelRU(status),
		EmptyMsg: emptyMsg,
		Choices:  choices,
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			rec, ok := pick.(*storage.AnimeProgress)
			if !ok {
				return pop()
			}
			// PR30: every record already carries its Shikimori
			// binding — the pick goes straight to the provider
			// fan-out, no actions menu and no rebind prompt.
			return push(newRebindProgress(deps, rec))
		},
	})
}

// NewHistoryList is the exported constructor used by tests and by
// flows holding their own items snapshot.
func NewHistoryList(deps *Deps, status string, filtered []storage.AnimeProgress) *MenuScreen {
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
	sp.titleOverride = "Поиск по провайдерам: " + query
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
	var groups [][]contracts.SearchResult
	for _, res := range results {
		placed := false
		for gi, g := range groups {
			if providers.SimilarityRatio(
				strings.ToLower(res.Title),
				strings.ToLower(g[0].Title)) > threshold {
				groups[gi] = append(groups[gi], res)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []contracts.SearchResult{res})
		}
	}
	return groups
}
