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
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Search flow screen ids.
const (
	searchProgressID = "search-progress"
	searchGroupID    = "search-group"
)

// searchTimeout bounds one provider search inside the fan-out.
const searchTimeout = 30 * time.Second

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
} // shikiEnrichmentActive gates the hybrid enrichment (PR25 A): the
// phase runs only with a wired, non-disabled Shikimori service — the
// enrichment is a bonus, never a requirement of search.
func shikiEnrichmentActive(deps *Deps) bool {
	if deps == nil || deps.Shiki == nil {
		return false
	}
	if !deps.Shiki.Enabled() {
		return false
	}
	return deps.Shiki.Mode() != "disabled"
}

// resolveSearchVariants runs the hybrid enrichment (PR24): Shikimori
// autocomplete over the original query → best-ratio match above the
// binding threshold → BOTH names of the matched title plus metadata
// aliases of the winning name → the capped variant set (original
// query first, max 8). Any failure — or a nil/disabled Shikimori —
// quietly degrades to the bare query (python parity; PR25 A:
// enrichment is optional).
//
// PR42: the binding ratios the query against BOTH names of a record
// (romaji/english AND russian — a Cyrillic query binds through the
// russian one), and both names seed the variant pool so the
// latin-only torrent providers can be routed the latin title.
func resolveSearchVariants(deps *Deps, query string) searchVariantsMsg {
	if !shikiEnrichmentActive(deps) {
		return searchVariantsMsg{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	items, err := deps.Shiki.Autocomplete(ctx, query, shikiAutocompleteLimit)
	if err != nil || len(items) == 0 {
		return searchVariantsMsg{}
	}
	best, bestName := bestShikiItem(query, items)
	if best == nil {
		return searchVariantsMsg{}
	}
	aliases := make([]string, 0, 2)
	for _, name := range []*string{best.TitleRu, best.TitleEn} {
		if name != nil && strings.TrimSpace(*name) != "" {
			aliases = append(aliases, *name)
		}
	}
	if deps.Metadata != nil {
		if more, err := deps.Metadata.SearchAlternativeTitles(ctx, bestName); err == nil {
			aliases = append(aliases, more...)
		}
	}
	return searchVariantsMsg{variants: metadata.QueryVariants(query, aliases)}
}

// shikiAutocompleteLimit caps the autocomplete records parsed for the
// enrichment binding (the endpoint returns a fixed handful per query;
// the same wire request the SearchIDs path makes).
const shikiAutocompleteLimit = 16

// bestShikiItem picks the autocomplete record whose name best matches
// the query above the binding threshold. Both names of a record
// compete — the romaji/english one and the russian one — so a Cyrillic
// query binds via its russian name (PR42 root cause: the latin-only
// match left every provider the bare Cyrillic query). Deterministic:
// record order wins ties, english before russian within a record.
// Returns the record and the winning name; nil under the threshold.
func bestShikiItem(query string, items []shikimori.AutocompleteItem) (*shikimori.AutocompleteItem, string) {
	bestItem, bestName := (*shikimori.AutocompleteItem)(nil), ""
	bestRatio := 0.0
	for i := range items {
		item := &items[i]
		for _, name := range []*string{item.TitleEn, item.TitleRu} {
			if name == nil || *name == "" {
				continue
			}
			ratio := providers.SimilarityRatio(strings.ToLower(query), strings.ToLower(*name))
			if ratio > bestRatio {
				bestRatio, bestItem, bestName = ratio, item, *name
			}
		}
	}
	if bestItem == nil || bestRatio <= shikiBindMinRatio {
		return nil, ""
	}
	return bestItem, bestName
}

// searchProgress is the live fan-out table (python
// search_provider_task + generate_search_table port, PR24 hybrid
// shape): the query first resolves Shikimori variants (when enabled),
// then one row per provider runs its language-routed variants inside
// a per-provider timeout budget.
//
// PR31 settled phase: once every row settles, EVERY result renders
// as its own checklist row BELOW the table — no similarity grouping,
// exact title+provider dedup only. Enter resolves the checked subset
// into the flow: a resumed record (the catalog «Списки» search)
// enters the session restored to its saved episode and dubs (I6); a
// fresh search with one provider checked opens the session directly,
// with several checked first the provider picker.
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
	// logTag prefixes this table's lifecycle log lines ("search" for
	// the plain flow, "rebind" for the lists binding flow — PR29).
	logTag string
	// resume carries the history record being continued (I6); nil in
	// the plain search flow. The catalog rebind flow sets it (PR30)
	// so a group pick resumes the session.
	resume *storage.AnimeProgress
	// resultCheck renders the per-result provider checklist BELOW the
	// table once every row settled (PR31: every result gets its own
	// row — no similarity grouping); nil until then.
	resultCheck *CheckList
	// titleOverride replaces the default live header when set (the
	// catalog flow's «Поиск по провайдерам: …», PR30/PR31).
	titleOverride string
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
		logTag:    "search",
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

// Init implements Screen: with Shikimori active the enrichment phase
// resolves the variant set first; everything else fans out
// immediately (PR25 A: a nil/disabled Shikimori never gates search).
// Commands own their timeout contexts rather than deriving from the
// app lifecycle — see the App.ctx divergence note.
func (m *searchProgress) Init() tea.Cmd {
	if m.deps != nil && m.deps.Log != nil {
		m.deps.Log.Info(m.logTag+": starting", "query", m.query, "providers", len(m.rows), "enrich", shikiEnrichmentActive(m.deps))
	}
	if m.deps != nil && shikiEnrichmentActive(m.deps) && len(m.rows) > 0 {
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
// language order, then applies the name-preference rule (PR42): a
// provider declaring NamePrefLatin (the latin-only torrent feeds) is
// queried with the latin variants ONLY — a Cyrillic query there is
// guaranteed-zero. Without any resolved latin variant the full set
// rides anyway (fail-soft: search, the provider returns 0).
func (m *searchProgress) providerQueries(providerID string) []string {
	lang := ""
	if m.deps != nil && m.deps.Episode != nil {
		lang = m.deps.Episode.ContentLanguage(providerID)
	}
	queries := queriesForLanguage(m.variants, lang)
	if m.deps == nil || m.deps.Search == nil {
		return queries
	}
	if m.deps.Search.NamePreference(providerID) != contracts.NamePrefLatin {
		return queries
	}
	if latin := latinOnlyQueries(queries); len(latin) > 0 {
		return latin
	}
	return queries
}

// latinOnlyQueries keeps the variants without Cyrillic characters, in
// order.
func latinOnlyQueries(queries []string) []string {
	out := make([]string, 0, len(queries))
	for _, q := range queries {
		if !hasCyrillic.MatchString(q) {
			out = append(out, q)
		}
	}
	return out
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
		if m.deps != nil && m.deps.Log != nil {
			m.deps.Log.Info(m.logTag+": provider settled",
				"provider", msg.provider.ID, "results", len(msg.results), "err", msg.err)
		}
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
		if len(m.pending) == 0 {
			if m.deps != nil && m.deps.Log != nil {
				m.deps.Log.Info(m.logTag+": complete",
					"responded", len(m.responded), "results", len(m.results))
			}
			// PR30/PR31: the settled table grows its results below
			// automatically — no enter gate between the fan-out and
			// the provider checklist.
			m.settleResults()
		}
		return m, nil
	case tea.KeyPressMsg:
		// PR78 type-to-search: with an engaged checklist filter the
		// first Esc clears it — only the second one pops (the
		// checklist is nil until the fan-out settles).
		if IsCancelKey(msg) && (m.resultCheck == nil || !m.resultCheck.filterActive()) {
			return m, pop()
		}
		if m.resultCheck != nil {
			// Settled (PR31): the below-table checklist owns the keys
			// — space/a/i toggle, enter resolves the checked subset
			// (esc already popped above).
			if m.resultCheck.HandleKey(msg) {
				return m, nil
			}
			if msg.Code != tea.KeyEnter {
				return m, nil
			}
			checked := m.resultCheck.CheckedItems()
			if len(checked) == 0 {
				return m, nil
			}
			group := make([]contracts.SearchResult, 0, len(checked))
			for _, c := range checked {
				if r, ok := c.Value.(contracts.SearchResult); ok {
					group = append(group, r)
				}
			}
			if m.resume != nil {
				// Catalog flow (I6): the selection resumes the record.
				// PR61: every checked provider joins the session (the
				// python merge) — the «Выберите провайдера» gate is
				// gone; the choice happens at the stream level.
				// PR62 #2: the pick IS the binding (python
				// search_and_bind commits before session_loop) — the
				// record moves onto the checked primary so the «!»
				// badge clears and the fan-out does not re-run on
				// re-entry.
				primary := stablePrimary(group)
				bindErr := bindRecordToSource(m.deps, m.resume, primary)
				if bindErr != nil {
					m.deps.logger().Error("search: provider binding failed",
						"record", m.resume.ID, "source", primary.SourceID, "error", bindErr)
				}
				sess := newResumedSession(m.deps, primary, stableGroup(group), *m.resume)
				if bindErr != nil {
					sess.setStatus("⚠ Не удалось сохранить привязку: " + bindErr.Error())
				}
				return m, replace(sess)
			}
			return m, replace(NewSessionScreen(m.deps, stablePrimary(group), stableGroup(group)))
		}
		if len(m.pending) > 0 {
			// Fan-out still running: nothing to pick yet — the
			// results appear on their own when the rows settle.
			return m, nil
		}
		if len(m.results) == 0 {
			return m, pop()
		}
		return m, nil
	default:
		return m, nil
	}
}

// bindRecordToSource persists the provider binding of a catalog
// record onto the checked primary (PR62 #2): the same write python
// search_and_bind performs before session_loop. Fast local SQLite
// write — inline like the other history writes in the TUI.
func bindRecordToSource(deps *Deps, rec *storage.AnimeProgress, primary contracts.SearchResult) error {
	if deps == nil || deps.History == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	return deps.History.BindSource(ctx, rec.ID, primary.SourceID, primary.URL, primary.Title)
}

// torrentResultSuffix renders the torrent preview suffix for search
// results that carry torrent metadata (PR36 — nyaa and the torrent
// providers generally): " · quality · size · seeds↑/leechers↓".
// Non-string meta values and absent keys are skipped; results without
// torrent meta (every stream provider) keep their plain labels.
func torrentResultSuffix(r contracts.SearchResult) string {
	meta := func(key string) string {
		s, _ := r.Meta[key].(string)
		return s
	}
	parts := make([]string, 0, 3)
	if q := meta(providers.SearchMetaQuality); q != "" {
		parts = append(parts, q)
	}
	if size := meta(providers.SearchMetaSize); size != "" {
		parts = append(parts, size)
	}
	seeds, leechers := meta(providers.SearchMetaSeeders), meta(providers.SearchMetaLeechers)
	if seeds != "" || leechers != "" {
		parts = append(parts, seeds+"↑/"+leechers+"↓")
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

// settleResults builds the below-table provider checklist (PR31):
// EVERY settled result becomes its own row — no similarity grouping,
// no dedup beyond an exact title+provider match (keep first). The
// label leads with the provider's friendly name so multi-provider
// hits of the same title stay distinguishable rows.
func (m *searchProgress) settleResults() {
	if m.resultCheck != nil || len(m.results) == 0 {
		return
	}
	names := make(map[string]string, len(m.rows))
	for _, row := range m.rows {
		names[row.ID] = row.Name
	}
	seen := make(map[string]bool, len(m.results))
	items := make([]Choice, 0, len(m.results))
	for _, r := range m.results {
		key := r.Title + "\x00" + r.SourceID
		if seen[key] {
			continue
		}
		seen[key] = true
		name := names[r.SourceID]
		if name == "" {
			name = r.SourceID
		}
		items = append(items, Choice{
			ID:    "r" + strconv.Itoa(len(items)),
			Label: name + " — " + r.Title + torrentResultSuffix(r),
			Value: r,
		})
	}
	m.resultCheck = NewCheckList("Выберите провайдеры:", items)
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
// centered overall counter (PR24); once every row settled, the
// grouped results render BELOW the table as a selectable list (PR30).
func (m *searchProgress) View() tea.View {
	var b strings.Builder
	header := m.titleOverride
	if header == "" {
		header = fmt.Sprintf("Поиск аниме (Найдено: %d)", len(m.results))
	}
	b.WriteString(theme.Title.Render(header))
	b.WriteString("\n\n")
	if m.enriching {
		b.WriteString(theme.Accent.Render("Shikimori: подбор вариантов поиска…"))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "  %s %s %s\n",
		padDisplay(theme.Dim.Render("Провайдер"), 16),
		padDisplay(theme.Dim.Render("Статус"), 36),
		theme.Dim.Render("Результатов"))
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
		fmt.Fprintf(&b, "  %s %s %s\n",
			padDisplay(row.Name, 16), padDisplay(style.Render(state), 36), padDisplay(count, 4))
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
	if m.resultCheck != nil {
		// PR31: settled — the provider checklist replaces the bare
		// counter area below the table (every result its own row).
		b.WriteString("\n")
		b.WriteString(m.resultCheck.Render())
	}
	if len(m.pending) == 0 && len(m.results) == 0 {
		b.WriteString("\n")
		b.WriteString(theme.Warning.Render("Ничего не найдено"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	// The settled checklist renders its own full key hints (space/a/
	// i/enter), so the outer status line stays the plain back hint.
	b.WriteString(theme.StatusLine.Render("esc — назад"))
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
			Label: fmt.Sprintf("%s [%s]", r.Title, r.SourceID) + torrentResultSuffix(r),
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
	// PR78 type-to-search: the first Esc clears an engaged filter.
	if IsCancelKey(key) && !g.check.filterActive() {
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
	return g, push(NewSessionScreen(g.deps, stablePrimary(group), stableGroup(group)))
}

// View implements Screen.
func (g *searchGroup) View() tea.View {
	if g.note != "" {
		return tea.NewView(theme.Warning.Render(g.note) + "\n" + g.check.Render())
	}
	return tea.NewView(g.check.Render())
}

// stableGroup returns a SourceID-sorted copy of the checked group
// (the removed provider picker's registry-stable order, PR61): the
// session's primary and history binding stay deterministic without a
// gate.
func stableGroup(group []contracts.SearchResult) []contracts.SearchResult {
	stable := append([]contracts.SearchResult(nil), group...)
	sort.SliceStable(stable, func(i, j int) bool { return stable[i].SourceID < stable[j].SourceID })
	return stable
}

// stablePrimary is the deterministic primary of a checked group: the
// sorted-first result.
func stablePrimary(group []contracts.SearchResult) contracts.SearchResult {
	stable := stableGroup(group)
	if len(stable) == 0 {
		return contracts.SearchResult{}
	}
	return stable[0]
}

// padDisplay right-pads s with spaces to the given display width,
// accounting for ANSI escape codes (zero visual width) and multi-byte
// Unicode (Cyrillic = 1 column, CJK = 2 columns). This replaces
// fmt's %-Ns which pads by byte count and misaligns Cyrillic rows.
func padDisplay(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}
