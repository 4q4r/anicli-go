package tui

import (
	"context"
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Offline flow screen ids.
const (
	offlineTitlesID  = "offline-titles"
	offlineSessionID = "offline-session"
)

// offlinePlayMsg carries one resolved local playback: the episode
// label and variant keys ride along so the settle records the PLAYED
// state, not whatever the session switched to meanwhile (review m7).
type offlinePlayMsg struct {
	path     string
	title    string
	episode  string
	videoKey string
	audioKey string
}

// offlinePlayedMsg settles one offline playback (C3), carrying the
// played episode label and variant keys into the progress save.
type offlinePlayedMsg struct {
	err      error
	episode  string
	videoKey string
	audioKey string
}

// offlineProgressMsg settles one offline progress round: the local
// save verdict plus the (optional) Shikimori sync verdict line.
type offlineProgressMsg struct {
	saveErr error
	// note is the sync verdict ("" when unbound or nothing to say).
	note string
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
		emptyMsg = i18n.T("offline.no_titles")
	}
	choices := make([]Choice, 0, len(titles))
	for i := range titles {
		t := titles[i]
		episodes := make(map[string]struct{})
		for _, e := range t.Snapshot.Entries {
			episodes[e.EpisodeNum] = struct{}{}
		}
		choices = append(choices, Choice{
			ID: t.Name,
			Label: i18n.T("offline.title_entry", i18n.Vals{
				"name": t.Name, "eps": strconv.Itoa(len(episodes)),
				"variants": strconv.Itoa(t.Snapshot.TotalDownloaded())}),
			Value: &titles[i],
		})
	}
	return NewMenuScreen(MenuScreenConfig{
		ID:       offlineTitlesID,
		Title:    i18n.T("offline.titles_title"),
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
	// episodeFilter is the PR78 type-to-search over the episode
	// picker (the download file list).
	episodeFilter listFilter
	variantList   *PinList // local variant picker
	// status scopes the transient verdict line to the surface that
	// set it (PR64 #1) — see surfaceStatus; bumpSurface runs on every
	// surface switch (episode picker ⇄ menu ⇄ variant picker).
	surfaceStatus
}

// NewOfflineSession builds the offline session for one title.
//
//nolint:revive // internal screen type
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

// sortByEpisodeKey sorts episode labels numerically; junk labels key
// 0.0 and therefore sort FIRST (python bug-compatibility, see
// group_test).
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

// buildEpisodeList renders the episode picker with variant counts. The
// PR78 type-to-search query narrows it live.
func (s *offlineSession) buildEpisodeList() {
	eps := s.episodes()
	prev := cursorID(s.episodeList)
	choices := make([]Choice, 0, len(eps))
	for _, num := range eps {
		choices = append(choices, Choice{
			ID:    num,
			Label: i18n.T("offline.ep_label", i18n.Vals{"num": num}),
			Value: num,
		})
	}
	choices = filterChoices(choices, s.episodeFilter.value())
	s.episodeList = NewPinList(NewMenu(i18n.T("common.pick_episode"), i18n.T("offline.no_local_episodes"), choices...), defaultListHeight)
	restoreCursor(s.episodeList, prev)
}

// buildActionMenu renders the offline action set (python
// _offline_session menu). The trailing Back row appended by NewMenu is
// the menu's exit path (PR24 bottom pin). Every offline session keeps
// the ✂️ binding-editor row (review M3): a fully-bound folder renders
// ONLY range titles, so each range title is an edit path — without the
// row the owner could never edit/rebind/delete ranges again.
func (s *offlineSession) buildActionMenu() {
	choices := []Choice{
		{ID: "watch", Label: i18n.T("menu.watch")},
		{ID: "next", Label: i18n.T("common.next_ep")},
		{ID: "prev", Label: i18n.T("common.prev_ep")},
		{ID: "jump", Label: i18n.T("common.jump_ep")},
		{ID: "variant", Label: i18n.T("offline.switch_variant")},
		{ID: "bind", Label: i18n.T("offline.ranges.open")},
	}
	s.list = NewPinList(NewMenu(s.header(), "", choices...), defaultListHeight)
}

// header renders the offline session header with the active variant.
func (s *offlineSession) header() string {
	variants := s.variants()
	vLabel := "—"
	if best := ResolveVariant(variants, s.videoKey, s.audioKey, s.quality); best != nil {
		vLabel = stripProviderTag(best.VideoKey) + " · " + strconv.Itoa(best.Quality) + "p"
	}
	return i18n.T("offline.header", i18n.Vals{
		"name": s.title.Name, "ep": s.current,
		"count": strconv.Itoa(len(variants)), "variant": vLabel})
}

// Update implements Screen: settles playback messages (C3) and
// routes key presses by surface.
func (s *offlineSession) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case offlinePlayMsg:
		return s.playLocal(m)
	case offlinePlayedMsg:
		if m.err != nil {
			s.setStatus(i18n.T("common.play_failed", i18n.Vals{"err": m.err.Error()}))
			return s, nil
		}
		s.setStatus(i18n.T("common.play_done"))
		// The played state rides the message: two overlapping plays
		// each settle their own episode (review m7).
		return s, s.saveRangeProgressFor(m.episode, m.videoKey, m.audioKey)
	case offlineProgressMsg:
		if m.saveErr != nil {
			s.setStatus(i18n.T("offline.progress_failed", i18n.Vals{"err": m.saveErr.Error()}))
			return s, nil
		}
		status := i18n.T("common.play_done")
		if m.note != "" {
			status += " · " + m.note
		}
		s.setStatus(status)
		return s, nil
	case tea.KeyPressMsg:
		return s.handleKey(m)
	default:
		return s, nil
	}
}

// handleKey routes key presses: the variant picker (when engaged)
// consumes keys first (C4); Esc inside it cancels the picker instead
// of leaving the session.
func (s *offlineSession) handleKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.stateVariant && s.current != "" {
		return s.handleVariantKey(key)
	}
	if IsCancelKey(key) {
		// PR78 type-to-search: an engaged episode-filter clears
		// first — only the NEXT Esc pops the session.
		if s.current == "" && s.episodeFilter.active() {
			s.episodeFilter.clear()
			s.buildEpisodeList()
			return s, nil
		}
		return s, pop()
	}

	// Two surfaces: episode picker then action menu.
	if s.current == "" {
		// PR78 type-to-search: printable keys narrow the list live;
		// the first Esc clears, the second falls through to Back.
		if consumed, changed := s.episodeFilter.consume(key, pinListBoundRunes); consumed {
			if changed {
				s.buildEpisodeList()
			}
			return s, nil
		}
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
		s.bumpSurface() // picker → menu (PR64 #1)
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
		s.stateVariant = false // reset the picker on exit paths (C4)
		s.bumpSurface()        // menu → picker (PR64 #1)
		return s, nil
	case "variant":
		return s.pickVariant()
	case "bind":
		return s, push(NewBindEditor(s.deps, s.title))
	default:
		return s, nil
	}
	s.buildActionMenu()
	return s, nil
}

// handleVariantKey drives the local variant picker (C4): Enter
// applies the variant and returns to the menu; Esc/Back cancels.
func (s *offlineSession) handleVariantKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.variantList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.variantList.Menu(), s.variantList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	s.stateVariant = false
	s.bumpSurface() // picker ⇄ menu (PR64 #1)
	if resolved == Back {
		return s, nil
	}
	entry, ok := resolved.(download.Entry)
	if !ok {
		return s, nil
	}
	s.videoKey, s.audioKey, s.quality = entry.VideoKey, entry.AudioKey, entry.Quality
	s.buildActionMenu()
	s.setStatus(i18n.T("offline.switched"))
	return s, nil
}

// playLocal runs the resolved local file through the playback
// service (C3): [OFFLINE] title, no skip lookups ever. The played
// episode and variant keys ride the messages into the settle.
func (s *offlineSession) playLocal(m offlinePlayMsg) (Screen, tea.Cmd) {
	if s.deps == nil || s.deps.Playback == nil {
		s.setStatus(i18n.T("offline.player_unavailable"))
		return s, nil
	}
	deps := s.deps
	return s, safeCmd(offlineSessionID, func() tea.Msg {
		err := deps.Playback.Play(context.Background(), PlayRequest{
			URL:        m.path,
			Title:      m.title,
			AnimeTitle: s.title.Name,
			EpisodeNum: m.episode,
		})
		return offlinePlayedMsg{err: err, episode: m.episode, videoKey: m.videoKey, audioKey: m.audioKey}
	})
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

// saveRangeProgress schedules the per-range progress round after a
// successful offline playback: one bounded source-only save onto the
// range's synthetic anime_progress key, then — for a bound range — the
// episode push (`position + episode_offset`) with the PRE-SAVE local
// counter as the no-rollback prior. Plain folders (Range nil) keep
// today's write-nothing behavior; unsorted limbo titles likewise.
func (s *offlineSession) saveRangeProgress() tea.Cmd {
	return s.saveRangeProgressFor(s.current, s.videoKey, s.audioKey)
}

// saveRangeProgressFor is saveRangeProgress with the explicitly played
// episode label and variant keys (review m7): the settle writes the
// played state even after the session moved on.
func (s *offlineSession) saveRangeProgressFor(episode, videoKey, audioKey string) tea.Cmd {
	if s.deps == nil || s.deps.History == nil {
		return nil
	}
	if s.title.Range == nil || episode == "" {
		return nil
	}
	deps := s.deps
	shikiID := s.title.Range.ShikimoriID
	offset := s.title.Range.EpisodeOffset
	position := episode
	title := s.title.Name
	relDir := s.title.RelDir
	root := s.title.Root
	rangeID := s.title.Range.ID
	shikiTitle := s.title.Range.ShikimoriTitle
	return safeCmd(offlineSessionID, func() tea.Msg {
		rec := storage.AnimeProgress{
			Title:          title,
			SourceID:       "offline",
			SourceURL:      download.SyntheticSourceURL(root, relDir, rangeID),
			CurrentEpisode: position,
			UpdatedAt:      nowUTC(),
		}
		if shikiID != 0 {
			id := shikiID
			rec.ShikimoriID = &id
			if shikiTitle != "" {
				st := shikiTitle
				rec.ShikimoriTitle = &st
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
		defer cancel()
		// The no-rollback prior must be read BEFORE the save
		// overwrites the row with the new position label (review M5):
		// converted to entry units it guards the push below. The prior
		// comes from the range's OWN row (the synthetic source key) —
		// a shikimori-id lookup returns the newest row of ANY source
		// sharing the entry, so a newer online row (or a foreign
		// folder's range) mixed its global episode into this range's
		// unit space and blocked legitimate pushes (review round 2).
		// hasPrior stays true for every bound range: prior 0 (no own
		// row — first watch) must own the guard instead of falling
		// back to the newest-by-shikimori-id row read, whose units are
		// whatever that foreign row stores.
		priorEntry := 0
		hasPrior := shikiID != 0
		if hasPrior {
			if own, err := deps.History.GetBySource(ctx, "offline", download.SyntheticSourceURL(root, relDir, rangeID)); err == nil && own != nil {
				priorEntry = parseWatchEpisode(own.CurrentEpisode) + offset
			}
		}
		// Source-only upsert (review M4): the shikimori-id fallback
		// would move/retire an unrelated ONLINE row sharing the id.
		err := deps.History.SavePlaybackSourceOnly(ctx, rec, position, videoKey, audioKey)
		if err != nil {
			return offlineProgressMsg{saveErr: err}
		}
		note := ""
		if shikiID != 0 {
			// Entry units: the Shikimori counter is the position plus
			// the range offset; the stored row keeps the position
			// label — the units never mix (Review Focus #2).
			episodeNum := parseWatchEpisode(position) + offset
			syncCtx, syncCancel := context.WithTimeout(context.Background(), shikiSyncTimeout)
			defer syncCancel()
			if deps.ProgressSyncPrior != nil {
				note = renderSyncReport(deps, deps.ProgressSyncPrior(syncCtx, shikiID, episodeNum, priorEntry, hasPrior), episodeNum)
			} else {
				note = syncWatchProgressPrior(syncCtx, deps, shikiID, episodeNum, priorEntry, hasPrior).note
			}
		}
		return offlineProgressMsg{note: note}
	})
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
	played := s.current
	videoKey, audioKey := s.videoKey, s.audioKey
	return s, func() tea.Msg {
		return offlinePlayMsg{
			path:     path,
			title:    title,
			episode:  played,
			videoKey: videoKey,
			audioKey: audioKey,
		}
	}
}

// pickVariant offers the local variants of the current episode. The
// pseudo-entry of a hand-placed file renders its honest «файл» label
// instead of the raw "V: file | A: — | 0p".
func (s *offlineSession) pickVariant() (Screen, tea.Cmd) {
	entries := s.variants()
	choices := make([]Choice, 0, len(entries))
	for i, e := range entries {
		label := "V: " + e.VideoKey + " | A: " + e.AudioKey + " | " + strconv.Itoa(e.Quality) + "p"
		if e.VideoKey == "file" {
			label = i18n.T("offline.file_variant")
		}
		choices = append(choices, Choice{
			ID:    strconv.Itoa(i),
			Label: label,
			Value: e,
		})
	}
	s.variantList = NewPinList(NewMenu(i18n.T("offline.variants_title"), i18n.T("offline.no_variants"), choices...), defaultListHeight)
	s.stateVariant = true
	s.bumpSurface() // menu → picker (PR64 #1)
	return s, nil
}

// View implements Screen.
func (s *offlineSession) View() tea.View {
	var body string
	switch {
	case s.current == "":
		body = filterLineAbove(s.episodeFilter, themedList(s.episodeList))
	case s.stateVariant && s.variantList != nil:
		body = themedList(s.variantList)
	default:
		body = theme.Title.Render(s.header()) + "\n\n" + s.list.Render()
	}
	if s.statusVisible() {
		body += "\n" + theme.StatusLine.Render(s.status)
	}
	return tea.NewView(body)
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
