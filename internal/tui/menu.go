package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/storage"
)

// defaultListHeight is the fallback body height before the first
// WindowSizeMsg arrives.
const defaultListHeight = 12

// PickHandler receives a resolved pick: the nav.Back sentinel or one
// choice value. Returning nil keeps the screen open.
type PickHandler func(pick any) tea.Cmd

// MenuScreenConfig parameterizes a generic menu screen.
type MenuScreenConfig struct {
	// ID is the screen identity.
	ID string
	// Title renders as the header.
	Title string
	// EmptyMsg is the I3 empty-state message for empty choice lists.
	EmptyMsg string
	// Choices are the caller's entries; Back is appended LAST (I1).
	Choices []Choice
	// OnPick consumes resolutions; nil means "pop on any pick".
	OnPick PickHandler
	// Root enables the I2 root exception: Ctrl-C quits the app AND
	// drops the Back entry entirely — the root menu is backless, its
	// last item («🚪 Выход») occupies the pinned bottom slot.
	Root bool
	// Notices renders as red warning lines between title and list
	// (root screen only — disabled providers, unconfigured Shikimori).
	Notices []string
	// Status is an optional bottom hint line.
	Status string
	// Height overrides the body height (0 = default).
	Height int
	// Markers attaches per-item markers (index into Items: choices
	// first, the trailing Back row last).
	Markers map[int]string
}

// MenuScreen is the generic §5-compliant menu: a PinList over a Menu
// with Back appended last, cancel keys normalized, and picks delegated
// to the flow handler.
type MenuScreen struct {
	id      string
	title   string
	root    bool
	status  string
	list    *PinList
	onPick  PickHandler
	notices []string
}

// NewMenuScreen builds the screen from cfg.
func NewMenuScreen(cfg MenuScreenConfig) *MenuScreen {
	var menu Menu
	if cfg.Root {
		menu = NewMenuWithoutBack(cfg.Title, cfg.EmptyMsg, cfg.Choices...)
	} else {
		menu = NewMenu(cfg.Title, cfg.EmptyMsg, cfg.Choices...)
	}
	height := cfg.Height
	if height <= 0 {
		height = defaultListHeight
	}
	l := NewPinList(menu, height)
	for idx, marker := range cfg.Markers {
		l.SetMarker(idx, marker)
	}
	onPick := cfg.OnPick
	if onPick == nil {
		onPick = func(any) tea.Cmd { return pop() }
	}
	return &MenuScreen{
		id:      cfg.ID,
		title:   cfg.Title,
		root:    cfg.Root,
		status:  cfg.Status,
		list:    l,
		onPick:  onPick,
		notices: cfg.Notices,
	}
}

// ID implements Screen.
func (m *MenuScreen) ID() string { return m.id }

// Init implements Screen.
func (m *MenuScreen) Init() tea.Cmd { return nil }

// SetHeight adjusts the body height on window resize.
func (m *MenuScreen) SetHeight(h int) {
	if h > 2 {
		*m.list = *NewPinList(m.list.Menu(), h-4)
		m.list.Jump(m.list.cursor)
	}
}

// Update implements Screen: movement keys drive the list; Enter and
// cancel keys resolve through nav; the root exception routes Ctrl-C
// to quit.
func (m *MenuScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}

	if m.root && RootInterruptExits(key) {
		return m, quit()
	}

	if m.list.HandleKey(key) {
		return m, nil
	}

	resolved := ResolveKey(m.list.Menu(), m.list.Cursor(), key)
	if resolved == nil {
		return m, nil
	}
	return m, m.onPick(resolved)
}

// View implements Screen. The title renders with a leading pad and a
// blank line before the list (PR24 title padding); the root screen
// additionally renders the red startup notices (disabled providers,
// unconfigured Shikimori) between the title and the list — they live
// INSIDE the TUI because pre-alt-screen terminal output is invisible.
func (m *MenuScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render(m.title)...)
	b = append(b, '\n', '\n')
	if m.root && len(m.notices) > 0 {
		for _, n := range m.notices {
			b = append(b, theme.Error.Render(n)...)
			b = append(b, '\n')
		}
		b = append(b, '\n')
	}
	b = append(b, m.list.Render()...)
	if m.status != "" {
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render(m.status)...)
	}
	return tea.NewView(string(b))
}

// Root screen ids and labels (PR110: labels resolve through i18n at
// construction time — package-level vars would freeze the pre-Init
// default, so the former consts became functions).
const rootScreenID = "root"

func rootListsLabel() string   { return i18n.T("menu.lists") }
func rootOfflineLabel() string { return i18n.T("menu.offline") }
func rootDBLabel() string      { return i18n.T("menu.db") }
func rootHealthLabel() string  { return i18n.T("menu.health") }
func rootSeasonLabel() string  { return i18n.T("menu.season") }
func rootExitLabel() string    { return i18n.T("menu.exit") }
func rootBackHint() string     { return i18n.T("menu.root_hint") }

// NewRootScreen builds the root menu: the four feature entries, then
// the PR113 «▶ Продолжить» row, then the PR114 «📅 Сезон» entry,
// with «🚪 Выход» as the pinned BOTTOM row and NO «Назад» entry
// (there is nothing above root to go back to, PR24); only here does
// Ctrl-C exit the app (I2 exception).
//
// The continue row rides ON TOP of the generic menu (the
// historyFilter embedding pattern): the wrapper owns the render-time
// label refresh and the pick, the embedded MenuScreen keeps the §5
// rendering and navigation unchanged.
//
//nolint:revive // internal screen type; tests assert on the concrete struct (NewSessionScreen pattern)
func NewRootScreen(deps *Deps) *rootScreen {
	r := &rootScreen{deps: deps}
	r.MenuScreen = NewMenuScreen(MenuScreenConfig{
		ID:      rootScreenID,
		Title:   i18n.T("menu.app_title"),
		Root:    true,
		Notices: deps.StartupNotices,
		Choices: []Choice{
			{ID: "lists", Label: rootListsLabel()},
			{ID: "downloads", Label: rootOfflineLabel()},
			{ID: "db", Label: rootDBLabel()},
			{ID: "check", Label: rootHealthLabel()},
			// PR113: appended AFTER the feature entries — nothing above
			// moves and «Выход» keeps the pinned bottom slot (I1). The
			// id joins the ▶ watch action family (PR74: one emoji, one
			// action — continuing IS watching).
			{ID: "watch", Label: rootContinueEmptyLabel()},
			// PR114: seasonal calendar, after the continue row.
			{ID: "season", Label: rootSeasonLabel()},
			{ID: "exit", Label: rootExitLabel()},
		},
		Status: rootBackHint(),
		OnPick: func(pick any) tea.Cmd {
			switch pick {
			case Back:
				// Esc at root normalizes to Back, which at root means
				// "stay" (I2: never an app exit).
				return nil
			case "lists":
				return push(NewHistoryFilter(r.deps))
			case "downloads":
				return push(NewOfflineTitles(r.deps))
			case "db":
				return push(NewDBMenu(r.deps))
			case "check":
				return push(NewHealthScreen(r.deps))
			case "watch":
				return r.continuePick()
			case "season":
				return push(NewSeasonalScreen(r.deps))
			case "exit":
				return quit()
			default:
				return nil
			}
		},
	})
	r.refreshContinue()
	return r
}

// rootContinueEmptyLabel is the dim dash placeholder of the continue
// row (PR110: resolved through i18n at construction time — package
// vars would freeze the pre-Init default).
func rootContinueEmptyLabel() string { return i18n.T("menu.continue_empty") }

// rootScreen wraps the root MenuScreen with the PR113 continue row.
// The row's label and actionability are computed at RENDER time from
// the history service: popToRoot reuses this screen instance, so a
// construction-time label would go stale the moment the user watches
// something and returns.
type rootScreen struct {
	*MenuScreen
	deps *Deps
	// hint is the transient status line answering a no-op pick on the
	// dim row (empty history, PR41 B2 — a disabled row never acts
	// silently); any next key press clears it.
	hint string
}

// latestRecord loads the most recently updated history row — the
// history service lists rows newest-first, so items[0] IS the
// "most recently played" record across all anime. nil when the store
// is absent, unreadable or empty.
func (r *rootScreen) latestRecord() *storage.AnimeProgress {
	items, err := loadHistory(r.deps)
	if err != nil || len(items) == 0 {
		return nil
	}
	return &items[0]
}

// refreshContinue re-renders the «Продолжить» row from the newest
// history record: the label and the dim/non-actionable state.
func (r *rootScreen) refreshContinue() {
	for i := range r.list.Menu().Items {
		if r.list.Menu().Items[i].ID != "watch" {
			continue
		}
		rec := r.latestRecord()
		r.list.Menu().Items[i].Label = ContinueLabel(rec)
		r.list.Menu().Items[i].Disabled = rec == nil
		return
	}
}

// continuePick resolves Enter on the «Продолжить» row: nothing to
// continue answers with the transient hint; a record without a
// usable source takes the manual history flow's rebind path; a bound
// record pushes the resumed session — the same screen the manual flow
// pushes, restored onto the labeled episode, one keypress earlier.
func (r *rootScreen) continuePick() tea.Cmd {
	rec := r.latestRecord()
	if rec == nil {
		r.hint = i18n.T("menu.continue_hint_empty")
		return nil
	}
	if rec.NeedsCorrection || rec.SourceID == "" || rec.SourceURL == "" {
		return push(newRebindProgress(r.deps, rec))
	}
	target := *rec
	target.CurrentEpisode = ContinueTarget(*rec)
	primary := contracts.SearchResult{
		Title:    derefStr(rec.BoundTitle, rec.Title),
		URL:      rec.SourceURL,
		SourceID: rec.SourceID,
	}
	if rec.Poster != nil {
		primary.Poster = *rec.Poster
	}
	return push(newResumedSession(r.deps, primary, []contracts.SearchResult{primary}, target))
}

// Update implements Screen: any key press retires the transient hint
// (the pick below may re-arm it within the same update), then the
// wrapped menu handles the key; the wrapper identity stays on the
// stack (the historyFilter pattern).
func (r *rootScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if _, isKey := msg.(tea.KeyPressMsg); isKey {
		r.hint = ""
	}
	next, cmd := r.MenuScreen.Update(msg)
	if next == Screen(r.MenuScreen) {
		return r, cmd
	}
	return next, cmd
}

// View implements Screen: the continue row refreshes first so the
// label always tracks the history store; the transient hint replaces
// the status line while it lives.
func (r *rootScreen) View() tea.View {
	r.refreshContinue()
	base := r.status
	if r.hint != "" {
		r.status = r.hint
	}
	v := r.MenuScreen.View()
	r.status = base
	return v
}
