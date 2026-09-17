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
	"github.com/an0nx/anicli-go/internal/providers"
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

// playedMsg settles one playback; quality is the label actually used
// so the model (not a snapshot copy) can remember it (I9).
type playedMsg struct {
	err     error
	quality string
}

// shikiUpdatedMsg settles one manual «Изменить инфо» patch (I10):
// a distinct verdict from playback, carrying the (new) rate id.
type shikiUpdatedMsg struct {
	err    error
	rateID int64
}

// shikiBoundMsg settles the background shikimori id resolution (I5);
// id 0 means "no confident match, binding skipped".
type shikiBoundMsg struct {
	id    int64
	title string
}

// hydrateDoneMsg settles one episode's source hydration (PR43): the
// embeds arrive provider-prefixed, errs carries the per-provider
// failures that feed the header cause line.
type hydrateDoneMsg struct {
	num    string
	gen    int
	embeds map[string][]string
	errs   map[string]error
}

// downloadSettledMsg reports a finished foreground download batch
// (I8).
type downloadSettledMsg struct {
	count int
	err   error
}

// sessionScreen is the watch-session state machine: a merged episode
// aggregate over the grouped sources with the python session_loop
// action set (watch / next / prev / jump / redub / info / download /
// exit→root).
type sessionScreen struct {
	deps    *Deps
	primary contracts.SearchResult
	group   []contracts.SearchResult
	// resume carries the history record being continued, nil for a
	// fresh search session (I5/I6).
	resume *storage.AnimeProgress

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
	// shikiRateID is the known shikimori rate id of the bound anime
	// (0 = none yet); PATCHed instead of re-created on updates (I10).
	shikiRateID int64

	// PR43 hydration state: lazily-hydrating providers (anilib,
	// animego) list episodes with empty RawEmbeds, so the session
	// hydrates the current episode on demand — once per episode, with
	// «🔄 Обновить источники» as the explicit recovery. The per-
	// episode/merge errors feed the header cause line.
	hydrated    map[string]bool
	hydrating   bool
	hydrateGen  int
	hydrateErrs map[string]map[string]error
	sourceErrs  map[string]error
	// providerNames caches the provider id → display name map for the
	// header breakdown («Ист: 3 (AnimeLib, Nyaa)»).
	providerNames map[string]string

	// PR43 buffered watch mode: per-session toggle (no config scope),
	// the active download's cancel func and its generation counter for
	// stale-message drops.
	buffered     bool
	bufferCancel context.CancelFunc
	bufferGen    int

	state sessionState

	list        *PinList // action menu
	episodeList *PinList // «Перейти к серии»
	dubList     *PinList // video/audio dub pickers
	qualityList *PinList
	// infoList/statusList/modeList persist their submenus for the
	// whole substate visit: rebuilding per keypress reset the cursor
	// and made Enter always resolve Back (C2).
	infoList   *PinList    // «Изменить инфо» submenu
	statusList *PinList    // RU status picker
	modeList   *PinList    // download mode picker
	rangeInput *TextPrompt // download range + info numeric fields
	infoPrompt *TextPrompt // score / rewatches entry

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

// newResumedSession builds a session continued from a history record
// (I5/I6): the stored shikimori binding is injected immediately and
// the saved episode/dubs are restored once the merge finalizes.
func newResumedSession(deps *Deps, primary contracts.SearchResult, group []contracts.SearchResult, rec storage.AnimeProgress) *sessionScreen {
	s := NewSessionScreen(deps, primary, group)
	if rec.ShikimoriID != nil && *rec.ShikimoriID != 0 {
		s.setShikimoriBinding(*rec.ShikimoriID)
	}
	s.resume = &rec
	return s
}

// setShikimoriBinding injects the shikimori id into the primary's
// metadata (python: item.meta["shikimori_id"]); only primary.Meta is
// consulted by the session flows.
func (s *sessionScreen) setShikimoriBinding(id int64) {
	if s.primary.Meta == nil {
		s.primary.Meta = map[string]any{}
	}
	s.primary.Meta["shikimori_id"] = id
}

// ID implements Screen.
func (s *sessionScreen) ID() string { return sessionScreenID }

// Init implements Screen: fetch every source's episodes and, for a
// fresh session, resolve the shikimori binding in the background
// (commands own their timeout contexts — see the App.ctx note).
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
	if resolve := s.maybeShikiResolve(); resolve != nil {
		cmds = append(cmds, resolve)
	}
	return tea.Batch(cmds...)
}

// maybeShikiResolve schedules the background shikimori binding for
// fresh sessions (I5, python _resolve_shikimori_info): best
// similarity-ratio match above 0.6 wins, anything less skips the
// binding quietly with a log note.
func (s *sessionScreen) maybeShikiResolve() tea.Cmd {
	if s.deps == nil || s.deps.Shiki == nil || !s.deps.Shiki.Enabled() {
		return nil
	}
	if s.shikimoriID() != 0 {
		return nil // already bound (resumed session)
	}
	deps := s.deps
	title := BestDisplayTitle(s.group)
	return safeCmd(sessionScreenID, func() tea.Msg {
		return resolveShikiBinding(deps, title)
	})
}

// resolveShikiBinding runs the binding lookup: SearchIDs over the
// display title, best SequenceMatcher ratio (bug-compatible port)
// must clear 0.6 or the binding is skipped with a note.
func resolveShikiBinding(deps *Deps, title string) shikiBoundMsg {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ids, err := deps.Shiki.SearchIDs(ctx, title)
	if err != nil {
		deps.logger().Warn("tui: shikimori id lookup failed; binding skipped",
			"title", title, "error", err)
		return shikiBoundMsg{}
	}
	bestTitle, bestID := "", int64(0)
	bestRatio := 0.0
	for cand, id := range ids {
		ratio := providers.SimilarityRatio(strings.ToLower(title), strings.ToLower(cand))
		if ratio > bestRatio {
			bestRatio, bestTitle, bestID = ratio, cand, id
		}
	}
	if bestID == 0 || bestRatio <= shikiBindMinRatio {
		deps.logger().Info("tui: no confident shikimori match; binding skipped",
			"title", title, "best", bestTitle)
		return shikiBoundMsg{}
	}
	return shikiBoundMsg{id: bestID, title: bestTitle}
}

// shikiBindMinRatio is the python _resolve_shikimori_info confidence
// threshold (difflib ratio > 0.6).
const shikiBindMinRatio = 0.6

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

// maybeHydrateCurrent schedules the hydration of the current episode
// when it carries no sources yet and no attempt was made (PR43): the
// lazily-hydrating providers list episodes with empty RawEmbeds, and
// without this step such episodes showed «Ист: 0» forever.
func (s *sessionScreen) maybeHydrateCurrent() tea.Cmd {
	if s.hydrating {
		return nil
	}
	ep := s.currentEpisodeData()
	if ep == nil || len(ep.RawEmbeds) > 0 || s.hydrated[ep.Num] {
		return nil
	}
	return s.hydrateEpisode(ep.Num)
}

// hydrateEpisode issues one hydration round for the episode num: the
// status line reports the work (the legitimate progress display), the
// settle message merges the results under the provider prefixes.
func (s *sessionScreen) hydrateEpisode(num string) tea.Cmd {
	if num == "" {
		s.status = "Нет серий"
		return nil
	}
	s.hydrating = true
	if s.hydrated == nil {
		s.hydrated = map[string]bool{}
	}
	s.hydrated[num] = true
	s.status = "Ищу источники…"
	s.hydrateGen++
	gen := s.hydrateGen
	ep := s.episodes[num]
	deps := s.deps
	return safeCmd(sessionScreenID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		return hydrateEpisodeCmd(deps, num, ep, gen, ctx)
	})
}

// hydrateEpisodeCmd hydrates every contributing provider of one merged
// episode: the merged RawID composes "prov1:id1|prov2:id2", providers
// that already carry embeds are skipped. The results come back
// provider-prefixed, ready to merge into the aggregate.
func hydrateEpisodeCmd(deps *Deps, num string, ep contracts.Episode, gen int, ctx context.Context) hydrateDoneMsg {
	embeds := map[string][]string{}
	errs := map[string]error{}
	for _, part := range strings.Split(ep.RawID, "|") {
		prov, id, found := strings.Cut(part, ":")
		if !found || prov == "" || id == "" {
			continue
		}
		if hasProviderEmbeds(ep.RawEmbeds, prov) {
			continue
		}
		local := contracts.Episode{Num: ep.Num, RawID: id, RawEmbeds: map[string][]string{}}
		out, err := deps.Episode.HydrateDubs(ctx, prov, local)
		if err != nil {
			errs[prov] = err
			deps.logger().Warn("tui: hydration failed",
				"provider", prov, "episode", num, "error", err)
			continue
		}
		for dub, links := range out.RawEmbeds {
			embeds["["+prov+"] "+dub] = links
		}
	}
	return hydrateDoneMsg{num: num, gen: gen, embeds: embeds, errs: errs}
}

// hasProviderEmbeds reports whether any embed key belongs to prov.
func hasProviderEmbeds(embeds map[string][]string, prov string) bool {
	for key := range embeds {
		if providerOfTrackKey(key) == prov {
			return true
		}
	}
	return false
}

// applyHydration merges the settled hydration into the aggregate: new
// embeds join the episode and the dub stats, failures feed the header
// cause. Stale generations (a refresh superseded an earlier round) are
// dropped.
func (s *sessionScreen) applyHydration(msg hydrateDoneMsg) tea.Cmd {
	if msg.gen != s.hydrateGen {
		return nil
	}
	s.hydrating = false
	if ep, ok := s.episodes[msg.num]; ok {
		if ep.RawEmbeds == nil {
			ep.RawEmbeds = map[string][]string{}
		}
		for dub, links := range msg.embeds {
			ep.RawEmbeds[dub] = links
		}
		s.episodes[msg.num] = ep
		for dub := range msg.embeds {
			s.dubStats[dub]++
		}
	}
	if len(msg.errs) > 0 {
		if s.hydrateErrs == nil {
			s.hydrateErrs = map[string]map[string]error{}
		}
		s.hydrateErrs[msg.num] = msg.errs
	}
	s.buildActionMenu()
	switch {
	case len(msg.embeds) > 0:
		s.status = fmt.Sprintf("Источники найдены: %d", len(msg.embeds))
	case len(msg.errs) > 0:
		s.status = "Источники не найдены — причина в заголовке"
	default:
		s.status = "Источники не найдены"
	}
	return nil
}

// Update implements Screen: the substate machine.
func (s *sessionScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case episodePartMsg:
		delete(s.pending, msg.sourceID)
		if msg.err != nil {
			if s.sourceErrs == nil {
				s.sourceErrs = map[string]error{}
			}
			s.sourceErrs[msg.sourceID] = msg.err
		}
		if msg.err == nil && len(msg.episodes) > 0 {
			s.parts = append(s.parts, SourceEpisodes{SourceID: msg.sourceID, Episodes: msg.episodes})
		}
		if len(s.pending) == 0 {
			return s, func() tea.Msg { return episodesDoneMsg{} }
		}
		return s, nil
	case episodesDoneMsg:
		s.finalizeMerge()
		return s, s.maybeHydrateCurrent()
	case hydrateDoneMsg:
		return s, s.applyHydration(msg)
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
		if msg.quality != "" {
			s.lastQuality = msg.quality
		}
		s.status = "Воспроизведение завершено"
		s.saveProgress()
		return s, s.maybeNext()
	case shikiUpdatedMsg:
		if msg.err != nil {
			s.status = "Ошибка обновления: " + msg.err.Error()
			return s, nil
		}
		if msg.rateID != 0 {
			s.shikiRateID = msg.rateID
		}
		s.status = "Информация обновлена"
		return s, nil
	case shikiBoundMsg:
		if msg.id != 0 {
			s.setShikimoriBinding(msg.id)
		}
		return s, nil
	case downloadSettledMsg:
		if msg.err != nil {
			s.status = "Ошибка загрузки: " + msg.err.Error()
			return s, nil
		}
		s.status = fmt.Sprintf("✓ Загружено серий: %d", msg.count)
		return s, nil
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
			s.buildActionMenu()
		}
		return s, s.maybeHydrateCurrent()
	case "prev":
		if s.currentIdx > 0 {
			s.currentIdx--
			s.buildActionMenu()
		}
		return s, s.maybeHydrateCurrent()
	case "jump":
		s.state = sessionStateEpisodeList
		return s, nil
	case "refresh":
		if s.hydrating {
			return s, nil // a round is already running; its settle will report
		}
		return s, s.hydrateEpisode(s.currentEpisode())
	case "redub":
		s.videoDub, s.audioDub = "", ""
		s.status = "Озвучка сброшена — выберите заново при просмотре"
		return s, nil
	case "info":
		s.state = sessionStateInfoMenu
		s.buildInfoList()
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
// stream resolution. A sourceless episode resolves (hydrates) first —
// the lazily-hydrating providers list empty embeds (PR43) — and an
// episode whose hydration already found nothing explains the recovery
// path instead of silently doing nothing.
func (s *sessionScreen) startWatch() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	if ep == nil {
		s.status = "Нет серий"
		return s, nil
	}
	if len(ep.RawEmbeds) == 0 {
		if !s.hydrated[ep.Num] && !s.hydrating {
			return s, s.hydrateEpisode(ep.Num)
		}
		if !s.hydrating {
			s.status = "Нет источников — выполните «🔄 Обновить источники»"
		}
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
		name := stripProviderTag(s.videoDub)
		if tag := s.dubLangTag(s.videoDub); tag != "" {
			name = tag + " " + name
		}
		choices = append(choices, Choice{
			ID:    s.videoDub,
			Label: "⭐ Как видео (" + name + ")",
			Value: s.videoDub,
		})
	}
	for _, k := range keys {
		if state == sessionStateDubAudio && k == s.videoDub {
			continue
		}
		label := k
		if tag := s.dubLangTag(k); tag != "" {
			label = tag + " " + k
		}
		choices = append(choices, Choice{
			ID:    k,
			Label: fmt.Sprintf("%s [%d сер.]", label, s.dubStats[k]),
			Value: k,
		})
	}
	title := "Выберите источник видео:"
	if state == sessionStateDubAudio {
		title = "Выберите источник аудио:"
	}
	s.state = state
	s.dubList = NewPinList(NewMenu(title, "Нет доступных потоков", choices...), defaultListHeight)
	return s, nil
}

// dubLangTag returns the display language tag ("[RU]", "[JA]") of a
// dub key, looking the key's provider content language up via the
// episode service; "" when the language is unknown (plain name).
func (s *sessionScreen) dubLangTag(key string) string {
	lang := s.deps.Episode.ContentLanguage(providerOfTrackKey(key))
	if lang == "" {
		return ""
	}
	return "[" + strings.ToUpper(lang) + "]"
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
	s.qualityList = NewPinList(NewMenu("Выберите источник (качество):", "", choices...), defaultListHeight)
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

// doPlay performs the synchronous play pipeline. It runs on a struct
// snapshot, so the quality used travels back via playedMsg and the
// live model applies it in Update (I9).
func (s *sessionScreen) doPlay(qualityChoice string) tea.Msg {
	ctx := context.Background()
	ep := s.currentEpisodeData()

	video, usedQuality, err := s.pickVideo(ctx, ep, qualityChoice)
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
	if err != nil {
		return playedMsg{err: err}
	}
	return playedMsg{quality: usedQuality}
}

// errNoPlayback reports an unwired playback service.
var errNoPlayback = fmt.Errorf("tui: playback service unavailable")

// pickVideo resolves the video variant for the chosen quality,
// returning the source and the quality label actually used.
func (s *sessionScreen) pickVideo(ctx context.Context, ep *contracts.Episode, qualityChoice string) (*contracts.VideoSource, string, error) {
	links := s.resolvedLinks
	if links == nil {
		stream, err := s.deps.Episode.ResolveStream(ctx, providerOfTrackKey(s.videoDub), *ep, s.videoDub)
		if err != nil {
			return nil, "", fmt.Errorf("видео %s: %w", s.videoDub, err)
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
				return nil, "", fmt.Errorf("нет потоков у %s", s.videoDub)
			}
			quality = qualities[0]
		}
	}
	src, ok := links[quality]
	if !ok {
		return nil, "", fmt.Errorf("качество %s недоступно", quality)
	}
	return &src, quality, nil
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
	s.buildActionMenu()
	return s, s.maybeHydrateCurrent()
}

// handleInfoMenuKey drives the «Изменить инфо» submenu over the
// persisted list (C2).
func (s *sessionScreen) handleInfoMenuKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.infoList == nil {
		s.buildInfoList()
	}
	if s.infoList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.infoList.Menu(), s.infoList.Cursor(), key)
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
		s.buildStatusList()
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

// buildInfoList renders the info submenu once per visit.
func (s *sessionScreen) buildInfoList() {
	s.infoList = NewPinList(NewMenu("Что изменить?", "", []Choice{
		{ID: "status", Label: "Статус"},
		{ID: "score", Label: "Оценка"},
		{ID: "rewatches", Label: "Пересмотры"},
	}...), defaultListHeight)
}

// handleInfoStatusKey drives the RU status picker over the persisted
// list (C2).
func (s *sessionScreen) handleInfoStatusKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.statusList == nil {
		s.buildStatusList()
	}
	if s.statusList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.statusList.Menu(), s.statusList.Cursor(), key)
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

// buildStatusList renders the RU status picker once per visit.
func (s *sessionScreen) buildStatusList() {
	choices := make([]Choice, 0, len(ruStatuses))
	for _, st := range ruStatuses {
		choices = append(choices, Choice{ID: st.Key, Label: st.Label, Value: st.Key})
	}
	s.statusList = NewPinList(NewMenu("Выберите статус:", "", choices...), defaultListHeight)
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
	// Branch on the ORIGINAL substate before returning to the menu —
	// the score prompt must submit as score, not rewatches (C1).
	wasScore := s.state == sessionStateInfoScore
	s.state = sessionStateMenu
	if wasScore {
		return s, s.pushShikiUpdate("", &v, nil)
	}
	return s, s.pushShikiUpdate("", nil, &v)
}

// pushShikiUpdate applies the manual status patch (python
// update_shikimori_status_manual port). The settle is the dedicated
// shikiUpdatedMsg (I10); the rate id is looked up, reused and
// persisted so repeated patches PATCH instead of duplicating rates.
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
	knownRate := s.shikiRateID
	return safeCmd(sessionScreenID, func() tea.Msg {
		return pushShikiUpdateCmd(deps, id, knownRate, status, score, rewatches)
	})
}

// pushShikiUpdateCmd runs one patch: resolve the stored rate id (the
// session's, else the history row's), PATCH it when present or create
// and persist a new one (SyncEpisodeProgress pattern — the id write
// survives caller cancellation so the next patch reuses it).
func pushShikiUpdateCmd(deps *Deps, shikimoriID, knownRate int64, status string, score, rewatches *int) shikiUpdatedMsg {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	rateID, animeID := knownRate, int64(0)
	if rateID == 0 && deps.History != nil {
		if rec, err := deps.History.GetByShikimoriID(ctx, shikimoriID); err == nil && rec != nil {
			animeID = rec.ID
			if rec.ShikimoriRateID != nil {
				rateID = *rec.ShikimoriRateID
			}
		}
	}
	newRate, err := deps.Shiki.UpdateStatus(ctx, shikimoriID, rateID, status, score, rewatches)
	if err != nil {
		return shikiUpdatedMsg{err: fmt.Errorf("shikimori: %w", err)}
	}
	if rateID == 0 && newRate != 0 && animeID != 0 && deps.History != nil {
		if err := deps.History.SetRateID(context.WithoutCancel(ctx), animeID, newRate); err != nil {
			deps.logger().Warn("tui: persist shikimori rate id failed",
				"anime", animeID, "error", err)
		}
	}
	return shikiUpdatedMsg{rateID: newRate}
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
	s.buildModeList()
	return s, nil
}

// handleDownloadModeKey dispatches foreground/background downloads
// over the persisted mode list (C2).
func (s *sessionScreen) handleDownloadModeKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.modeList == nil {
		s.buildModeList()
	}
	if s.modeList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.modeList.Menu(), s.modeList.Cursor(), key)
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
		deps := s.deps
		count := len(tasks)
		s.status = fmt.Sprintf("Загрузка %d серий (передний план)…", count)
		return s, safeCmd(sessionScreenID, func() tea.Msg {
			var firstErr error
			for _, task := range tasks {
				if err := deps.Download.Download(context.Background(), task); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return downloadSettledMsg{count: count, err: firstErr}
		})
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

// buildModeList renders the foreground/background picker once per
// visit (C2).
func (s *sessionScreen) buildModeList() {
	s.modeList = NewPinList(NewMenu("Режим загрузки:", "", []Choice{
		{ID: "foreground", Label: "⏳ Передний план (последовательно)", Value: "foreground"},
		{ID: "background", Label: "🔁 Фон (продолжить работу)", Value: "background"},
	}...), defaultListHeight)
}

// downloadSettledMsg reports one finished foreground download.

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
	s.restoreResume()
}

// restoreResume re-applies the history record's saved episode and dub
// preferences once the merge is final (I6, python session_loop's
// start_episode / initial dubs).
func (s *sessionScreen) restoreResume() {
	if s.resume == nil {
		return
	}
	rec := *s.resume
	for i, n := range s.order {
		if n == rec.CurrentEpisode {
			s.currentIdx = i
			break
		}
	}
	if rec.VideoDub != nil && *rec.VideoDub != "" {
		s.videoDub = *rec.VideoDub
	}
	if rec.AudioDub != nil && *rec.AudioDub != "" {
		s.audioDub = *rec.AudioDub
	}
	if rec.ShikimoriRateID != nil && *rec.ShikimoriRateID != 0 {
		s.shikiRateID = *rec.ShikimoriRateID
	}
	s.buildActionMenu()
}

// buildActionMenu renders the python session_loop choices plus the
// PR43 additions («🔄 Обновить источники» recovery action). «Смотреть»
// is disabled while the current episode carries no sources (the dimmed
// row explains on an attempt — never a silent dead end).
func (s *sessionScreen) buildActionMenu() {
	watchDisabled := false
	if ep := s.currentEpisodeData(); ep != nil && len(ep.RawEmbeds) == 0 {
		watchDisabled = true
	}
	s.list = NewPinList(NewMenu(s.renderHeader(), "", []Choice{
		{ID: "watch", Label: "▶ Смотреть", Disabled: watchDisabled},
		{ID: "next", Label: "⏭ След."},
		{ID: "prev", Label: "⏮ Пред."},
		{ID: "jump", Label: "🔢 Перейти к серии"},
		{ID: "redub", Label: "🎨 Сменить озвучку"},
		{ID: "info", Label: "📝 Изменить инфо"},
		{ID: "download", Label: "⬇ Скачать серии"},
		{ID: "refresh", Label: "🔄 Обновить источники"},
		{ID: "exit", Label: "🚪 Выход"},
	}...), defaultListHeight)
}

// formatLabel renders the buffered-mode toggle («Формат: [потоковый]» /
// «Формат: [буферный]», PR43 C: per-session preference, no config).
func (s *sessionScreen) formatLabel() string {
	if s.buffered {
		return "Формат: [буферный]"
	}
	return "Формат: [потоковый]"
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
			s.episodeList.SetMarker(i, "★")
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

// renderHeader composes the session status header (PR41 breakdown):
// «Ист: N (Provider, Provider)» when sources exist, otherwise the
// honest «Ист: 0 — источники не найдены» with the known one-line cause
// (resolve not performed / per-provider errors / providers finished
// without results).
func (s *sessionScreen) renderHeader() string {
	ep := s.currentEpisodeData()
	embeds := 0
	if ep != nil {
		embeds = len(ep.RawEmbeds)
	}
	h := fmt.Sprintf("📺 %s | Эп. %s | Ист: %d", BestDisplayTitle(s.group), s.currentEpisode(), embeds)
	if ep != nil {
		if embeds > 0 {
			if names := s.embedProviderNames(ep); len(names) > 0 {
				h += fmt.Sprintf(" (%s)", strings.Join(names, ", "))
			}
		} else {
			h += " — источники не найдены"
			if cause := s.episodeCause(ep); cause != "" {
				h += ": " + cause
			}
		}
	}
	if s.videoDub != "" {
		h += fmt.Sprintf(" | 🔊 %s", s.videoDub)
	}
	if total := sumCounts(s.localCounts); total > 0 {
		h += fmt.Sprintf(" | локально: %d", total)
	}
	return h
}

// embedProviderNames lists the deduplicated display names of the
// providers contributing to the episode's embeds (the data is already
// in hand — the keys carry the provider ids; no extra API calls).
func (s *sessionScreen) embedProviderNames(ep *contracts.Episode) []string {
	ids := map[string]bool{}
	for key := range ep.RawEmbeds {
		if prov := providerOfTrackKey(key); prov != "" {
			ids[prov] = true
		}
	}
	names := make([]string, 0, len(ids))
	for prov := range ids {
		names = append(names, s.providerDisplayName(prov))
	}
	sort.Strings(names)
	return names
}

// providerDisplayName resolves the human-readable provider name,
// falling back to the raw id (the lookup is lazily cached from the
// search service roster).
func (s *sessionScreen) providerDisplayName(prov string) string {
	if s.providerNames == nil {
		s.providerNames = map[string]string{}
		if s.deps != nil && s.deps.Search != nil {
			for _, meta := range s.deps.Search.Providers() {
				s.providerNames[meta.ID] = meta.Name
			}
		}
	}
	if name := s.providerNames[prov]; name != "" {
		return name
	}
	return prov
}

// causeCauseLineMax bounds the header error summary so a long provider
// failure list cannot push the menu off-screen.
const causeLineMax = 160

// episodeCause builds the one-line «why no sources» explanation for
// the header (PR41 B1): resolve not performed, the per-provider error
// summary (truncated), or the providers-finished-empty verdict.
func (s *sessionScreen) episodeCause(ep *contracts.Episode) string {
	if !s.hydrated[ep.Num] {
		return "резолв не выполнен"
	}
	var parts []string
	for prov, err := range s.hydrateErrs[ep.Num] {
		parts = append(parts, prov+": "+err.Error())
	}
	for srcID, err := range s.sourceErrs {
		if !hasProviderEmbeds(ep.RawEmbeds, srcID) && !strings.Contains(ep.RawID, srcID+":") {
			parts = append(parts, srcID+": "+err.Error())
		}
	}
	if len(parts) == 0 {
		return "все провайдеры завершились без результатов"
	}
	sort.Strings(parts)
	summary := strings.Join(parts, "; ")
	runes := []rune(summary)
	if len(runes) > causeLineMax {
		summary = string(runes[:causeLineMax]) + "…"
	}
	return summary
}

// renderEpisodeList renders the jump list surface, recomputing the
// local-availability stars so late index refreshes show up.
func (s *sessionScreen) renderEpisodeList() string {
	for i, num := range s.order {
		if s.localCounts[num] > 0 {
			s.episodeList.SetMarker(i, "★")
		} else {
			s.episodeList.SetMarker(i, "")
		}
	}
	return s.episodeList.Render()
}

// renderInfoMenu renders the info submenu surface.
func (s *sessionScreen) renderInfoMenu() string {
	if s.infoList == nil {
		s.buildInfoList()
	}
	return theme.Title.Render("Что изменить?") + "\n\n" + s.infoList.Render()
}

// themedList renders one PinList surface with its padded title and a
// blank separator line (PR24 title padding) — the picker substates.
func themedList(list *PinList) string {
	if list == nil {
		return ""
	}
	return theme.Title.Render(list.Menu().Title) + "\n\n" + list.Render()
}

// View implements Screen.
func (s *sessionScreen) View() tea.View {
	var body string
	switch s.state {
	case sessionStateLoading:
		body = theme.Title.Render("Сбор ссылок со всех провайдеров…") + "\n\n" +
			theme.Dim.Render("ожидание провайдеров")
	case sessionStateMenu:
		body = theme.Title.Render(s.renderHeader()) + "\n\n" + s.list.Render()
	case sessionStateEpisodeList:
		body = theme.Title.Render(s.renderHeader()) + "\n\n" + s.episodeList.Render()
	case sessionStateDubVideo, sessionStateDubAudio:
		body = themedList(s.dubList)
	case sessionStateQuality:
		body = themedList(s.qualityList)
	case sessionStatePlaying:
		body = theme.Title.Render(s.renderHeader()) + "\n" + theme.Success.Render(s.status)
	case sessionStateInfoMenu:
		body = s.renderInfoMenu()
	case sessionStateInfoStatus:
		if s.statusList == nil {
			s.buildStatusList()
		}
		body = theme.Title.Render("Выберите статус:") + "\n\n" + s.statusList.Render()
	case sessionStateInfoScore, sessionStateInfoRewatches:
		body = s.infoPrompt.View().Content
	case sessionStateDownloadRange:
		body = s.rangeInput.View().Content
	case sessionStateDownloadMode:
		if s.modeList == nil {
			s.buildModeList()
		}
		banner := ""
		if s.deps != nil && s.deps.Download != nil {
			banner = s.deps.Download.ActiveBanner()
		}
		if banner != "" {
			banner = theme.StatusLine.Render(banner) + "\n"
		}
		body = theme.Title.Render("Режим загрузки:") + "\n\n" + banner + s.modeList.Render()
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
