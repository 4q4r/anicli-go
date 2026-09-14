package tui

import (
	tea "charm.land/bubbletea/v2"
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

// Root screen ids and labels (RU vocabulary from the Python menu).
const (
	rootScreenID     = "root"
	rootSearchLabel  = "🔎 Поиск"
	rootListsLabel   = "📜 Списки"
	rootOfflineLabel = "📂 Скачанное"
	rootDBLabel      = "🗄️ Управление БД"
	rootHealthLabel  = "🛠 Проверка"
	rootExitLabel    = "🚪 Выход"
	rootBackHint     = "enter — выбрать · ctrl+c — выход"
)

// NewRootScreen builds the root menu: six entries with «🚪 Выход» as
// the pinned BOTTOM row and NO «Назад» entry (there is nothing above
// root to go back to, PR24); only here does Ctrl-C exit the app (I2
// exception).
func NewRootScreen(deps *Deps) *MenuScreen {
	return NewMenuScreen(MenuScreenConfig{
		ID:      rootScreenID,
		Title:   "AniCLI — аниме в терминале",
		Root:    true,
		Notices: deps.StartupNotices,
		Choices: []Choice{
			{ID: "search", Label: rootSearchLabel},
			{ID: "lists", Label: rootListsLabel},
			{ID: "downloads", Label: rootOfflineLabel},
			{ID: "db", Label: rootDBLabel},
			{ID: "check", Label: rootHealthLabel},
			{ID: "exit", Label: rootExitLabel},
		},
		Status: rootBackHint,
		OnPick: func(pick any) tea.Cmd {
			switch pick {
			case Back:
				// Esc at root normalizes to Back, which at root means
				// "stay" (I2: never an app exit).
				return nil
			case "search":
				return push(NewSearchInput(deps))
			case "lists":
				return push(NewHistoryFilter(deps))
			case "downloads":
				return push(NewOfflineTitles(deps))
			case "db":
				return push(NewDBMenu(deps))
			case "check":
				return push(NewHealthScreen(deps))
			case "exit":
				return quit()
			default:
				return nil
			}
		},
	})
}
