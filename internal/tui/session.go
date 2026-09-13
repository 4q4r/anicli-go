package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Session substates (python session_loop's inline prompts).
type sessionState string

const (
	sessionStateLoading       sessionState = "loading"
	sessionStateMenu          sessionState = "menu"
	sessionStateEpisodeList   sessionState = "episodes"
	sessionStateDubVideo      sessionState = "dub_video"
	sessionStateDubAudio      sessionState = "dub_audio"
	sessionStateQuality       sessionState = "quality"
	sessionStatePlaying       sessionState = "playing"
	sessionStateInfoMenu      sessionState = "info_menu"
	sessionStateInfoStatus    sessionState = "info_status"
	sessionStateInfoScore     sessionState = "info_score"
	sessionStateInfoRewatches sessionState = "info_rewatches"
	sessionStateDownloadRange sessionState = "download_range"
	sessionStateDownloadMode  sessionState = "download_mode"
)

// sessionScreenID is the session screen identity.
const sessionScreenID = "session"

// lookupTimeout bounds provider episode/stream lookups launched from
// session commands; the mpv lifetime itself is governed by the player.
const lookupTimeout = 30 * time.Second

// statWriteTimeout bounds progress writes (mirrors the registry's
// stat-write cap).
const statWriteTimeout = 5 * time.Second

// RU status labels (python RUSSIAN_STATUSES).
var ruStatuses = []struct{ Key, Label string }{
	{"watching", "Смотрю"},
	{"planned", "В планах"},
	{"rewatching", "Пересмотриваю"},
	{"completed", "Просмотрено"},
	{"on_hold", "Отложено"},
	{"dropped", "Брошено"},
}

// episodePartMsg settles one source's episode fetch.
type episodePartMsg struct {
	sourceID string
	episodes []contracts.Episode
	err      error
}

// episodesDoneMsg finalizes the merge after every source settled.
type episodesDoneMsg struct{}

// streamResolvedMsg carries the resolved video links for the quality
// picker.
type streamResolvedMsg struct {
	links map[string]contracts.VideoSource
	err   error
}

// playedMsg settles one playback.
type playedMsg struct{ err error }

// sessionScreen is the watch-session state machine: a merged episode
// aggregate over the grouped sources with the python session_loop
// action set (watch / next / prev / jump / redub / info / download /
// exit→root).
type sessionScreen struct {
	deps    *Deps
	primary contracts.SearchResult
	group   []contracts.SearchResult

	// merged state
	parts    []SourceEpisodes
	episodes map[string]contracts.Episode
	order    []string
	dubStats map[string]int
	pending  map[string]bool

	currentIdx  int
	videoDub    string
	audioDub    string
	lastQuality string

	state sessionState

	list        *PinList // action menu
	episodeList *PinList // «Перейти к серии»
	dubList     *PinList // video/audio dub pickers
	qualityList *PinList
	rangeInput  *TextPrompt // download range + info numeric fields
	infoPrompt  *TextPrompt // score / rewatches entry

	downloadEpisodes []string
	resolvedLinks    map[string]contracts.VideoSource
	localCounts      map[string]int

	status string // transient status line (play verdicts, sync notes)
}

// NewSessionScreen builds the session for one grouped title.
//
//nolint:revive // internal screen type; tests assert on the concrete struct
func NewSessionScreen(deps *Deps, primary contracts.SearchResult, group []contracts.SearchResult) *sessionScreen {
	return &sessionScreen{
		deps:     deps,
		primary:  primary,
		group:    group,
		episodes: map[string]contracts.Episode{},
		pending:  map[string]bool{},
		state:    sessionStateLoading,
	}
}

// ID implements Screen.
func (s *sessionScreen) ID() string { return sessionScreenID }

// Init implements Screen: fetch every source's episodes.
func (s *sessionScreen) Init() tea.Cmd {
	cmds := []tea.Cmd{}
	for _, res := range s.group {
		s.pending[res.SourceID] = true
		cmds = append(cmds, safeCmd(sessionScreenID, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			defer cancel()
			eps, err := s.deps.Episode.GetEpisodes(ctx, res.SourceID, res.URL)
			return episodePartMsg{sourceID: res.SourceID, episodes: eps, err: err}
		}))
	}
	return tea.Batch(cmds...)
}

// loadEpisodesSync is the synchronous loading path for tests and for
// rehydration flows that already hold the parts.
func (s *sessionScreen) loadEpisodesSync() {
	for _, res := range s.group {
		s.pending[res.SourceID] = true
		eps, err := s.deps.Episode.GetEpisodes(context.Background(), res.SourceID, res.URL)
		msg := episodePartMsg{sourceID: res.SourceID, episodes: eps, err: err}
		s.Update(msg)
	}
	s.Update(episodesDoneMsg{})
}

// Update implements Screen: the substate machine.
func (s *sessionScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case episodePartMsg:
		delete(s.pending, msg.sourceID)
		if msg.err == nil && len(msg.episodes) > 0 {
			s.parts = append(s.parts, SourceEpisodes{SourceID: msg.sourceID, Episodes: msg.episodes})
		}
		if len(s.pending) == 0 {
			return s, func() tea.Msg { return episodesDoneMsg{} }
		}
		return s, nil
	case episodesDoneMsg:
		s.finalizeMerge()
		return s, nil
	case streamResolvedMsg:
		if msg.err != nil {
			s.status = "Ошибка: " + msg.err.Error()
			s.state = sessionStateMenu
			return s, nil
		}
		s.resolvedLinks = msg.links
		s.buildQualityList()
		return s, nil
	case playedMsg:
		s.state = sessionStateMenu
		if msg.err != nil {
			s.status = "Ошибка воспроизведения: " + msg.err.Error()
			return s, nil
		}
		s.status = "Воспроизведение завершено"
		s.saveProgress()
		return s, s.maybeNext()
	case tea.KeyPressMsg:
		return s.handleKey(msg)
	default:
		return s, nil
	}
}

// handleKey routes key presses by substate.
func (s *sessionScreen) handleKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if IsCancelKey(key) {
		return s.handleCancel()
	}

	switch s.state {
	case sessionStateLoading:
		return s, nil
	case sessionStateMenu:
		return s.handleMenuKey(key)
	case sessionStateEpisodeList:
		return s.handleEpisodeListKey(key)
	case sessionStateDubVideo, sessionStateDubAudio:
		return s.handleDubKey(key)
	case sessionStateQuality:
		return s.handleQualityKey(key)
	case sessionStateInfoMenu:
		return s.handleInfoMenuKey(key)
	case sessionStateInfoStatus:
		return s.handleInfoStatusKey(key)
	case sessionStateInfoScore, sessionStateInfoRewatches:
		return s.handleInfoNumericKey(key)
	case sessionStateDownloadRange:
		return s.handleDownloadRangeKey(key)
	case sessionStateDownloadMode:
		return s.handleDownloadModeKey(key)
	case sessionStatePlaying:
		return s, nil
	default:
		return s, nil
	}
}

// handleCancel applies I2 per state: substates return to the session
// menu; the menu itself pops (root via «Выход»).
func (s *sessionScreen) handleCancel() (Screen, tea.Cmd) {
	switch s.state {
	case sessionStateMenu, sessionStatePlaying:
		return s, pop()
	case sessionStateLoading:
		return s, pop()
	default:
		s.state = sessionStateMenu
		return s, nil
	}
}

// handleMenuKey drives the action menu.
func (s *sessionScreen) handleMenuKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
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
	switch id {
	case "watch":
		return s.startWatch()
	case "next":
		if s.currentIdx < len(s.order)-1 {
			s.currentIdx++
		}
		return s, nil
	case "prev":
		if s.currentIdx > 0 {
			s.currentIdx--
		}
		return s, nil
	case "jump":
		s.state = sessionStateEpisodeList
		return s, nil
	case "redub":
		s.videoDub, s.audioDub = "", ""
		s.status = "Озвучка сброшена — выберите заново при просмотре"
		return s, nil
	case "info":
		s.state = sessionStateInfoMenu
		return s, nil
	case "download":
		if s.deps == nil || s.deps.Download == nil {
			s.status = "Загрузка недоступна"
			return s, nil
		}
		s.state = sessionStateDownloadRange
		s.rangeInput = NewTextPrompt(TextPromptConfig{
			ID:    "download-range",
			Title: "Серии для загрузки (например 1-5, 7):",
		})
		return s, s.rangeInput.Init()
	case "exit":
		return s, popToRoot()
	default:
		return s, nil
	}
}

// startWatch launches the watch pipeline: interactive dub selection
// when preferences are missing or unavailable, else straight to
// stream resolution.
func (s *sessionScreen) startWatch() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	if ep == nil {
		s.status = "Нет серий"
		return s, nil
	}
	if s.videoDub == "" || ep.RawEmbeds[s.videoDub] == nil {
		return s.openDubSelect(sessionStateDubVideo)
	}
	if (s.audioDub == "" || ep.RawEmbeds[s.audioDub] == nil) && s.audioDub != s.videoDub {
		return s.openDubSelect(sessionStateDubAudio)
	}
	return s.beginStreamResolve()
}

// openDubSelect builds the picker for the current state.
func (s *sessionScreen) openDubSelect(state sessionState) (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	keys := sortedEmbedKeys(ep.RawEmbeds)
	choices := make([]Choice, 0, len(keys)+1)
	if state == sessionStateDubAudio && s.videoDub != "" {
		choices = append(choices, Choice{
			ID:    s.videoDub,
			Label: "⭐ Как видео (" + stripProviderTag(s.videoDub) + ")",
			Value: s.videoDub,
		})
	}
	for _, k := range keys {
		if state == sessionStateDubAudio && k == s.videoDub {
			continue
		}
		choices = append(choices, Choice{
			ID:    k,
			Label: fmt.Sprintf("%s [%d сер.]", k, s.dubStats[k]),
			Value: k,
		})
	}
	title := "Выберите видеопоток:"
	if state == sessionStateDubAudio {
		title = "Выберите аудиопоток:"
	}
	s.state = state
	s.dubList = NewPinList(NewMenu(title, "Нет доступных потоков", choices...), defaultListHeight)
	if len(choices) > 0 {
		s.dubList.Jump(1)
	}
	return s, nil
}

// handleDubKey resolves the dub pickers.
func (s *sessionScreen) handleDubKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.dubList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.dubList.Menu(), s.dubList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		// Back from video select returns to the menu; from audio
		// select back to the video select (python loop).
		if s.state == sessionStateDubAudio && s.videoDub != "" {
			return s.openDubSelect(sessionStateDubVideo)
		}
		s.state = sessionStateMenu
		return s, nil
	}
	pick, _ := resolved.(string)
	if s.state == sessionStateDubVideo {
		s.videoDub = pick
		return s.openDubSelect(sessionStateDubAudio)
	}
	s.audioDub = pick
	if s.videoDub != "" && s.audioDub == "" {
		s.audioDub = s.videoDub
	}
	return s.beginStreamResolve()
}

// beginStreamResolve resolves the video stream for the quality picker.
func (s *sessionScreen) beginStreamResolve() (Screen, tea.Cmd) {
	s.state = sessionStateQuality
	s.buildQualityList()
	dub := s.videoDub
	ep := *s.currentEpisodeData()
	provider := providerOfTrackKey(dub)
	deps := s.deps
	return s, safeCmd(sessionScreenID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		stream, err := deps.Episode.ResolveStream(ctx, provider, ep, dub)
		if err != nil {
			return streamResolvedMsg{err: err}
		}
		return streamResolvedMsg{links: stream.Links}
	})
}

// buildQualityList renders the picker from resolved links (or the
// auto option while unresolved).
func (s *sessionScreen) buildQualityList() {
	choices := []Choice{{ID: "auto", Label: "Авто (лучшее качество)", Value: "auto"}}
	qualities := sortedQualityDesc(s.resolvedLinks)
	for _, q := range qualities {
		choices = append(choices, Choice{ID: q, Label: q + "p", Value: q})
	}
	s.qualityList = NewPinList(NewMenu("Выберите качество:", "", choices...), defaultListHeight)
	s.qualityList.Jump(1)
}

// handleQualityKey resolves the quality pick and launches playback.
func (s *sessionScreen) handleQualityKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.qualityList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.qualityList.Menu(), s.qualityList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.state = sessionStateMenu
		return s, nil
	}
	choice, _ := resolved.(string)
	s.state = sessionStatePlaying
	s.status = "▶ Запуск mpv…"
	return s, s.playCmd(choice)
}

// playCmd resolves dubs/skips and runs the player; it settles into
// playedMsg.
func (s *sessionScreen) playCmd(qualityChoice string) tea.Cmd {
	snapshot := *s
	return func() tea.Msg {
		msg := snapshot.doPlay(qualityChoice)
		return msg
	}
}

// doPlay performs the synchronous play pipeline.
func (s *sessionScreen) doPlay(qualityChoice string) tea.Msg {
	ctx := context.Background()
	ep := s.currentEpisodeData()

	video, err := s.pickVideo(ctx, ep, qualityChoice)
	if err != nil {
		return playedMsg{err: err}
	}

	audioURL := ""
	audio, err := s.pickAudio(ctx, ep)
	if err != nil {
		return playedMsg{err: err}
	}
	if audio != nil {
		audioURL = audio.URL
	}

	chapters := ""
	if s.deps != nil && s.deps.Playback != nil {
		var cleanup func()
		chapters, cleanup, err = s.deps.Playback.ResolveSkips(ctx, s.shikimoriID(), EpisodeSortKey(ep.Num))
		if err == nil && cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			// Skips are best-effort: play without chapters.
			chapters = ""
		}
	}

	title := fmt.Sprintf("[%s - %s] %s - %s",
		stripProviderTag(s.videoDub), stripProviderTag(s.audioDub),
		BestDisplayTitle(s.group), ep.Num)

	if s.deps == nil || s.deps.Playback == nil {
		return playedMsg{err: errNoPlayback}
	}
	err = s.deps.Playback.Play(ctx, PlayRequest{
		URL:          video.URL,
		AudioURL:     audioURL,
		Title:        title,
		Headers:      video.Headers,
		ExtraMPVOpts: video.ExtraMPVOpts,
		ChaptersFile: chapters,
	})
	if err == nil {
		if q := qualityChoice; q != "auto" {
			s.lastQuality = q
		} else if video.Quality != "" {
			s.lastQuality = video.Quality
		}
	}
	return playedMsg{err: err}
}

// errNoPlayback reports an unwired playback service.
var errNoPlayback = fmt.Errorf("tui: playback service unavailable")

// pickVideo resolves the video variant for the chosen quality.
func (s *sessionScreen) pickVideo(ctx context.Context, ep *contracts.Episode, qualityChoice string) (*contracts.VideoSource, error) {
	links := s.resolvedLinks
	if links == nil {
		stream, err := s.deps.Episode.ResolveStream(ctx, providerOfTrackKey(s.videoDub), *ep, s.videoDub)
		if err != nil {
			return nil, fmt.Errorf("видео %s: %w", s.videoDub, err)
		}
		links = stream.Links
	}
	quality := qualityChoice
	if quality == "auto" || quality == "" {
		if s.lastQuality != "" {
			if _, ok := links[s.lastQuality]; ok {
				quality = s.lastQuality
			}
		}
		if quality == "auto" || quality == "" {
			qualities := sortedQualityDesc(links)
			if len(qualities) == 0 {
				return nil, fmt.Errorf("нет потоков у %s", s.videoDub)
			}
			quality = qualities[0]
		}
	}
	src, ok := links[quality]
	if !ok {
		return nil, fmt.Errorf("качество %s недоступно", quality)
	}
	return &src, nil
}

// pickAudio resolves the separate audio track when the dubs differ.
func (s *sessionScreen) pickAudio(ctx context.Context, ep *contracts.Episode) (*contracts.VideoSource, error) {
	if s.audioDub == "" || s.audioDub == s.videoDub {
		return nil, nil
	}
	stream, err := s.deps.Episode.ResolveStream(ctx, providerOfTrackKey(s.audioDub), *ep, s.audioDub)
	if err != nil {
		return nil, fmt.Errorf("аудио %s: %w", s.audioDub, err)
	}
	qualities := sortedQualityDesc(stream.Links)
	if len(qualities) == 0 {
		return nil, fmt.Errorf("нет аудиопотоков у %s", s.audioDub)
	}
	src := stream.Links[qualities[0]]
	return &src, nil
}

// shikimoriID extracts the shikimori binding from primary metadata.
func (s *sessionScreen) shikimoriID() int64 {
	if s.primary.Meta == nil {
		return 0
	}
	switch v := s.primary.Meta["shikimori_id"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// maybeNext offers nothing automatic (python stays on the same
// episode after playback).
func (s *sessionScreen) maybeNext() tea.Cmd { return nil }

// savePlayback persists progress through the history service.
func (s *sessionScreen) saveProgress() {
	if s.deps == nil || s.deps.History == nil {
		return
	}
	ep := s.currentEpisodeData()
	if ep == nil {
		return
	}
	rec := storage.AnimeProgress{
		Title:          BestDisplayTitle(s.group),
		SourceID:       s.primary.SourceID,
		SourceURL:      s.primary.URL,
		CurrentEpisode: ep.Num,
		ShikimoriID:    nil,
		UpdatedAt:      nowUTC(),
	}
	if id := s.shikimoriID(); id != 0 {
		rec.ShikimoriID = &id
	}
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	if err := s.deps.History.SavePlayback(ctx, rec, ep.Num, s.videoDub, s.audioDub); err != nil {
		s.status = "Не удалось сохранить прогресс: " + err.Error()
	}
}

// handleEpisodeListKey drives «Перейти к серии».
func (s *sessionScreen) handleEpisodeListKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.episodeList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.episodeList.Menu(), s.episodeList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.state = sessionStateMenu
		return s, nil
	}
	num, _ := resolved.(string)
	for i, n := range s.order {
		if n == num {
			s.currentIdx = i
			break
		}
	}
	s.state = sessionStateMenu
	return s, nil
}

// handleInfoMenuKey drives the «Изменить инфо» submenu.
func (s *sessionScreen) handleInfoMenuKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	menu := s.infoMenu()
	if menu.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(menu.list.Menu(), menu.list.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.state = sessionStateMenu
		return s, nil
	}
	id, _ := resolved.(string)
	switch id {
	case "status":
		s.state = sessionStateInfoStatus
	case "score":
		s.state = sessionStateInfoScore
		s.infoPrompt = NewTextPrompt(TextPromptConfig{ID: "info-score", Title: "Введите оценку (0-10):"})
		return s, s.infoPrompt.Init()
	case "rewatches":
		s.state = sessionStateInfoRewatches
		s.infoPrompt = NewTextPrompt(TextPromptConfig{ID: "info-rew", Title: "Количество пересмотров:"})
		return s, s.infoPrompt.Init()
	}
	return s, nil
}

// infoMenu builds the info submenu view model.
func (s *sessionScreen) infoMenu() *MenuScreen {
	return NewMenuScreen(MenuScreenConfig{
		ID:    "session-info",
		Title: "Что изменить?",
		Choices: []Choice{
			{ID: "status", Label: "Статус"},
			{ID: "score", Label: "Оценка"},
			{ID: "rewatches", Label: "Пересмотры"},
		},
	})
}

// handleInfoStatusKey drives the RU status picker.
func (s *sessionScreen) handleInfoStatusKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	menu := s.statusMenu()
	if menu.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(menu.list.Menu(), menu.list.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.state = sessionStateInfoMenu
		return s, nil
	}
	statusKey, _ := resolved.(string)
	s.state = sessionStateMenu
	return s, s.pushShikiUpdate(statusKey, nil, nil)
}

// statusMenu builds the RU status picker.
func (s *sessionScreen) statusMenu() *MenuScreen {
	choices := make([]Choice, 0, len(ruStatuses))
	for _, st := range ruStatuses {
		choices = append(choices, Choice{ID: st.Key, Label: st.Label, Value: st.Key})
	}
	return NewMenuScreen(MenuScreenConfig{ID: "session-status", Title: "Выберите статус:", Choices: choices})
}

// handleInfoNumericKey submits score/rewatches.
func (s *sessionScreen) handleInfoNumericKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	next, cmd := s.infoPrompt.Update(key)
	s.infoPrompt = next.(*TextPrompt)
	resolved := ResolveText(key, s.infoPrompt.Value())
	if resolved == nil {
		return s, cmd
	}
	if resolved == Back {
		s.state = sessionStateInfoMenu
		return s, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(resolved.(string)))
	if err != nil {
		s.status = "Нужно число"
		return s, nil
	}
	v := value
	s.state = sessionStateMenu
	if s.state == sessionStateInfoScore {
		return s, s.pushShikiUpdate("", &v, nil)
	}
	return s, s.pushShikiUpdate("", nil, &v)
}

// pushShikiUpdate applies the manual status patch (python
// update_shikimori_status_manual port).
func (s *sessionScreen) pushShikiUpdate(status string, score, rewatches *int) tea.Cmd {
	if s.deps == nil || s.deps.Shiki == nil || !s.deps.Shiki.Enabled() {
		s.status = "Shikimori отключён — обновление только локально невозможно"
		return nil
	}
	id := s.shikimoriID()
	if id == 0 {
		s.status = "Запись не привязана к Shikimori"
		return nil
	}
	deps := s.deps
	return safeCmd(sessionScreenID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		if _, err := deps.Shiki.UpdateStatus(ctx, id, 0, status, score, rewatches); err != nil {
			return playedMsg{err: fmt.Errorf("shikimori: %w", err)}
		}
		return playedMsg{}
	})
}

// handleDownloadRangeKey parses the range and moves to the mode pick.
func (s *sessionScreen) handleDownloadRangeKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	next, cmd := s.rangeInput.Update(key)
	s.rangeInput = next.(*TextPrompt)
	resolved := ResolveText(key, s.rangeInput.Value())
	if resolved == nil {
		return s, cmd
	}
	if resolved == Back {
		s.state = sessionStateMenu
		return s, nil
	}
	spec := resolved.(string)
	nums := ParseRange(spec)
	s.downloadEpisodes = nil
	for _, n := range nums {
		label := strconv.Itoa(n)
		for _, order := range s.order {
			if order == label {
				s.downloadEpisodes = append(s.downloadEpisodes, label)
			}
		}
	}
	if len(s.downloadEpisodes) == 0 {
		s.status = "В диапазоне нет доступных серий"
		s.state = sessionStateMenu
		return s, nil
	}
	s.state = sessionStateDownloadMode
	return s, nil
}

// handleDownloadModeKey dispatches foreground/background downloads.
func (s *sessionScreen) handleDownloadModeKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	menu := s.downloadModeMenu()
	if menu.list.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(menu.list.Menu(), menu.list.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.state = sessionStateMenu
		return s, nil
	}
	mode, _ := resolved.(string)
	s.state = sessionStateMenu
	episodes := make([]contracts.Episode, 0, len(s.downloadEpisodes))
	for _, num := range s.downloadEpisodes {
		episodes = append(episodes, s.episodes[num])
	}
	tasks := make([]DownloadTask, 0, len(episodes))
	for _, ep := range episodes {
		tasks = append(tasks, DownloadTask{
			AnimeTitle:  BestDisplayTitle(s.group),
			EpisodeNum:  ep.Num,
			ShikimoriID: s.shikimoriID(),
			ProviderID:  providerOfTrackKey(s.videoDub),
			Episode:     ep,
			DubID:       s.videoDub,
			Quality:     s.lastQuality,
		})
	}
	switch mode {
	case "foreground":
		cmds := make([]tea.Cmd, 0, len(tasks))
		for _, task := range tasks {
			cmds = append(cmds, safeCmd(sessionScreenID, func() tea.Msg {
				return downloadSettledMsg{err: s.deps.Download.Download(context.Background(), task)}
			}))
		}
		s.status = fmt.Sprintf("Загрузка %d серий (передний план)…", len(tasks))
		return s, tea.Sequence(cmds...)
	case "background":
		for _, task := range tasks {
			s.deps.Download.Submit(task)
		}
		s.status = fmt.Sprintf("Отправлено в фон: %d серий", len(tasks))
		return s, nil
	default:
		return s, nil
	}
}

// downloadSettledMsg reports one finished foreground download.
type downloadSettledMsg struct{ err error }

// downloadModeMenu builds the foreground/background picker.
func (s *sessionScreen) downloadModeMenu() *MenuScreen {
	return NewMenuScreen(MenuScreenConfig{
		ID:    "download-mode",
		Title: "Режим загрузки:",
		Choices: []Choice{
			{ID: "foreground", Label: "⏳ Передний план (последовательно)", Value: "foreground"},
			{ID: "background", Label: "🔁 Фон (продолжить работу)", Value: "background"},
		},
		Status: s.deps.Download.ActiveBanner(),
	})
}

// finalizeMerge assembles the merged aggregate and the action menu.
func (s *sessionScreen) finalizeMerge() {
	merged, order := MergeEpisodeLists(s.parts)
	s.episodes = merged
	s.order = order
	s.dubStats = DubStats(mergeValues(merged))
	s.currentIdx = 0
	s.state = sessionStateMenu
	s.attachLocalCounts()
	s.buildActionMenu()
	s.buildEpisodeList()
}

// buildActionMenu renders the python session_loop choices.
func (s *sessionScreen) buildActionMenu() {
	s.list = NewPinList(NewMenu(s.renderHeader(), "", []Choice{
		{ID: "watch", Label: "▶ Смотреть"},
		{ID: "next", Label: "⏭ След."},
		{ID: "prev", Label: "⏮ Пред."},
		{ID: "jump", Label: "🔢 Перейти к серии"},
		{ID: "redub", Label: "🎨 Сменить озвучку"},
		{ID: "info", Label: "📝 Изменить инфо"},
		{ID: "download", Label: "⬇ Скачать серии"},
		{ID: "exit", Label: "🚪 Выход"},
	}...), defaultListHeight)
}

// buildEpisodeList builds the jump list with local markers.
func (s *sessionScreen) buildEpisodeList() {
	choices := make([]Choice, 0, len(s.order))
	for _, num := range s.order {
		ep := s.episodes[num]
		label := fmt.Sprintf("Серия %s", num)
		if ep.Title != "" {
			label += " — " + ep.Title
		}
		choices = append(choices, Choice{ID: num, Label: label, Value: num})
	}
	s.episodeList = NewPinList(NewMenu("Выберите серию:", "Нет серий", choices...), defaultListHeight)
	for i, num := range s.order {
		if s.localCounts[num] > 0 {
			s.episodeList.SetMarker(i+1, "★")
		}
	}
}

// attachLocalCounts fills per-episode local variant counts from the
// offline index when the title directory matches.
func (s *sessionScreen) attachLocalCounts() {
	if s.deps == nil || s.deps.Offline == nil {
		return
	}
	titles, err := s.deps.Offline.Titles()
	if err != nil {
		return
	}
	s.localCounts = LocalEpisodeCounts(titles, BestDisplayTitle(s.group))
}

// LocalEpisodeCounts matches an offline title by its cleaned
// directory name and maps episode → variant count.
func LocalEpisodeCounts(titles []OfflineTitle, displayTitle string) map[string]int {
	cleaned := download.CleanTitleForFS(displayTitle)
	for _, t := range titles {
		if download.CleanTitleForFS(t.Name) == cleaned {
			counts := make(map[string]int)
			for _, e := range t.Snapshot.Entries {
				counts[e.EpisodeNum]++
			}
			return counts
		}
	}
	return nil
}

// currentEpisode returns the current episode number.
func (s *sessionScreen) currentEpisode() string {
	if s.currentIdx < 0 || s.currentIdx >= len(s.order) {
		return ""
	}
	return s.order[s.currentIdx]
}

// currentEpisodeData returns the merged episode at the cursor.
func (s *sessionScreen) currentEpisodeData() *contracts.Episode {
	num := s.currentEpisode()
	if num == "" {
		return nil
	}
	ep, ok := s.episodes[num]
	if !ok {
		return nil
	}
	return &ep
}

// renderHeader composes the session status header.
func (s *sessionScreen) renderHeader() string {
	ep := s.currentEpisodeData()
	embeds := 0
	if ep != nil {
		embeds = len(ep.RawEmbeds)
	}
	h := fmt.Sprintf("📺 %s | Эп. %s | Ист: %d", BestDisplayTitle(s.group), s.currentEpisode(), embeds)
	if s.videoDub != "" {
		h += fmt.Sprintf(" | 🔊 %s", s.videoDub)
	}
	if total := sumCounts(s.localCounts); total > 0 {
		h += fmt.Sprintf(" | локально: %d", total)
	}
	return h
}

// renderEpisodeList renders the jump list surface, recomputing the
// local-availability stars so late index refreshes show up.
func (s *sessionScreen) renderEpisodeList() string {
	for i, num := range s.order {
		if s.localCounts[num] > 0 {
			s.episodeList.SetMarker(i+1, "★")
		} else {
			s.episodeList.SetMarker(i+1, "")
		}
	}
	return s.episodeList.Render()
}

// renderInfoMenu renders the info submenu surface.
func (s *sessionScreen) renderInfoMenu() string {
	menu := s.infoMenu()
	// reflect the live cursor: cheap rebuild for rendering only
	return menu.View().Content
}

// View implements Screen.
func (s *sessionScreen) View() tea.View {
	var body string
	switch s.state {
	case sessionStateLoading:
		body = theme.Title.Render("Сбор ссылок со всех источников…") + "\n" +
			theme.Dim.Render("ожидание провайдеров")
	case sessionStateMenu:
		body = theme.Title.Render(s.renderHeader()) + "\n" + s.list.Render()
	case sessionStateEpisodeList:
		body = theme.Title.Render(s.renderHeader()) + "\n" + s.episodeList.Render()
	case sessionStateDubVideo, sessionStateDubAudio:
		body = s.dubList.Render()
	case sessionStateQuality:
		body = s.qualityList.Render()
	case sessionStatePlaying:
		body = theme.Title.Render(s.renderHeader()) + "\n" + theme.Success.Render(s.status)
	case sessionStateInfoMenu:
		body = s.renderInfoMenu()
	case sessionStateInfoStatus:
		body = s.statusMenu().View().Content
	case sessionStateInfoScore, sessionStateInfoRewatches:
		body = s.infoPrompt.View().Content
	case sessionStateDownloadRange:
		body = s.rangeInput.View().Content
	case sessionStateDownloadMode:
		body = s.downloadModeMenu().View().Content
	default:
		body = s.list.Render()
	}
	if s.status != "" && s.state != sessionStatePlaying {
		body += "\n" + theme.StatusLine.Render(s.status)
	}
	return tea.NewView(body)
}

// sortedEmbedKeys orders embed keys lexically (python sorted()).
func sortedEmbedKeys(embeds map[string][]string) []string {
	keys := make([]string, 0, len(embeds))
	for k := range embeds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedQualityDesc orders quality labels numerically descending.
func sortedQualityDesc(links map[string]contracts.VideoSource) []string {
	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		qi, erri := strconv.Atoi(keys[i])
		qj, errj := strconv.Atoi(keys[j])
		if erri == nil && errj == nil {
			return qi > qj
		}
		return keys[i] > keys[j]
	})
	return keys
}

// providerOfTrackKey extracts the provider id of "[prov] dub".
func providerOfTrackKey(key string) string {
	if !strings.HasPrefix(key, "[") {
		return ""
	}
	if end := strings.Index(key, "]"); end > 0 {
		return key[1:end]
	}
	return ""
}

// stripProviderTag removes the "[prov] " prefix.
func stripProviderTag(key string) string {
	if end := strings.Index(key, "]"); end >= 0 && strings.HasPrefix(key, "[") {
		return strings.TrimSpace(key[end+1:])
	}
	return key
}

// mergeValues flattens a map into a slice.
func mergeValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// nowUTC is the clock seam (swapped in nothing yet; keeps saveProgress
// honest about time).
var nowUTC = func() time.Time { return time.Now().UTC() }

// sumCounts totals a count map.
func sumCounts(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}
