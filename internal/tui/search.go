package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/metadata"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Search flow screen ids.
const (
	searchInputID    = "search-input"
	searchProgressID = "search-progress"
	searchGroupID    = "search-group"
	searchSourceID   = "search-source"
)

// searchTimeout bounds one provider search inside the fan-out.
const searchTimeout = 30 * time.Second

// NewSearchInput builds the query prompt (python search_and_start's
// questionary.text port). Empty input and interrupts normalize to
// Back (I2) and pop the screen.
func NewSearchInput(deps *Deps) *TextPrompt {
	return NewTextPrompt(TextPromptConfig{
		ID:          searchInputID,
		Title:       "🔎 Поиск:",
		Placeholder: "название аниме…",
		Status:      "enter — искать · esc — назад",
		OnSubmit: func(resolved any) tea.Cmd {
			query, ok := resolved.(string)
			if !ok {
				return pop()
			}
			return replace(NewSearchProgress(deps, query))
		},
	})
}

// providerResultMsg settles one provider's fan-out row.
type providerResultMsg struct {
	provider ProviderMeta
	results  []contracts.SearchResult
	err      error
}

// withProvider re-tags a settled row with its provider metadata.
func withProvider(msg providerResultMsg, prov ProviderMeta) providerResultMsg {
	msg.provider = prov
	return msg
}

// errSearchTimeout marks a provider that blew its whole fan-out budget
// (all query variants included) — rendered as the dedicated ⏱ row.
var errSearchTimeout = errors.New("таймаут")

// searchProviderVariants runs ONE provider's share of the hybrid
// fan-out (PR24): the language-routed query variants in order, within
// the caller's per-provider budget. The loop stops at the first
// variant yielding results (bounded load); an erroring variant fails
// the row. Panics degrade to row errors like the python try/except.
func searchProviderVariants(ctx context.Context, deps *Deps, providerID string, queries []string) (msg providerResultMsg) {
	defer func() {
		if r := recover(); r != nil {
			msg = providerResultMsg{
				provider: ProviderMeta{ID: providerID},
				err:      fmt.Errorf("%w: %v", errPanic, r),
			}
		}
	}()
	var results []contracts.SearchResult
	for _, q := range queries {
		if ctx.Err() != nil {
			return providerResultMsg{
				provider: ProviderMeta{ID: providerID},
				err:      fmt.Errorf("%w: %w", errSearchTimeout, ctx.Err()),
			}
		}
		res, err := deps.Search.Search(ctx, providerID, q)
		if err != nil {
			if ctx.Err() != nil {
				// The budget expired mid-request: report the timeout,
				// not the downstream's mangled error text.
				return providerResultMsg{
					provider: ProviderMeta{ID: providerID},
					err:      fmt.Errorf("%w: %w", errSearchTimeout, ctx.Err()),
				}
			}
			return providerResultMsg{provider: ProviderMeta{ID: providerID}, err: err}
		}
		if len(res) > 0 {
			return providerResultMsg{provider: ProviderMeta{ID: providerID}, results: res}
		}
		results = res
	}
	return providerResultMsg{provider: ProviderMeta{ID: providerID}, results: results}
}

// queriesForLanguage orders the query variants for one provider
// (PR24): Russian providers get the Cyrillic variants first; other
// declared languages (ja/en sites that choke on Cyrillic —
// gogoanime's WordPress EOFs on it) get the Latin/romaji variants
// first; unknown/undeclared languages keep the original order.
func queriesForLanguage(variants []string, lang string) []string {
	if len(variants) <= 1 || lang == "" {
		return variants
	}
	var cyr, lat []string
	for _, v := range variants {
		if hasCyrillic.MatchString(v) {
			cyr = append(cyr, v)
		} else {
			lat = append(lat, v)
		}
	}
	if strings.HasPrefix(lang, "ru") {
		return append(cyr, lat...)
	}
	return append(lat, cyr...)
}

// searchVariantsMsg settles the Shikimori enrichment phase: empty
// variants mean "no enrichment, run the bare query".
type searchVariantsMsg struct {
	variants []string
}

// resolveSearchVariants runs the hybrid enrichment (PR24): Shikimori
// SearchIDs over the original query → best-ratio match above the
// binding threshold → metadata aliases of the MATCHED title → the
// capped variant set (original query first, max 8). Any failure
// quietly degrades to the bare query (python parity).
func resolveSearchVariants(deps *Deps, query string) searchVariantsMsg {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ids, err := deps.Shiki.SearchIDs(ctx, query)
	if err != nil || len(ids) == 0 {
		return searchVariantsMsg{}
	}
	bestTitle, bestID := bestShikiCandidate(query, ids)
	if bestID == 0 {
		return searchVariantsMsg{}
	}
	aliases := []string{bestTitle}
	if deps.Metadata != nil {
		if more, err := deps.Metadata.SearchAlternativeTitles(ctx, bestTitle); err == nil {
			aliases = append(aliases, more...)
		}
	}
	return searchVariantsMsg{variants: metadata.QueryVariants(query, aliases)}
}

// bestShikiCandidate picks the best SequenceMatcher-ratio match above
// the binding threshold (the resolveShikiBinding criterion, reused for
// the variant seed).
func bestShikiCandidate(query string, ids map[string]int64) (string, int64) {
	bestTitle, bestID := "", int64(0)
	bestRatio := 0.0
	for cand, id := range ids {
		ratio := providers.SimilarityRatio(strings.ToLower(query), strings.ToLower(cand))
		if ratio > bestRatio {
			bestRatio, bestTitle, bestID = ratio, cand, id
		}
	}
	if bestID == 0 || bestRatio <= shikiBindMinRatio {
		return "", 0
	}
	return bestTitle, bestID
}

// searchProgress is the live fan-out table (python
// search_provider_task + generate_search_table port, PR24 hybrid
// shape): the query first resolves Shikimori variants (when enabled),
// then one row per provider runs its language-routed variants inside
// a per-provider timeout budget. Enter advances to the manual
// grouping checklist once every row settled.
// In resume mode (resume != nil) enter first tries the record's
// rehydrate auto-match (I6) and only falls through to manual
// grouping with a note when no match exists.
type searchProgress struct {
	deps    *Deps
	query   string
	spin    spinner.Model
	rows    []ProviderMeta
	status  map[string]string
	pending map[string]bool
	// counts carries each row's result count for the Результатов
	// column; responded marks rows that settled without an error
	// (0 results still counts as answered).
	counts    map[string]int
	responded map[string]bool
	results   []contracts.SearchResult
	// variants is the active query-variant set (bare query until the
	// enrichment settles); enriching marks the Shikimori phase.
	variants  []string
	enriching bool
	// resume carries the history record being continued (I6); nil in
	// the plain search flow.
	resume *storage.AnimeProgress
}

// NewSearchProgress builds the fan-out screen and schedules the
// enrichment (Shikimori enabled) or the fan-out directly.
//
//nolint:revive // internal screen type
func NewSearchProgress(deps *Deps, query string) *searchProgress {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	rows := []ProviderMeta{}
	if deps != nil && deps.Search != nil {
		rows = deps.Search.Providers()
	}
	m := &searchProgress{
		deps:      deps,
		query:     query,
		spin:      sp,
		rows:      rows,
		status:    make(map[string]string, len(rows)),
		pending:   make(map[string]bool, len(rows)),
		counts:    make(map[string]int, len(rows)),
		responded: make(map[string]bool, len(rows)),
		variants:  []string{query},
	}
	for _, r := range rows {
		m.status[r.ID] = "Ожидание…"
		// Rows are pending from construction: the enrichment phase and
		// the fan-out both settle them later; Enter stays blocked
		// until every row lands.
		m.pending[r.ID] = true
	}
	return m
}

// searchBudget is the per-provider fan-out ceiling.
func (m *searchProgress) searchBudget() time.Duration {
	if m.deps != nil && m.deps.SearchTimeout > 0 {
		return m.deps.SearchTimeout
	}
	return searchTimeout
}

// ID implements Screen.
func (m *searchProgress) ID() string { return searchProgressID }

// Init implements Screen: with Shikimori enabled the enrichment phase
// resolves the variant set first; everything else fans out
// immediately. Commands own their timeout contexts rather than
// deriving from the app lifecycle — see the App.ctx divergence note.
func (m *searchProgress) Init() tea.Cmd {
	if m.deps != nil && m.deps.Shiki != nil && m.deps.Shiki.Enabled() && len(m.rows) > 0 {
		m.enriching = true
		return tea.Batch(m.spin.Tick, safeCmd(searchProgressID, func() tea.Msg {
			return resolveSearchVariants(m.deps, m.query)
		}))
	}
	return m.startFanOut()
}

// startFanOut schedules one panic-safe variant command per provider.
func (m *searchProgress) startFanOut() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick}
	budget := m.searchBudget()
	for _, row := range m.rows {
		queries := m.providerQueries(row.ID)
		cmds = append(cmds, safeCmd(searchProgressID, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			return withProvider(searchProviderVariants(ctx, m.deps, row.ID, queries), row)
		}))
	}
	return tea.Batch(cmds...)
}

// providerQueries routes the active variants into the provider's
// language order.
func (m *searchProgress) providerQueries(providerID string) []string {
	lang := ""
	if m.deps != nil && m.deps.Episode != nil {
		lang = m.deps.Episode.ContentLanguage(providerID)
	}
	return queriesForLanguage(m.variants, lang)
}

// Update implements Screen.
func (m *searchProgress) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case searchVariantsMsg:
		if !m.enriching {
			return m, nil
		}
		m.enriching = false
		if len(msg.variants) > 0 {
			m.variants = msg.variants
		}
		return m, m.startFanOut()
	case providerResultMsg:
		delete(m.pending, msg.provider.ID)
		switch {
		case msg.err != nil:
			m.status[msg.provider.ID] = searchErrText(msg.err)
		default:
			m.responded[msg.provider.ID] = true
			m.counts[msg.provider.ID] = len(msg.results)
			m.status[msg.provider.ID] = "Завершено"
			m.results = append(m.results, msg.results...)
		}
		return m, nil
	case tea.KeyPressMsg:
		if IsCancelKey(msg) {
			return m, pop()
		}
		if msg.Code != tea.KeyEnter {
			return m, nil
		}
		if len(m.pending) > 0 {
			return m, nil
		}
		if m.resume != nil {
			// Resume (I6, python history.py _rehydrate_group): a
			// confident match enters the session directly, restored
			// to the saved episode and dubs; anything else (including
			// an empty result set) falls through to manual grouping
			// with a note.
			groups := GroupByTitle(m.results, rehydrateGroupThreshold)
			if matched := RehydrateGroup(groups, *m.resume); matched != nil {
				primary := primaryForResume(matched, *m.resume)
				return m, replace(newResumedSession(m.deps, primary, matched, *m.resume))
			}
			return m, replace(newSearchGroupNoted(m.deps, m.results,
				"Автопривязка не найдена — отметьте один тайтл и сгруппируйте вручную"))
		}
		if len(m.results) == 0 {
			return m, pop()
		}
		return m, replace(NewSearchGroup(m.deps, m.results))
	default:
		return m, nil
	}
}

// searchErrText renders one settled error: timeouts get the dedicated
// ⏱ verdict; everything else keeps the error text.
func searchErrText(err error) string {
	if errors.Is(err, errSearchTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "Таймаут"
	}
	return err.Error()
}

// tableWidth is the fixed width the centered counter is placed into
// (the table's own visual width; screens have no terminal width).
const tableWidth = 60

// View implements Screen: the live three-column status table with the
// centered overall counter (PR24).
func (m *searchProgress) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render(fmt.Sprintf("Поиск аниме (Найдено: %d)", len(m.results))))
	b.WriteString("\n\n")
	if m.enriching {
		b.WriteString(theme.Accent.Render("Shikimori: подбор вариантов поиска…"))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "  %-16s %-36s %s\n",
		theme.Dim.Render("Провайдер"), theme.Dim.Render("Статус"), theme.Dim.Render("Результатов"))
	for _, row := range m.rows {
		state := m.status[row.ID]
		count := "—"
		var style lipgloss.Style
		switch {
		case m.pending[row.ID] && m.enriching:
			// Waiting for the Shikimori variant phase, not the
			// provider itself yet.
			style = theme.Dim
		case m.pending[row.ID]:
			state = m.spin.View() + " Поиск…"
			style = theme.Accent
		case state == "Завершено":
			state = "✓ " + state
			count = strconv.Itoa(m.counts[row.ID])
			style = theme.Success
		case state == "Таймаут":
			state = "⏱ " + state
			style = theme.Warning
		default:
			state = "✗ " + state
			style = theme.Error
		}
		fmt.Fprintf(&b, "  %-16s %-36s %s\n", row.Name, style.Render(state), count)
	}
	if len(m.rows) == 0 {
		b.WriteString(theme.Dim.Render("Нет зарегистрированных провайдеров"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	responded := len(m.responded)
	if len(m.rows) > 0 && (responded > 0 || len(m.pending) == 0) {
		counter := fmt.Sprintf("Ответившие: %d/%d провайдеров · Всего результатов: %d",
			responded, len(m.rows), len(m.results))
		b.WriteString(lipgloss.PlaceHorizontal(tableWidth, lipgloss.Center, theme.StatusLine.Render(counter)))
		b.WriteString("\n")
	}
	if len(m.pending) == 0 && len(m.results) == 0 {
		b.WriteString("\n")
		b.WriteString(theme.Warning.Render("Ничего не найдено"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(theme.StatusLine.Render("enter — продолжить · esc — назад"))
	return tea.NewView(b.String())
}

// searchGroup is the manual grouping checklist: the user marks the
// results that belong to the same title, enter proceeds to the source
// pick (this ports the Python semantic grouper's role onto explicit
// user action per the Go TUI spec).
type searchGroup struct {
	deps  *Deps
	check *CheckList
	// note renders above the checklist (resume fall-through notice).
	note string
}

// NewSearchGroup builds the grouping screen over the flat result set.
//
//nolint:revive // internal screen type
func NewSearchGroup(deps *Deps, results []contracts.SearchResult) *searchGroup {
	return newSearchGroupNoted(deps, results, "")
}

// newSearchGroupNoted builds the grouping screen with an explanatory
// note (the resume fall-through path, I6).
func newSearchGroupNoted(deps *Deps, results []contracts.SearchResult, note string) *searchGroup {
	items := make([]Choice, 0, len(results))
	for i, r := range results {
		items = append(items, Choice{
			ID:    fmt.Sprintf("r%d", i),
			Label: fmt.Sprintf("%s [%s]", r.Title, r.SourceID),
			Value: r,
		})
	}
	return &searchGroup{deps: deps, check: NewCheckList("Результаты поиска — отметьте один тайтл", items), note: note}
}

// ID implements Screen.
func (g *searchGroup) ID() string { return searchGroupID }

// Init implements Screen.
func (g *searchGroup) Init() tea.Cmd { return nil }

// Update implements Screen.
func (g *searchGroup) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return g, nil
	}
	if IsCancelKey(key) {
		return g, pop()
	}
	if g.check.HandleKey(key) {
		return g, nil
	}
	if key.Code != tea.KeyEnter {
		return g, nil
	}
	checked := g.check.CheckedItems()
	if len(checked) == 0 {
		return g, nil
	}
	group := make([]contracts.SearchResult, 0, len(checked))
	for _, c := range checked {
		if r, ok := c.Value.(contracts.SearchResult); ok {
			group = append(group, r)
		}
	}
	return g, push(NewSearchSource(g.deps, group))
}

// View implements Screen.
func (g *searchGroup) View() tea.View {
	if g.note != "" {
		return tea.NewView(theme.Warning.Render(g.note) + "\n" + g.check.Render())
	}
	return tea.NewView(g.check.Render())
}

// rehydrateGroupThreshold is the clustering similarity used before the
// resume rehydrate match (same 0.6 family as the rebind flow).
const rehydrateGroupThreshold = 0.6

// primaryForResume picks the session primary: the group member that
// matches the record's stored (source, url); else the record's own
// source (python keeps the saved res as primary even when only a
// similar group matched).
func primaryForResume(group []contracts.SearchResult, rec storage.AnimeProgress) contracts.SearchResult {
	for _, res := range group {
		if res.SourceID == rec.SourceID && res.URL == rec.SourceURL {
			return res
		}
	}
	return contracts.SearchResult{Title: rec.Title, SourceID: rec.SourceID, URL: rec.SourceURL}
}

// searchSource picks the primary source of the freshly grouped title
// (python selected_group[0] made explicit).
type searchSource struct {
	deps  *Deps
	group []contracts.SearchResult
	list  *PinList
}

// NewSearchSource builds the source picker: each grouped result in
// registry-stable order.
//
//nolint:revive // internal screen type
func NewSearchSource(deps *Deps, group []contracts.SearchResult) *searchSource {
	stable := append([]contracts.SearchResult(nil), group...)
	sort.SliceStable(stable, func(i, j int) bool { return stable[i].SourceID < stable[j].SourceID })
	choices := make([]Choice, 0, len(stable))
	for i, r := range stable {
		choices = append(choices, Choice{
			ID:    fmt.Sprintf("s%d", i),
			Label: fmt.Sprintf("%s — %s", r.Title, r.SourceID),
			Value: r,
		})
	}
	title := "Источник: " + BestDisplayTitle(group)
	return &searchSource{
		deps:  deps,
		group: stable,
		list:  NewPinList(NewMenu(title, "", choices...), defaultListHeight),
	}
}

// ID implements Screen.
func (s *searchSource) ID() string { return searchSourceID }

// Init implements Screen.
func (s *searchSource) Init() tea.Cmd { return nil }

// Update implements Screen.
func (s *searchSource) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	if IsCancelKey(key) {
		return s, pop()
	}
	if s.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	primary, ok := resolved.(contracts.SearchResult)
	if !ok {
		return s, pop()
	}
	return s, replace(NewSessionScreen(s.deps, primary, s.group))
}

// View implements Screen: the padded source-picker title above the
// list (PR24).
func (s *searchSource) View() tea.View {
	return tea.NewView(theme.Title.Render(s.list.Menu().Title) + "\n\n" + s.list.Render())
}
