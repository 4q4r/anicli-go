package tui

import (
	"context"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/metadata"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"
)

// History flow screen ids.
const (
	historyFilterID  = "history-filter"
	historyListID    = "history-list"
	historyActionsID = "history-actions"
	historyRebindID  = "history-rebind"
	historyResumeID  = "history-resume"
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

// NewHistoryFilter builds the status filter screen — always the FIRST
// step of «📜 Списки» (python history_menu), with Back (I1) and the
// empty state when history is empty (I3).
func NewHistoryFilter(deps *Deps) *MenuScreen {
	items, err := loadHistory(deps)
	if err != nil {
		items = nil
	}
	emptyMsg := ""
	if len(items) == 0 {
		emptyMsg = "История пуста"
	}
	return NewMenuScreen(MenuScreenConfig{
		ID:       historyFilterID,
		Title:    "Фильтр списка:",
		EmptyMsg: emptyMsg,
		Choices:  historyStatusChoices(items),
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			status, _ := pick.(string)
			return push(newHistoryList(deps, status, items))
		},
	})
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
			if rec.NeedsCorrection {
				return push(newHistoryRebind(deps, rec))
			}
			return push(newHistoryActions(deps, rec))
		},
	})
}

// NewHistoryList is the exported constructor used by tests and by
// flows holding their own items snapshot.
func NewHistoryList(deps *Deps, status string, filtered []storage.AnimeProgress) *MenuScreen {
	return newHistoryList(deps, status, filtered)
}

// newHistoryActions offers resume / rebind on one record.
func newHistoryActions(deps *Deps, rec *storage.AnimeProgress) *MenuScreen {
	return NewMenuScreen(MenuScreenConfig{
		ID:    historyActionsID,
		Title: rec.Title + " — действия",
		Choices: []Choice{
			{ID: "resume", Label: "▶ Продолжить просмотр"},
			{ID: "rebind", Label: "🔗 Перепривязать источник"},
		},
		Status: "Эпизод: " + rec.CurrentEpisode,
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			switch pick {
			case "resume":
				return push(newHistoryResume(deps, rec))
			case "rebind":
				return push(newHistoryRebind(deps, rec))
			default:
				return nil
			}
		},
	})
}

// newHistoryRebind runs the binding search flow for a
// needs-correction or rebind-requested record.
func newHistoryRebind(deps *Deps, rec *storage.AnimeProgress) *TextPrompt {
	return NewTextPrompt(TextPromptConfig{
		ID:          historyRebindID,
		Title:       "🔎 Поиск источника:",
		Initial:     derefStr(rec.BoundTitle, rec.Title),
		Placeholder: "название аниме…",
		OnSubmit: func(resolved any) tea.Cmd {
			query, ok := resolved.(string)
			if !ok {
				return pop()
			}
			return replace(newRebindProgress(deps, rec, query))
		},
	})
}

// derefStr falls back when the pointer is nil.
func derefStr(p *string, fallback string) string {
	if p != nil && *p != "" {
		return *p
	}
	return fallback
}

// rebindGroupsMsg settles the rebind fan-out.
type rebindGroupsMsg struct {
	groups [][]contracts.SearchResult
}

// rebindProgress reuses the searchProgress live table for the lists
// binding search (PR29): the record's canonical Shikimori title seeds
// the query variants, the table renders per-provider statuses plus the
// centered overall counter, and enter — once every row settled —
// groups the results for the binding pick (python search_and_bind).
type rebindProgress struct {
	*searchProgress
	rec  *storage.AnimeProgress
	list *PinList
}

func newRebindProgress(deps *Deps, rec *storage.AnimeProgress, query string) *rebindProgress {
	sp := NewSearchProgress(deps, query)
	sp.logTag = "rebind"
	return &rebindProgress{searchProgress: sp, rec: rec}
}

// ID implements Screen.
func (r *rebindProgress) ID() string { return historyRebindID + "-search" }

// Init implements Screen: log the start, then enrich the variants from
// the record's canonical Shikimori title when the record is bound
// (canonical + metadata aliases — the Shikimori-first flow shape);
// unbound records fan out on the bare query. Commands own their
// timeout contexts — see the App.ctx note.
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

// Update implements Screen: the table machinery (spinner ticks,
// variant settling, provider rows) delegates to the embedded
// searchProgress; enter, the grouped pick and the binding stay
// rebind-specific.
func (r *rebindProgress) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case rebindGroupsMsg:
		r.buildGroupList(m.groups)
		return r, nil
	case tea.KeyPressMsg:
		if IsCancelKey(m) {
			return r, pop()
		}
		if r.list != nil {
			if r.list.HandleKey(m) {
				return r, nil
			}
			resolved := ResolveKey(r.list.Menu(), r.list.Cursor(), m)
			if resolved == nil {
				return r, nil
			}
			if resolved == Back {
				return r, pop()
			}
			group, ok := resolved.([]contracts.SearchResult)
			if !ok {
				return r, nil
			}
			if err := r.bind(group); err != nil {
				return r, func() tea.Msg {
					return errMsg{screen: r.ID(), err: err}
				}
			}
			return r, popToRoot()
		}
		if m.Code != tea.KeyEnter {
			return r, nil
		}
		if len(r.pending) > 0 {
			return r, nil
		}
		groups := GroupByTitle(r.results, 0.6)
		return r, func() tea.Msg { return rebindGroupsMsg{groups: groups} }
	default:
		next, cmd := r.searchProgress.Update(msg)
		if next == Screen(r.searchProgress) {
			return r, cmd
		}
		return next, cmd
	}
}

// buildGroupList renders the grouped candidates.
func (r *rebindProgress) buildGroupList(groups [][]contracts.SearchResult) {
	choices := make([]Choice, 0, len(groups))
	for i, g := range groups {
		sources := make([]string, 0, len(g))
		for _, res := range g {
			sources = append(sources, res.SourceID)
		}
		choices = append(choices, Choice{
			ID:    "g" + strconv.Itoa(i),
			Label: BestDisplayTitle(g) + " (" + strconv.Itoa(len(sources)) + " ист.) [" + strings.Join(dedupe(sources), ", ") + "]",
			Value: g,
		})
	}
	r.list = NewPinList(NewMenu("Выберите правильный тайтл для привязки:", "Ничего не найдено", choices...), defaultListHeight)
}

// bind persists the chosen binding (python record patch).
func (r *rebindProgress) bind(group []contracts.SearchResult) error {
	if len(group) == 0 {
		return nil
	}
	primary := group[0]
	if r.deps == nil || r.deps.History == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	return r.deps.History.BindSource(ctx, r.rec.ID, primary.SourceID, primary.URL)
}

// View implements Screen: the live provider table from the embedded
// searchProgress (PR29) until the grouped pick list takes over.
func (r *rebindProgress) View() tea.View {
	if r.list != nil {
		return tea.NewView(themedList(r.list))
	}
	return r.searchProgress.View()
}

// newHistoryResume rehydrates the source group and opens the resume
// fan-out (python _rehydrate_group): the standard search progress
// surface driven by the record's bound title, with the record
// attached so a confident auto-match enters the session directly
// (I6).
func newHistoryResume(deps *Deps, rec *storage.AnimeProgress) *searchProgress {
	m := NewSearchProgress(deps, derefStr(rec.BoundTitle, rec.Title))
	m.resume = rec
	return m
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

// dedupe removes duplicate strings preserving order.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
