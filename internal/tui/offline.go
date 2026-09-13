package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/download"
)

// Offline flow screen ids.
const (
	offlineTitlesID  = "offline-titles"
	offlineSessionID = "offline-session"
)

// offlinePlayMsg carries one resolved local playback.
type offlinePlayMsg struct {
	path  string
	title string
}

// NewOfflineTitles builds the «📂 Скачанное» title list (python
// offline_menu port) with episode and variant counts; an empty
// library renders its empty state (I3).
func NewOfflineTitles(deps *Deps) *MenuScreen {
	var titles []OfflineTitle
	if deps != nil && deps.Offline != nil {
		titles, _ = deps.Offline.Titles() //nolint:errcheck // errors degrade to the empty state
	}
	emptyMsg := ""
	if len(titles) == 0 {
		emptyMsg = "Нет скачанных тайтлов"
	}
	choices := make([]Choice, 0, len(titles))
	for i := range titles {
		t := titles[i]
		episodes := make(map[string]struct{})
		for _, e := range t.Snapshot.Entries {
			episodes[e.EpisodeNum] = struct{}{}
		}
		choices = append(choices, Choice{
			ID:    t.Name,
			Label: "📁 " + t.Name + " [" + intToStr(len(episodes)) + " сер., " + intToStr(t.Snapshot.TotalDownloaded()) + " лок. вариантов]",
			Value: &titles[i],
		})
	}
	return NewMenuScreen(MenuScreenConfig{
		ID:       offlineTitlesID,
		Title:    "📂 Скачанные тайтлы:",
		EmptyMsg: emptyMsg,
		Choices:  choices,
		OnPick: func(pick any) tea.Cmd {
			if pick == Back {
				return pop()
			}
			title, ok := pick.(*OfflineTitle)
			if !ok {
				return pop()
			}
			return push(NewOfflineSession(deps, *title))
		},
	})
}

// offlineSession is the offline watch loop (python _offline_session):
// episode pick, local variant resolve, playback with the [OFFLINE]
// title and NO skip lookups (chapters are embedded at download time).
type offlineSession struct {
	deps         *Deps
	title        OfflineTitle
	current      string
	videoKey     string
	audioKey     string
	quality      int
	stateVariant bool
	list         *PinList // action menu
	episodeList  *PinList // episodes
	variantList  *PinList // local variant picker
}

// NewOfflineSession builds the offline session for one title.
func NewOfflineSession(deps *Deps, title OfflineTitle) *offlineSession {
	s := &offlineSession{deps: deps, title: title}
	s.buildEpisodeList()
	s.buildActionMenu()
	return s
}

// ID implements Screen.
func (s *offlineSession) ID() string { return offlineSessionID }

// Init implements Screen.
func (s *offlineSession) Init() tea.Cmd { return nil }

// episodes returns the sorted unique episode labels.
func (s *offlineSession) episodes() []string {
	set := map[string]struct{}{}
	for _, e := range s.title.Snapshot.Entries {
		set[e.EpisodeNum] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sortByEpisodeKey(out)
	return out
}

// sortByEpisodeKey sorts episode labels numerically with junk last.
func sortByEpisodeKey(labels []string) {
	for i := 1; i < len(labels); i++ {
		for j := i; j > 0; j-- {
			ki, kj := EpisodeSortKey(labels[j]), EpisodeSortKey(labels[j-1])
			if ki < kj || (ki == kj && labels[j] < labels[j-1]) {
				labels[j], labels[j-1] = labels[j-1], labels[j]
			} else {
				break
			}
		}
	}
}

// variants returns the entries of the current episode.
func (s *offlineSession) variants() []download.Entry {
	var out []download.Entry
	for _, e := range s.title.Snapshot.Entries {
		if e.EpisodeNum == s.current {
			out = append(out, e)
		}
	}
	return out
}

// buildEpisodeList renders the episode picker with variant counts.
func (s *offlineSession) buildEpisodeList() {
	eps := s.episodes()
	choices := make([]Choice, 0, len(eps))
	for _, num := range eps {
		choices = append(choices, Choice{
			ID:    num,
			Label: "Эп. " + num,
			Value: num,
		})
	}
	s.episodeList = NewPinList(NewMenu("Выберите серию:", "Локальные серии не найдены", choices...), defaultListHeight)
	if len(choices) > 0 {
		s.episodeList.Jump(1)
	}
}

// buildActionMenu renders the offline action set (python
// _offline_session menu).
func (s *offlineSession) buildActionMenu() {
	s.list = NewPinList(NewMenu(s.header(), "", []Choice{
		{ID: "watch", Label: "▶ Смотреть"},
		{ID: "next", Label: "⏭ След."},
		{ID: "prev", Label: "⏮ Пред."},
		{ID: "jump", Label: "🔢 Перейти к серии"},
		{ID: "variant", Label: "🎛 Сменить локальный поток"},
		{ID: "exit", Label: "🚪 Назад"},
	}...), defaultListHeight)
}

// header renders the offline session header.
func (s *offlineSession) header() string {
	variants := s.variants()
	vLabel := "—"
	if len(variants) > 0 {
		best := ResolveVariant(variants, "", "", 0)
		if best != nil {
			vLabel = stripProviderTag(best.VideoKey) + " · " + intToStr(best.Quality) + "p"
		}
	}
	return "📂 " + s.title.Name + " | Эп. " + s.current + " | Вариантов: " + intToStr(len(variants)) + " | " + vLabel
}

// Update implements Screen.
func (s *offlineSession) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	if IsCancelKey(key) {
		return s, pop()
	}

	// Two surfaces: episode picker then action menu.
	if s.current == "" {
		if s.episodeList.HandleKey(key) {
			return s, nil
		}
		resolved := ResolveKey(s.episodeList.Menu(), s.episodeList.Cursor(), key)
		if resolved == nil {
			return s, nil
		}
		if resolved == Back {
			return s, pop()
		}
		s.current, _ = resolved.(string)
		s.buildActionMenu()
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
	id, _ := resolved.(string)
	eps := s.episodes()
	switch id {
	case "watch":
		return s.watch()
	case "next":
		s.shiftEpisode(1, eps)
	case "prev":
		s.shiftEpisode(-1, eps)
	case "jump":
		s.current = ""
		return s, nil
	case "variant":
		return s.pickVariant()
	case "exit":
		return s, pop()
	default:
		return s, nil
	}
	s.buildActionMenu()
	return s, nil
}

// shiftEpisode moves the current episode with clamping.
func (s *offlineSession) shiftEpisode(delta int, eps []string) {
	for i, e := range eps {
		if e == s.current {
			next := i + delta
			if next >= 0 && next < len(eps) {
				s.current = eps[next]
			}
			return
		}
	}
}

// watch resolves the best local variant and plays it offline-marked;
// NO skip resolution ever runs (spec).
func (s *offlineSession) watch() (Screen, tea.Cmd) {
	entries := s.variants()
	if len(entries) == 0 {
		return s, nil
	}
	variant := ResolveVariant(entries, s.videoKey, s.audioKey, s.quality)
	if variant == nil {
		variant = &entries[0]
	}
	s.videoKey, s.audioKey, s.quality = variant.VideoKey, variant.AudioKey, variant.Quality
	path := s.title.Dir + "/" + variant.RelativePath
	title := offlineTitle(s.videoKey, s.audioKey, s.title.Name, s.current)
	return s, func() tea.Msg {
		return offlinePlayMsg{path: path, title: title}
	}
}

// pickVariant offers the local variants of the current episode.
func (s *offlineSession) pickVariant() (Screen, tea.Cmd) {
	entries := s.variants()
	choices := make([]Choice, 0, len(entries))
	for i, e := range entries {
		choices = append(choices, Choice{
			ID:    intToStr(i),
			Label: "V: " + e.VideoKey + " | A: " + e.AudioKey + " | " + intToStr(e.Quality) + "p",
			Value: e,
		})
	}
	s.variantList = NewPinList(NewMenu("Локальные варианты:", "Нет вариантов", choices...), defaultListHeight)
	s.stateVariant = true
	return s, nil
}

// View implements Screen.
func (s *offlineSession) View() tea.View {
	if s.current == "" {
		return tea.NewView(s.episodeList.Render())
	}
	if s.stateVariant && s.variantList != nil {
		return tea.NewView(s.variantList.Render())
	}
	return tea.NewView(theme.Title.Render(s.header()) + "\n" + s.list.Render())
}

// offlineTitle composes the [OFFLINE] window title.
func offlineTitle(videoKey, audioKey, title, episode string) string {
	return "[" + stripProviderTag(videoKey) + " - " + stripProviderTag(audioKey) + "] " +
		title + " - " + episode + " [OFFLINE]"
}

// ResolveVariant picks the best local variant (python _resolve_variant
// port): exact (video, audio, quality) match, then (video, audio) at
// the best quality, then simply the best quality.
func ResolveVariant(entries []download.Entry, videoKey, audioKey string, quality int) *download.Entry {
	if videoKey != "" && audioKey != "" && quality > 0 {
		for i := range entries {
			e := &entries[i]
			if e.VideoKey == videoKey && e.AudioKey == audioKey && e.Quality == quality {
				return e
			}
		}
	}
	if videoKey != "" && audioKey != "" {
		best := -1
		for i := range entries {
			e := &entries[i]
			if e.VideoKey != videoKey || e.AudioKey != audioKey {
				continue
			}
			if best < 0 || e.Quality > entries[best].Quality {
				best = i
			}
		}
		if best >= 0 {
			return &entries[best]
		}
	}
	best := 0
	for i := range entries {
		if entries[i].Quality > entries[best].Quality {
			best = i
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return &entries[best]
}
