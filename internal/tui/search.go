package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
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

// searchOne runs one provider's search with a per-provider recover:
// panics degrade to row errors like the python try/except instead of
// killing the fan-out (the app-level safeCmd stays as the outer net).
func searchOne(ctx context.Context, deps *Deps, providerID, query string) (msg providerResultMsg) {
	defer func() {
		if r := recover(); r != nil {
			msg = providerResultMsg{
				provider: ProviderMeta{ID: providerID},
				err:      fmt.Errorf("%w: %v", errPanic, r),
			}
		}
	}()
	results, err := deps.Search.Search(ctx, providerID, query)
	return providerResultMsg{results: results, err: err}
}

// searchProgress is the live fan-out table (python
// search_provider_task + generate_search_table port): one row per
// provider, spinner while pending, Найдено/Ошибка when settled; enter
// advances to the manual grouping checklist once every row settled.
type searchProgress struct {
	deps    *Deps
	query   string
	spin    spinner.Model
	rows    []ProviderMeta
	status  map[string]string
	pending map[string]bool
	results []contracts.SearchResult
}

// NewSearchProgress builds the fan-out screen and schedules one
// panic-safe command per provider.
//
//nolint:revive // internal screen type
func NewSearchProgress(deps *Deps, query string) *searchProgress {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	rows := []ProviderMeta{}
	if deps != nil && deps.Search != nil {
		rows = deps.Search.Providers()
	}
	m := &searchProgress{
		deps:    deps,
		query:   query,
		spin:    sp,
		rows:    rows,
		status:  make(map[string]string, len(rows)),
		pending: make(map[string]bool, len(rows)),
	}
	for _, r := range rows {
		m.status[r.ID] = "Загрузка…"
		m.pending[r.ID] = true
	}
	return m
}

// ID implements Screen.
func (m *searchProgress) ID() string { return searchProgressID }

// Init implements Screen: fan out one safe command per provider plus
// the spinner tick.
func (m *searchProgress) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick}
	for _, row := range m.rows {
		cmds = append(cmds, safeCmd(searchProgressID, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
			defer cancel()
			return withProvider(searchOne(ctx, m.deps, row.ID, m.query), row)
		}))
	}
	return tea.Batch(cmds...)
}

// Update implements Screen.
func (m *searchProgress) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case providerResultMsg:
		delete(m.pending, msg.provider.ID)
		switch {
		case msg.err != nil:
			m.status[msg.provider.ID] = "Ошибка: " + msg.err.Error()
		case len(msg.results) == 0:
			m.status[msg.provider.ID] = "Ничего не найдено"
		default:
			m.status[msg.provider.ID] = fmt.Sprintf("Найдено: %d", len(msg.results))
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
		if len(m.results) == 0 {
			return m, pop()
		}
		return m, replace(NewSearchGroup(m.deps, m.results))
	default:
		return m, nil
	}
}

// View implements Screen: the live status table.
func (m *searchProgress) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render(fmt.Sprintf("Поиск аниме (Найдено: %d)", len(m.results))))
	b.WriteString("\n\n")
	for _, row := range m.rows {
		state := m.status[row.ID]
		style := theme.Dim
		switch {
		case m.pending[row.ID]:
			state = m.spin.View() + " " + state
			style = theme.Accent
		case strings.HasPrefix(state, "Найдено"):
			style = theme.Success
		case strings.HasPrefix(state, "Ошибка"):
			style = theme.Error
		}
		fmt.Fprintf(&b, "  %-16s %s\n", row.Name, style.Render(state))
	}
	if len(m.rows) == 0 {
		b.WriteString(theme.Dim.Render("Нет зарегистрированных провайдеров"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if len(m.pending) == 0 && len(m.results) == 0 {
		b.WriteString(theme.Warning.Render("Ничего не найдено"))
		b.WriteString("\n")
	}
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
}

// NewSearchGroup builds the grouping screen over the flat result set.
//
//nolint:revive // internal screen type
func NewSearchGroup(deps *Deps, results []contracts.SearchResult) *searchGroup {
	items := make([]Choice, 0, len(results))
	for i, r := range results {
		items = append(items, Choice{
			ID:    fmt.Sprintf("r%d", i),
			Label: fmt.Sprintf("%s [%s]", r.Title, r.SourceID),
			Value: r,
		})
	}
	return &searchGroup{deps: deps, check: NewCheckList("Результаты поиска — отметьте один тайтл", items)}
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
	return tea.NewView(g.check.Render())
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

// View implements Screen.
func (s *searchSource) View() tea.View {
	return tea.NewView(s.list.Render())
}
