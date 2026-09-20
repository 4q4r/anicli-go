package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/buffered"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Session substates (python session_loop's inline prompts).
type sessionState string

const (
	sessionStateLoading       sessionState = "loading"
	sessionStateMenu          sessionState = "menu"
	sessionStateEpisodeList   sessionState = "episodes"
	sessionStateDubAudio      sessionState = "dub_audio"
	sessionStateQuality       sessionState = "quality"
	sessionStateFormat        sessionState = "format"
	sessionStateBuffering     sessionState = "buffering"
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

// episodePartMsg settles one source's episode fetch. The list
// surfaces as-is — hydration is NOT part of the fetch phase (PR44
// owner model: the list shows immediately; streams resolve on
// demand for the opened episode only).
type episodePartMsg struct {
	sourceID string
	episodes []contracts.Episode
	err      error
}

// episodesDoneMsg finalizes the merge after every source settled.
type episodesDoneMsg struct{}

// streamEntry is one concrete playable stream of the merged picker
// list (PR61): a quality variant of one provider dub — the python
// merged model where the choice happens per stream, never per
// provider.
type streamEntry struct {
	// Quality is the resolution label ("1080").
	Quality string
	// DubKey is the provider-prefixed dub ("[anilib] AniLib").
	DubKey string
	// Source is the playable link.
	Source contracts.VideoSource
}

// streamResolvedMsg carries the merged stream entries for the picker
// (PR61): entries from every consulted provider dub of the episode,
// quality-sorted. scope mirrors the resolve request ("" = all dubs —
// the interactive merged list; a dub key = the remembered-dub fast
// path that auto-plays its best/remembered quality). The skip verdict
// rides along (PR61): it is stream-independent, so it fetches once
// during the resolve and the note composes into the launch line. gen
// tags the resolve round — a cancelled or superseded round's late
// settle cleans up after itself.
type streamResolvedMsg struct {
	gen          int
	scope        string
	entries      []streamEntry
	skipEpisode  string
	skipNote     string
	skipChapters string
	skipCleanup  func()
	err          error
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

// shikiSyncedMsg settles the on-start watch-progress push (PR61):
// note carries the status-line verdict (synced / typed skip), err a
// hard failure.
type shikiSyncedMsg struct {
	err  error
	note string
}

// shikiSyncTimeout bounds the on-start progress push (3c: non-blocking,
// bounded context alongside playback).
const shikiSyncTimeout = 15 * time.Second

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

// bufferReadyMsg settles one buffered download (PR43 C). On success
// the handle's file plays locally and is cleaned up after the player
// exits; stale generations clean the handle up immediately instead.
type bufferReadyMsg struct {
	gen     int
	handle  buffered.Handle
	quality string
	err     error
}

// bufferedProgressMsg is one throttled progress sample of the active
// download; bufferedProgressEnd closes the pump (channel drained).
type bufferedProgressMsg struct {
	gen int
	p   buffered.Progress
}

type bufferedProgressEnd struct{ gen int }

// downloadSettledMsg reports a finished foreground download batch
// (I8) with its per-episode report (PR64 #3): count/total successes,
// the first error for the headline and one report line per episode.
type downloadSettledMsg struct {
	count  int
	total  int
	err    error
	report []downloadEpisodeReport
}

// backgroundQueuedMsg settles the per-episode dub resolution of a
// background range download (PR64 #3): the RESOLVED tasks are queued
// (never a dead dub), and the report types each episode's dub or
// failure.
type backgroundQueuedMsg struct {
	queued int
	total  int
	report []downloadEpisodeReport
}

// downloadProgressMsg is one per-episode tick of a running foreground
// range download (review fix 2): a resolve verdict or a download
// start/finish. gen tags the batch — a superseded batch's ticks drop.
// downloadProgressEnd closes the pump (channel drained).
type downloadProgressMsg struct {
	gen  int
	line string
}

type downloadProgressEnd struct{ gen int }

// waitDownloadProgress is the re-arming pump of the range-download
// progress channel (the buffered-watch pattern).
func waitDownloadProgress(ch <-chan string, gen int) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return downloadProgressEnd{gen: gen}
		}
		return downloadProgressMsg{gen: gen, line: line}
	}
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

	// PR43/PR44 hydration state: hydration is strictly ON-DEMAND —
	// the fetch phase never resolves streams in bulk (the owner
	// model: the list surfaces immediately, the release-scoped dub
	// lists ride the episodes from the provider, and the heavy
	// per-episode hydration happens only for the episode being
	// opened, via «Смотреть» or «🔄 Обновить источники»). `hydrated`
	// marks the episodes an attempt was made for; `hydrateErrs`
	// carries the per-episode/per-provider failures feeding the
	// header cause.
	hydrated    map[string]bool
	hydrating   bool
	hydrateGen  int
	hydrateErrs map[string]map[string]error
	sourceErrs  map[string]error
	// providerNames caches the provider id → display name map for the
	// header breakdown («Ист: 3 (AnimeLib, Nyaa)»).
	providerNames map[string]string

	// PR43 buffered watch mode; PR44 arms it from the pre-play format
	// selector (no config scope). Plus the active download's cancel
	// func, generation counter for stale-message drops, and its
	// progress channel (the pump re-arms through bufferedProgressMsg).
	buffered     bool
	bufferCancel context.CancelFunc
	bufferGen    int
	bufferProgCh chan buffered.Progress

	// Range-download batch state (review fix 2): downloadGen tags the
	// active batch (a newer batch drops the stale pump) and
	// downloadProgCh carries the per-episode progress ticks.
	downloadGen    int
	downloadProgCh chan string

	state sessionState

	list          *PinList   // action menu
	episodeList   *PinList   // «Перейти к серии»
	episodeFilter listFilter // PR78 type-to-search over the jump list
	dubList       *PinList   // video/audio dub pickers
	qualityList   *PinList
	// formatList is the pre-play format selector (PR44): «▶ Смотреть»
	// opens it with exactly two items («Потоковый», «Буферный»); the
	// pick arms the buffered mode and continues the watch pipeline.
	formatList *PinList
	// infoList/statusList/modeList persist their submenus for the
	// whole substate visit: rebuilding per keypress reset the cursor
	// and made Enter always resolve Back (C2).
	infoList   *PinList    // «Изменить инфо» submenu
	statusList *PinList    // RU status picker
	modeList   *PinList    // download mode picker
	rangeInput *TextPrompt // download range + info numeric fields
	infoPrompt *TextPrompt // score / rewatches entry

	downloadEpisodes []string
	// streamEntries cache the merged picker entries between the
	// resolve settle and the pick (Back from the audio prompt returns
	// to the list without re-resolving).
	streamEntries []streamEntry
	// pickedVideo is the entry chosen from the merged list; doPlay
	// and the buffered pipeline consume it directly.
	pickedVideo contracts.VideoSource
	// skip cache (PR61): the verdict of the episode's skip lookup,
	// fetched during the stream resolve (stream-independent) and
	// consumed at launch/doPlay. The cleanup removes the chapters
	// file; playedMsg clears the whole cache (the file is gone).
	skipEpisode  string
	skipNote     string
	skipChapters string
	skipCleanup  func()
	// resolveGen tags the in-flight stream-resolve round: a cancel
	// bumps it so the late settle drops its own chapters file instead
	// of leaking it into the pending slot (PR61 review R1c).
	resolveGen  int
	localCounts map[string]int

	// status scopes the transient verdict line to the substate
	// surface that set it (PR64 #1) — see surfaceStatus.
	surfaceStatus
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

// setState switches the session substate surface. Every transition
// invalidates the transient status line: a verdict belongs to the
// surface that set it, and the next surface starts clean (PR64 #1).
func (s *sessionScreen) setState(next sessionState) {
	if s.state != next {
		s.bumpSurface()
	}
	s.state = next
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

// hydrateEpisode issues one hydration round for the episode num (the
// PR43 on-demand trigger behind «Смотреть» and the «🔄 Обновить
// источники» recovery): the status line reports the work (the
// legitimate progress display), the settle message merges the results
// under the provider prefixes. The attempt is recorded so the header
// cause can distinguish unopened episodes from genuinely empty ones.
func (s *sessionScreen) hydrateEpisode(num string) tea.Cmd {
	if num == "" {
		s.setStatus("Нет серий")
		return nil
	}
	if s.hydrated == nil {
		s.hydrated = map[string]bool{}
	}
	s.hydrated[num] = true
	s.hydrating = true
	s.setStatus("Ищу источники…")
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

// hasProviderEmbeds reports whether prov already contributes embeds
// with ACTUAL links to the episode. The release-scope tier-1 dub list
// rides every episode as keys with EMPTY lists — that state is NOT
// hydrated, and counting it here turned «🔄 Обновить источники» into
// a no-op (review MAJOR): only keys carrying ≥1 link count.
func hasProviderEmbeds(embeds map[string][]string, prov string) bool {
	for key, links := range embeds {
		if providerOfTrackKey(key) == prov && len(links) > 0 {
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
		s.setStatus(fmt.Sprintf("Источники найдены: %d", len(msg.embeds)))
	case len(msg.errs) > 0:
		s.setStatus("Источники не найдены — причина в заголовке")
	default:
		s.setStatus("Источники не найдены")
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
		return s, nil
	case hydrateDoneMsg:
		return s, s.applyHydration(msg)
	case bufferReadyMsg:
		return s, s.applyBufferReady(msg)
	case bufferedProgressMsg:
		if msg.gen != s.bufferGen {
			return s, nil
		}
		s.setStatus(formatBufferedProgress(msg.p))
		return s, waitBufferedProgress(s.bufferProgCh, msg.gen)
	case bufferedProgressEnd:
		return s, nil
	case streamResolvedMsg:
		if msg.gen != s.resolveGen {
			// A superseded round (cancel or a newer watch landed
			// first): nobody else will clean its chapters file.
			if msg.skipCleanup != nil {
				msg.skipCleanup()
			}
			return s, nil
		}
		if msg.err != nil {
			if msg.skipCleanup != nil {
				msg.skipCleanup()
			}
			s.setState(sessionStateMenu)
			s.setStatus("Ошибка: " + msg.err.Error())
			return s, nil
		}
		// A new verdict always retires the previous pending chapters
		// file — each round writes a unique temp file, so leaving the
		// old cleanup pending would orphan it (PR61 review R1b).
		if s.skipCleanup != nil {
			s.skipCleanup()
			s.skipCleanup = nil
		}
		s.skipEpisode = msg.skipEpisode
		s.skipNote = msg.skipNote
		s.skipChapters = msg.skipChapters
		s.skipCleanup = msg.skipCleanup
		if msg.scope != "" {
			// Remembered-dub fast path (python resolve_dubs_smart):
			// auto-pick the remembered quality (or the best) and play
			// straight away — no extra prompts.
			e := autoStreamEntry(msg.entries, s.lastQuality)
			s.videoDub, s.lastQuality, s.pickedVideo = e.DubKey, e.Quality, e.Source
			return s.launchPlayback()
		}
		s.streamEntries = msg.entries
		s.buildStreamList()
		return s, nil
	case playedMsg:
		s.setState(sessionStateMenu)
		// The chapters file was removed by the play pipeline's
		// cleanup (PR61); the cache clears so the next watch of the
		// episode re-resolves instead of replaying a dead path.
		s.skipChapters, s.skipNote, s.skipCleanup = "", "", nil
		if msg.err != nil {
			s.setStatus("Ошибка воспроизведения: " + msg.err.Error())
			return s, nil
		}
		if msg.quality != "" {
			s.lastQuality = msg.quality
		}
		s.setStatus("Воспроизведение завершено")
		return s, s.maybeNext()
	case shikiUpdatedMsg:
		if msg.err != nil {
			s.setStatus("Ошибка обновления: " + msg.err.Error())
			return s, nil
		}
		if msg.rateID != 0 {
			s.shikiRateID = msg.rateID
		}
		s.setStatus("Информация обновлена")
		return s, nil
	case shikiSyncedMsg:
		if msg.err != nil {
			s.setStatus(msg.err.Error())
			return s, nil
		}
		if msg.note != "" {
			s.setStatus(msg.note)
		}
		return s, nil
	case shikiBoundMsg:
		if msg.id != 0 {
			s.setShikimoriBinding(msg.id)
		}
		return s, nil
	case downloadProgressMsg:
		if msg.gen != s.downloadGen {
			return s, nil
		}
		s.setStatus(msg.line)
		return s, waitDownloadProgress(s.downloadProgCh, msg.gen)
	case downloadProgressEnd:
		return s, nil
	case downloadSettledMsg:
		s.setStatus(renderDownloadSettle(msg))
		return s, nil
	case backgroundQueuedMsg:
		s.setStatus(renderBackgroundQueued(msg))
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
	case sessionStateDubAudio:
		return s.handleDubKey(key)
	case sessionStateQuality:
		return s.handleQualityKey(key)
	case sessionStateFormat:
		return s.handleFormatKey(key)
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
// menu; the menu itself pops (root via «Выход»). Cancelling the
// buffering state stops the download and cleans its temp file (PR43 C).
// A pending skip chapters file (resolved but never played, PR61) is
// removed on the way out.
func (s *sessionScreen) handleCancel() (Screen, tea.Cmd) {
	if s.skipCleanup != nil && s.state != sessionStatePlaying {
		s.skipCleanup()
		s.skipCleanup = nil
		s.skipChapters = ""
	}
	// Any in-flight resolve round is invalidated: its late settle
	// cleans up after itself (PR61 review R1c).
	s.resolveGen++
	switch s.state {
	case sessionStateMenu, sessionStatePlaying:
		return s, pop()
	case sessionStateLoading:
		return s, pop()
	case sessionStateBuffering:
		s.stopBuffering("Буферизация отменена")
		return s, nil
	default:
		// PR78 type-to-search: an engaged episode-filter clears
		// first — only the NEXT Esc leaves the list for the menu.
		if s.state == sessionStateEpisodeList && s.episodeFilter.active() {
			s.episodeFilter.clear()
			s.buildEpisodeList()
			return s, nil
		}
		s.setState(sessionStateMenu)
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
			// python parity (PR63 #2): the pick falls through into
			// resolve_dubs_smart + extract_and_play — the next episode
			// starts playing right away when the remembered dubs still
			// fit.
			return s.autoWatchNext()
		}
		return s, nil
	case "prev":
		if s.currentIdx > 0 {
			s.currentIdx--
			s.buildActionMenu()
		}
		return s, nil
	case "jump":
		s.setState(sessionStateEpisodeList)
		return s, nil
	case "refresh":
		if s.hydrating {
			return s, nil // a round is already running; its settle will report
		}
		return s, s.hydrateEpisode(s.currentEpisode())
	case "rebind":
		// PR62 #3: re-run the provider fan-out over the stored record;
		// the next checklist pick re-persists the binding (BindSource
		// moves the row, deleting the old source key).
		if s.resume == nil {
			return s, nil
		}
		return s, replace(newRebindProgress(s.deps, s.resume))
	case "redub":
		s.videoDub, s.audioDub = "", ""
		s.setStatus("Озвучка сброшена — выберите заново при просмотре")
		return s, nil
	case "info":
		s.setState(sessionStateInfoMenu)
		s.buildInfoList()
		return s, nil
	case "download":
		if s.deps == nil || s.deps.Download == nil {
			s.setStatus("Загрузка недоступна")
			return s, nil
		}
		s.setState(sessionStateDownloadRange)
		s.rangeInput = NewTextPrompt(TextPromptConfig{
			ID:     "download-range",
			Title:  "Серии для загрузки (например 1-5, 7):",
			Status: describeAvailableEpisodes(s.order),
		})
		return s, s.rangeInput.Init()
	case "exit":
		return s, popToRoot()
	default:
		return s, nil
	}
}

// startWatch launches the watch pipeline at the PR44 pre-play format
// selector («Потоковый» / «Буферный» — the python per-episode format
// choice pattern). A sourceless episode resolves (hydrates) first —
// the lazily-hydrating providers list empty embeds (PR43) — and an
// episode whose hydration already found nothing explains the recovery
// path instead of silently doing nothing; the selector never opens
// without sources (the dimmed «Смотреть» row keeps it unreachable).
func (s *sessionScreen) startWatch() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	if ep == nil {
		s.setStatus("Нет серий")
		return s, nil
	}
	if len(ep.RawEmbeds) == 0 {
		// PR43's on-demand trigger, scoped to the opened episode: the
		// first watch hydrates THIS episode once; the steady state
		// after an attempt that found nothing points at the recovery
		// item (PR44 owner model — resolving never runs bulk).
		if !s.hydrated[ep.Num] && !s.hydrating {
			return s, s.hydrateEpisode(ep.Num)
		}
		if !s.hydrating {
			s.setStatus("Нет источников — выполните «🔄 Обновить источники»")
		}
		return s, nil
	}
	s.setState(sessionStateFormat)
	s.formatList = NewPinList(NewMenu("Формат просмотра:", "", []Choice{
		{ID: "stream", Label: "Потоковый", Value: "stream"},
		{ID: "buffer", Label: "Буферный", Value: "buffer"},
	}...), defaultListHeight)
	return s, nil
}

// autoWatchNext launches the watch pipeline for the episode «⏭ След.»
// just advanced to (PR63 #2, python session_loop parity: the pick
// falls through into resolve_dubs_smart + extract_and_play). The
// remembered dubs still available on the new episode → straight to
// playback with no manual picks; the previously used source gone →
// the typed «⚠ Прошлые настройки недоступны» note + the usual
// selection (the merged stream list, with the audio prompt when the
// audio pick is fresh). The format selector does not re-open: the
// armed buffered mode carries over — the python loop has no
// per-episode format step. A sourceless episode hydrates first (the
// same on-demand trigger as «Смотреть»).
func (s *sessionScreen) autoWatchNext() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	if ep == nil {
		s.setStatus("Нет серий")
		return s, nil
	}
	if len(ep.RawEmbeds) == 0 {
		if !s.hydrated[ep.Num] && !s.hydrating {
			return s, s.hydrateEpisode(ep.Num)
		}
		if !s.hydrating {
			s.setStatus("Нет источников — выполните «🔄 Обновить источники»")
		}
		return s, nil
	}
	warn := ""
	if s.videoDub != "" && len(ep.RawEmbeds[s.videoDub]) == 0 {
		// python resolve_dubs_smart: the previously used source is not
		// on the new episode — a typed note, then the usual selection.
		// The note is stamped AFTER the pipeline opens its surface so
		// it scopes to the picker that explains it (PR64 #1).
		warn = fmt.Sprintf("⚠ Прошлые настройки недоступны: %s / %s",
			s.videoDub, s.audioDub)
	}
	scr, cmd := s.proceedWatch()
	if warn != "" {
		s.setStatus(warn)
	}
	return scr, cmd
}

// handleFormatKey resolves the format selector: the pick arms the
// watch mode and continues the pipeline; Back/Esc returns to the
// episode menu without playing. A buffered pick without the buffered
// service explains honestly and keeps the selector open (streaming
// remains pickable).
func (s *sessionScreen) handleFormatKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.formatList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.formatList.Menu(), s.formatList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.setState(sessionStateMenu)
		return s, nil
	}
	choice, _ := resolved.(string)
	switch choice {
	case "buffer":
		if s.deps == nil || s.deps.Buffered == nil {
			s.setStatus("Буферный режим недоступен")
			return s, nil
		}
		s.buffered = true
	case "stream":
		s.buffered = false
	default:
		return s, nil
	}
	return s.proceedWatch()
}

// proceedWatch continues the watch pipeline after the format pick:
// dubs remembered and available → straight to the scoped resolve
// (auto quality, python resolve_dubs_smart parity); otherwise the
// merged stream list over every provider dub (PR61 — the choice is
// per stream, never per provider), with the audio prompt following
// the video pick.
func (s *sessionScreen) proceedWatch() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	if ep == nil {
		s.setStatus("Нет серий")
		return s, nil
	}
	if s.videoDub == "" || len(ep.RawEmbeds[s.videoDub]) == 0 {
		return s.beginStreamResolve("")
	}
	if (s.audioDub == "" || len(ep.RawEmbeds[s.audioDub]) == 0) && s.audioDub != s.videoDub {
		return s.openAudioSelect()
	}
	return s.beginStreamResolve(s.videoDub)
}

// openAudioSelect builds the audio prompt (python's second prompt):
// «⭐ Как видео» first — one muxed stream for both tracks — then the
// other dub keys of the episode.
func (s *sessionScreen) openAudioSelect() (Screen, tea.Cmd) {
	ep := s.currentEpisodeData()
	keys := sortedEmbedKeys(ep.RawEmbeds)
	choices := make([]Choice, 0, len(keys)+1)
	if s.videoDub != "" {
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
		if k == s.videoDub {
			continue
		}
		if len(ep.RawEmbeds[k]) == 0 {
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
	s.setState(sessionStateDubAudio)
	s.dubList = NewPinList(NewMenu("Выберите аудиопоток:", "Нет доступных аудиопотоков", choices...), defaultListHeight)
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

// handleDubKey resolves the audio picker (the video pick happens in
// the merged stream list, PR61).
func (s *sessionScreen) handleDubKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.dubList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.dubList.Menu(), s.dubList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		// Back from the audio prompt returns to the merged stream
		// list when its entries are cached (python's loop back to the
		// video prompt), else to the menu.
		if len(s.streamEntries) > 0 {
			s.setState(sessionStateQuality)
			s.buildStreamList()
			return s, nil
		}
		s.setState(sessionStateMenu)
		return s, nil
	}
	pick, _ := resolved.(string)
	s.audioDub = pick
	if s.videoDub != "" && s.audioDub == "" {
		s.audioDub = s.videoDub
	}
	return s.launchPlayback()
}

// beginStreamResolve resolves the episode's streams for the picker.
// An empty scope resolves EVERY dub carrying embed links — the merged
// list (PR61); a dub key scopes the resolve to the remembered dub
// (the fast path auto-plays its result).
func (s *sessionScreen) beginStreamResolve(scope string) (Screen, tea.Cmd) {
	s.setState(sessionStateQuality)
	s.streamEntries = nil
	s.buildStreamList()
	s.resolveGen++
	gen := s.resolveGen
	ep := *s.currentEpisodeData()
	shikiID := s.shikimoriID()
	deps := s.deps
	return s, safeCmd(sessionScreenID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		entries, err := resolveAllStreams(ctx, deps.Episode, ep, scope)
		// The skip verdict is stream-independent — it fetches here so
		// the note is ready at launch (PR61). Best-effort: a failure
		// degrades the note, never the resolve.
		note, chapters, cleanup := "", "", func() {}
		if deps.Playback != nil {
			path, cl, n, serr := deps.Playback.ResolveSkips(ctx, shikiID, EpisodeSortKey(ep.Num))
			note, cleanup = n, cl
			if serr == nil {
				chapters = path
			}
		}
		return streamResolvedMsg{
			gen:          gen,
			scope:        scope,
			entries:      entries,
			skipEpisode:  ep.Num,
			skipNote:     note,
			skipChapters: chapters,
			skipCleanup:  cleanup,
			err:          err,
		}
	})
}

// streamResolveFanout bounds the concurrent per-dub resolves of one
// merged resolve (PR61 review): the same order as the provider
// fan-out consts (anilibProbeConcurrency, ttPreflightConcurrency).
const streamResolveFanout = 8

// resolveAllStreams resolves every dub of the episode that carries
// embed links (one dub when scope is set) concurrently and merges the
// results into one quality-sorted entry list (python merge parity —
// the streams of all providers live in ONE list). A provider that
// fails degrades: its entries drop, the rest still surface; only a
// fully empty merge is an error.
func resolveAllStreams(ctx context.Context, eps EpisodeService, ep contracts.Episode, scope string) ([]streamEntry, error) {
	targets := make([]string, 0, len(ep.RawEmbeds))
	for _, k := range sortedEmbedKeys(ep.RawEmbeds) {
		if scope != "" && k != scope {
			continue
		}
		if len(ep.RawEmbeds[k]) == 0 {
			continue // tier-1 key without hydrated links (PR43)
		}
		targets = append(targets, k)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("нет источников с потоками")
	}

	type resolveResult struct {
		key    string
		stream contracts.MediaStream
		err    error
	}
	var (
		mu      sync.Mutex
		results = make([]resolveResult, 0, len(targets))
	)
	// The fan-out degrades per provider: fn never fails, so one
	// provider's error cannot cancel its siblings' lookups.
	_ = netclient.Parallel(ctx, targets, streamResolveFanout, func(ctx context.Context, key string) error {
		stream, err := eps.ResolveStream(ctx, providerOfTrackKey(key), ep, key)
		mu.Lock()
		results = append(results, resolveResult{key: key, stream: stream, err: err})
		mu.Unlock()
		return nil
	})

	entries := make([]streamEntry, 0, len(results))
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		for q, src := range r.stream.Links {
			entries = append(entries, streamEntry{Quality: q, DubKey: r.key, Source: src})
		}
	}
	sortStreamEntries(entries)
	if len(entries) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("потоки не получены: %w", firstErr)
		}
		return nil, fmt.Errorf("потоки не найдены")
	}
	return entries, nil
}

// sortStreamEntries orders the merged list: quality numerically
// descending, ties broken by dub key for a deterministic order.
func sortStreamEntries(entries []streamEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		qi, erri := strconv.Atoi(entries[i].Quality)
		qj, errj := strconv.Atoi(entries[j].Quality)
		if erri == nil && errj == nil && qi != qj {
			return qi > qj
		}
		if erri == nil && errj == nil {
			return entries[i].DubKey < entries[j].DubKey
		}
		return entries[i].Quality > entries[j].Quality
	})
}

// autoStreamEntry picks the fast-path entry: the remembered quality
// when the dub still offers it, else the best (first sorted) entry.
func autoStreamEntry(entries []streamEntry, lastQuality string) streamEntry {
	if len(entries) == 0 {
		return streamEntry{}
	}
	if lastQuality != "" {
		for _, e := range entries {
			if e.Quality == lastQuality {
				return e
			}
		}
	}
	return entries[0]
}

// handleQualityKey buffered branch (PR43 C): buffer the chosen stream
// to a local file, play the file, delete it on exit.
func (s *sessionScreen) startBuffered() (Screen, tea.Cmd) {
	snapshot := *s
	ctx, cancel := context.WithCancel(context.Background())
	s.bufferCancel = cancel
	s.bufferGen++
	gen := s.bufferGen
	s.setState(sessionStateBuffering)
	s.setStatus("Буферизация: подготовка…")
	progCh := make(chan buffered.Progress, 16)
	s.bufferProgCh = progCh
	download := safeCmd(sessionScreenID, func() tea.Msg {
		defer close(progCh)
		return snapshot.runBuffered(ctx, gen, progCh)
	})
	return s, tea.Batch(download, waitBufferedProgress(progCh, gen))
}

// runBuffered buffers the picked stream to completion, pumping
// progress onto the channel (non-blocking: a full channel just drops
// samples — the throttle keeps them coming).
func (s *sessionScreen) runBuffered(ctx context.Context, gen int, progCh chan<- buffered.Progress) tea.Msg {
	if s.pickedVideo.URL == "" {
		return bufferReadyMsg{gen: gen, err: fmt.Errorf("поток не выбран")}
	}
	handle, err := s.deps.Buffered.Buffer(ctx, buffered.Source{
		URL:     s.pickedVideo.URL,
		Headers: s.pickedVideo.Headers,
	}, func(p buffered.Progress) {
		select {
		case progCh <- p:
		default:
		}
	})
	if err != nil {
		return bufferReadyMsg{gen: gen, err: err}
	}
	return bufferReadyMsg{gen: gen, handle: handle, quality: s.lastQuality}
}

// waitBufferedProgress is the re-arming pump of the buffering phase.
func waitBufferedProgress(ch <-chan buffered.Progress, gen int) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return bufferedProgressEnd{gen: gen}
		}
		return bufferedProgressMsg{gen: gen, p: p}
	}
}

// applyBufferReady transitions from buffering into playback. Stale
// generations (a cancel or a newer round superseded this one) clean
// their handle up and vanish.
func (s *sessionScreen) applyBufferReady(msg bufferReadyMsg) tea.Cmd {
	if msg.gen != s.bufferGen {
		if msg.handle.Path != "" {
			msg.handle.Cleanup()
		}
		return nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			return nil // the cancel handler owns the status line
		}
		s.setState(sessionStateMenu)
		s.setStatus("Ошибка буферизации: " + msg.err.Error())
		return nil
	}
	s.setState(sessionStatePlaying)
	s.setStatus("▶ Запуск mpv…")
	snapshot := *s
	handle := msg.handle
	quality := msg.quality
	return safeCmd(sessionScreenID, func() tea.Msg {
		return snapshot.doPlayBuffered(handle, quality)
	})
}

// doPlayBuffered plays the buffered local file through the usual
// plumbing (audio track, skip chapters, title) and deletes the file
// when the player exits — python buffered parity.
func (s *sessionScreen) doPlayBuffered(handle buffered.Handle, quality string) tea.Msg {
	defer handle.Cleanup() // the deletion is logged by the downloader
	ctx := context.Background()
	ep := s.currentEpisodeData()
	if ep == nil {
		return playedMsg{err: fmt.Errorf("нет серий")}
	}

	audioURL := ""
	audio, err := s.pickAudio(ctx, ep)
	if err != nil {
		return playedMsg{err: err}
	}
	if audio != nil {
		audioURL = audio.URL
	}

	// Chapters come from the resolve-phase skip verdict (PR61); the
	// cleanup runs when the player exits.
	chapters := s.skipChapters
	if s.skipEpisode != ep.Num {
		chapters = ""
	}
	if chapters != "" && s.skipCleanup != nil {
		defer s.skipCleanup()
	}

	title := fmt.Sprintf("[%s - %s] %s - %s",
		stripProviderTag(s.videoDub), stripProviderTag(s.audioDub),
		BestDisplayTitle(s.group), ep.Num)

	if s.deps == nil || s.deps.Playback == nil {
		return playedMsg{err: errNoPlayback}
	}
	// Headers stay unset: the source is a local file.
	err = s.deps.Playback.Play(ctx, PlayRequest{
		URL:          handle.Path,
		AudioURL:     audioURL,
		Title:        title,
		ExtraMPVOpts: nil,
		ChaptersFile: chapters,
	})
	if err != nil {
		return playedMsg{err: err}
	}
	return playedMsg{quality: quality}
}

// stopBuffering cancels the active download and returns to the menu;
// the downloader removes its temp file on cancellation.
func (s *sessionScreen) stopBuffering(note string) {
	if s.bufferCancel != nil {
		s.bufferCancel()
		s.bufferCancel = nil
	}
	s.bufferGen++ // stale progress/ready messages drop
	s.setState(sessionStateMenu)
	s.setStatus(note)
}

// formatBufferedProgress renders the minimal progress line: percent by
// bytes (or segment count for HLS) plus the smoothed speed. The speed
// segment is byte-derived, so it is suppressed for segment-based
// samples whose byte speed is unknown (a bogus «0.0 МБ/с»).
func formatBufferedProgress(p buffered.Progress) string {
	// Segment-based samples carry no byte counts; their byte speed is
	// unknown rather than zero.
	speed := fmt.Sprintf("%.1f МБ/с", p.SpeedBPS/(1<<20))
	switch {
	case p.SegmentsTotal > 0:
		pct := 100 * p.SegmentsDone / p.SegmentsTotal
		if p.SpeedBPS == 0 {
			return fmt.Sprintf("Буферизация: %d%% (сегмент %d/%d)", pct, p.SegmentsDone, p.SegmentsTotal)
		}
		return fmt.Sprintf("Буферизация: %d%% (сегмент %d/%d) · %s", pct, p.SegmentsDone, p.SegmentsTotal, speed)
	case p.Total > 0:
		pct := 100 * p.Done / p.Total
		return fmt.Sprintf("Буферизация: %d%% · %s", pct, speed)
	default:
		return fmt.Sprintf("Буферизация: %s · %s", humanBytes(p.Done), speed)
	}
}

// humanBytes renders a byte count in the largest sensible unit.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f КБ", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d Б", n)
	}
}

// buildStreamList renders the merged stream picker (PR61): one
// quality-sorted list over every consulted provider dub, entries
// labeled quality · dub [provider] · episode coverage.
func (s *sessionScreen) buildStreamList() {
	choices := make([]Choice, 0, len(s.streamEntries)+1)
	if len(s.streamEntries) == 0 {
		choices = append(choices, Choice{ID: "resolving", Label: "Ищу потоки…", Disabled: true})
	}
	for i, e := range s.streamEntries {
		choices = append(choices, Choice{
			ID:    fmt.Sprintf("s%d", i),
			Label: s.streamEntryLabel(e),
			Value: e,
		})
	}
	s.qualityList = NewPinList(NewMenu("Выберите поток (качество · озвучка · провайдер):", "Потоки не найдены", choices...), defaultListHeight)
}

// streamEntryLabel renders one merged-list row: quality first, then
// the dub (with the language tag when known) and the provider tag,
// then the dub's episode coverage.
func (s *sessionScreen) streamEntryLabel(e streamEntry) string {
	label := e.Quality + "p"
	dub := stripProviderTag(e.DubKey)
	if tag := s.dubLangTag(e.DubKey); tag != "" {
		dub = tag + " " + dub
	}
	label += " · " + dub
	if prov := providerOfTrackKey(e.DubKey); prov != "" {
		label += " [" + prov + "]"
	}
	if n := s.dubStats[e.DubKey]; n > 0 {
		label += fmt.Sprintf(" · %d сер.", n)
	}
	return label
}

// handleQualityKey resolves the stream pick (PR61): the entry records
// video dub + quality; a fresh session continues to the audio prompt
// (python's second prompt), a remembered audio dub plays directly.
func (s *sessionScreen) handleQualityKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.qualityList.HandleKey(key) {
		return s, nil
	}
	resolved := ResolveKey(s.qualityList.Menu(), s.qualityList.Cursor(), key)
	if resolved == nil {
		return s, nil
	}
	if resolved == Back {
		s.setState(sessionStateMenu)
		return s, nil
	}
	entry, ok := resolved.(streamEntry)
	if !ok {
		return s, nil
	}
	s.videoDub = entry.DubKey
	s.lastQuality = entry.Quality
	s.pickedVideo = entry.Source
	if s.audioDub == "" {
		return s.openAudioSelect()
	}
	return s.launchPlayback()
}

// shikiSyncCmd schedules the bounded watch-progress push for the
// episode about to play (PR62 #1): nil when there is no wired shiki
// service, no binding or a non-numeric episode.
func (s *sessionScreen) shikiSyncCmd() tea.Cmd {
	if s.deps == nil || s.deps.Shiki == nil {
		return nil
	}
	if id := s.shikimoriID(); id != 0 {
		if episode := parseWatchEpisode(s.currentEpisode()); episode > 0 {
			return syncWatchProgressCmd(s.deps, id, episode)
		}
	}
	return nil
}

// launchPlayback continues after the video (and, when fresh, audio)
// picks: buffered mode buffers the picked stream, streaming launches
// mpv. The local progress save runs at LAUNCH (PR63 #1, python
// extract_and_play's db_service.save_progress before the player) for
// BOTH formats — an early exit (Esc popping the session mid-play,
// Ctrl+C leaving the detached mpv running, a player error) can no
// longer lose the episode record the library display reads. The
// skip verdict fetched during the resolve (PR61) composes into the
// launch line and auto-clears on the playback settle. The shikimori
// watch-progress push (PR61) runs ALONGSIDE playback — python
// extract_and_play's update_rate at the launch moment — for
// BOTH formats (PR62 #1: the buffered path used to skip it).
func (s *sessionScreen) launchPlayback() (Screen, tea.Cmd) {
	saveErr := s.saveProgress()
	sync := s.shikiSyncCmd()
	if s.buffered {
		if s.deps == nil || s.deps.Buffered == nil {
			s.setStatus("Буферный режим недоступен")
			return s, nil
		}
		_, buf := s.startBuffered()
		if saveErr != nil {
			s.status += " · ⚠ Не удалось сохранить прогресс: " + saveErr.Error()
		}
		if sync != nil {
			return s, tea.Batch(buf, sync)
		}
		return s, buf
	}
	s.setState(sessionStatePlaying)
	s.setStatus("▶ Запуск mpv…")
	if s.skipNote != "" && s.skipEpisode == s.currentEpisode() {
		s.status += " · ⏭ " + s.skipNote
	}
	if saveErr != nil {
		s.status += " · ⚠ Не удалось сохранить прогресс: " + saveErr.Error()
	}
	play := s.playCmd()
	if sync != nil {
		return s, tea.Batch(play, sync)
	}
	return s, play
}

// parseWatchEpisode reads the episode counter for the progress push;
// 0 marks a non-numeric episode (OVA/special) that must not zero the
// remote counter.
func parseWatchEpisode(num string) int {
	f := EpisodeSortKey(num)
	if f <= 0 {
		return 0
	}
	return int(f)
}

// syncWatchProgressCmd schedules the bounded, non-blocking push.
func syncWatchProgressCmd(deps *Deps, shikimoriID int64, episode int) tea.Cmd {
	return safeCmd(sessionScreenID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), shikiSyncTimeout)
		defer cancel()
		return syncWatchProgress(ctx, deps, shikimoriID, episode)
	})
}

// syncWatchProgress pushes {episodes: N, status} to shikimori with
// python parity: the stored status is kept (planned → watching), the
// counter only ever moves forward, and every verdict — synced, typed
// skip or failure — is logged.
func syncWatchProgress(ctx context.Context, deps *Deps, shikimoriID int64, episode int) shikiSyncedMsg {
	log := deps.logger()
	if deps.Shiki == nil || !deps.Shiki.Enabled() {
		log.Info("tui: shiki: tracker disabled; progress push skipped", "episode", episode)
		return shikiSyncedMsg{note: "Shikimori: трекер отключён — прогресс не отправлен"}
	}
	if mode := deps.Shiki.Mode(); mode == "none" || mode == "disabled" {
		log.Info("tui: shiki: no auth; progress push skipped",
			"episode", episode, "mode", mode)
		return shikiSyncedMsg{note: "Shikimori: нет авторизации — прогресс не отправлен"}
	}

	var (
		rateID  int64
		animeID int64
		prior   int
		status  = "watching"
	)
	if deps.History != nil {
		rec, err := deps.History.GetByShikimoriID(ctx, shikimoriID)
		if err != nil {
			log.Warn("tui: shiki: local row lookup failed", "error", err)
		}
		if rec != nil {
			animeID = rec.ID
			prior = parseWatchEpisode(rec.CurrentEpisode)
			if rec.ShikimoriRateID != nil {
				rateID = *rec.ShikimoriRateID
			}
			if st := shikimori.CanonicalStatus(rec.ShikimoriStatus); st != "" {
				if st == "plan_to_watch" || st == "planned" {
					st = "watching" // python: planned → watching on play
				}
				status = st
			}
		}
	}
	if prior > episode {
		log.Info("tui: shiki: episode behind local progress; push skipped",
			"episode", episode, "prior", prior)
		return shikiSyncedMsg{note: fmt.Sprintf(
			"Shikimori: серия %d — прогресс уже %d, счётчик не откатывается", episode, prior)}
	}

	newRate, err := deps.Shiki.UpdateEpisodes(ctx, shikimoriID, rateID, episode, status)
	if err != nil {
		log.Warn("tui: shiki: progress push failed", "episode", episode, "error", err)
		//nolint:staticcheck // ST1005: user-facing verdict carries the product name (PR80 owner ruling)
		return shikiSyncedMsg{err: fmt.Errorf("Shikimori: ошибка синхронизации: %w", err)}
	}
	// A created rate id is persisted so the next push PATCHes instead
	// of duplicating (SyncEpisodeProgress pattern: the id write
	// survives caller cancellation).
	if rateID == 0 && newRate != 0 && animeID != 0 && deps.History != nil {
		if err := deps.History.SetRateID(context.WithoutCancel(ctx), animeID, newRate); err != nil {
			log.Warn("tui: shiki: persist rate id failed", "anime", animeID, "error", err)
		}
	}
	log.Info("tui: shiki: progress synced",
		"episode", episode, "rate", newRate, "status", status)
	return shikiSyncedMsg{note: fmt.Sprintf("Shikimori: прогресс синхронизирован (эп %d)", episode)}
}

// playCmd runs the player; it settles into playedMsg.
func (s *sessionScreen) playCmd() tea.Cmd {
	snapshot := *s
	return func() tea.Msg {
		msg := snapshot.doPlay()
		return msg
	}
}

// doPlay performs the synchronous play pipeline. It runs on a struct
// snapshot, so the quality used travels back via playedMsg and the
// live model applies it in Update (I9).
func (s *sessionScreen) doPlay() tea.Msg {
	ctx := context.Background()
	ep := s.currentEpisodeData()
	if ep == nil {
		return playedMsg{err: fmt.Errorf("нет серий")}
	}
	if s.pickedVideo.URL == "" {
		return playedMsg{err: fmt.Errorf("поток не выбран")}
	}
	video := s.pickedVideo

	audioURL := ""
	audio, err := s.pickAudio(ctx, ep)
	if err != nil {
		return playedMsg{err: err}
	}
	if audio != nil {
		audioURL = audio.URL
	}

	// Chapters come from the resolve-phase skip verdict (PR61); the
	// cleanup runs when the player exits.
	chapters := s.skipChapters
	if s.skipEpisode != ep.Num {
		chapters = ""
	}
	if chapters != "" && s.skipCleanup != nil {
		defer s.skipCleanup()
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
	return playedMsg{quality: s.lastQuality}
}

// errNoPlayback reports an unwired playback service.
var errNoPlayback = fmt.Errorf("tui: playback service unavailable")

// pickAudio resolves the separate audio track when the dubs differ
// (the SEPARATE case of PR61: the player gets both URLs, mpv joins
// them via --audio-file).
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

// savePlayback persists progress through the history service. The
// error returns to the caller (PR63 #1): the launch line composes a
// failed save into its verdict instead of silently dropping it.
func (s *sessionScreen) saveProgress() error {
	if s.deps == nil || s.deps.History == nil {
		return nil
	}
	ep := s.currentEpisodeData()
	if ep == nil {
		return nil
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
	return s.deps.History.SavePlayback(ctx, rec, ep.Num, s.videoDub, s.audioDub)
}

// handleEpisodeListKey drives «Перейти к серии».
func (s *sessionScreen) handleEpisodeListKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	// PR78 type-to-search: printable keys narrow the list live; the
	// first Esc clears, the second falls through to the Back below.
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
		s.setState(sessionStateMenu)
		return s, nil
	}
	num, _ := resolved.(string)
	for i, n := range s.order {
		if n == num {
			s.currentIdx = i
			break
		}
	}
	s.setState(sessionStateMenu)
	s.buildActionMenu()
	return s, nil
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
		s.setState(sessionStateMenu)
		return s, nil
	}
	id, _ := resolved.(string)
	switch id {
	case "status":
		s.setState(sessionStateInfoStatus)
		s.buildStatusList()
	case "score":
		s.setState(sessionStateInfoScore)
		s.infoPrompt = NewTextPrompt(TextPromptConfig{ID: "info-score", Title: "Введите оценку (0-10):"})
		return s, s.infoPrompt.Init()
	case "rewatches":
		s.setState(sessionStateInfoRewatches)
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
		s.setState(sessionStateInfoMenu)
		return s, nil
	}
	statusKey, _ := resolved.(string)
	s.setState(sessionStateMenu)
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
		s.setState(sessionStateInfoMenu)
		return s, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(resolved.(string)))
	if err != nil {
		s.setStatus("Нужно число")
		return s, nil
	}
	v := value
	// Branch on the ORIGINAL substate before returning to the menu —
	// the score prompt must submit as score, not rewatches (C1).
	wasScore := s.state == sessionStateInfoScore
	s.setState(sessionStateMenu)
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
		s.setStatus("Shikimori отключён — обновление только локально невозможно")
		return nil
	}
	id := s.shikimoriID()
	if id == 0 {
		s.setStatus("Запись не привязана к Shikimori")
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
		s.setState(sessionStateMenu)
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
		s.setState(sessionStateMenu)
		s.setStatus("В диапазоне нет доступных серий")
		return s, nil
	}
	s.setState(sessionStateDownloadMode)
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
		s.setState(sessionStateMenu)
		return s, nil
	}
	mode, _ := resolved.(string)
	s.setState(sessionStateMenu)
	episodes := make([]contracts.Episode, 0, len(s.downloadEpisodes))
	for _, num := range s.downloadEpisodes {
		episodes = append(episodes, s.episodes[num])
	}
	// The dub is resolved PER EPISODE inside the command (PR64 #3):
	// the remembered dub first, per-episode fallback otherwise — the
	// tasks leave this handler without a dub stamp.
	tasks := make([]DownloadTask, 0, len(episodes))
	for _, ep := range episodes {
		tasks = append(tasks, DownloadTask{
			AnimeTitle:  BestDisplayTitle(s.group),
			EpisodeNum:  ep.Num,
			ShikimoriID: s.shikimoriID(),
			Episode:     ep,
			Quality:     s.lastQuality,
		})
	}
	deps := s.deps
	preferred := s.videoDub
	switch mode {
	case "foreground":
		count := len(tasks)
		s.setStatus(fmt.Sprintf("Загрузка %d серий (передний план)…", count))
		s.downloadGen++
		gen := s.downloadGen
		progCh := make(chan string, 16)
		s.downloadProgCh = progCh
		return s, tea.Batch(
			safeCmd(sessionScreenID, func() tea.Msg {
				defer close(progCh)
				return runForegroundDownload(context.Background(), deps, tasks, preferred, progCh)
			}),
			waitDownloadProgress(progCh, gen),
		)
	case "background":
		s.setStatus(fmt.Sprintf("Разрешаю озвучки для %d серий…", len(tasks)))
		return s, safeCmd(sessionScreenID, func() tea.Msg {
			return queueBackgroundDownloads(context.Background(), deps, tasks, preferred)
		})
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
	s.setState(sessionStateMenu)
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
// is disabled only for an episode whose hydration attempt already
// found nothing — an unopened episode stays clickable, because
// clicking it IS the on-demand trigger (PR44 owner model).
// buildActionMenu assembles the action menu; a resumed record adds
// «🔗 Перепривязать» (PR62 #3): the manual re-binding path that
// re-runs the provider fan-out over the stored record (PR74: 🔗, not
// 🔄 — every emoji means exactly one action).
func (s *sessionScreen) buildActionMenu() {
	watchDisabled := false
	if ep := s.currentEpisodeData(); ep != nil && s.hydrated[ep.Num] && len(ep.RawEmbeds) == 0 {
		watchDisabled = true
	}
	choices := []Choice{
		{ID: "watch", Label: "▶ Смотреть", Disabled: watchDisabled},
		{ID: "next", Label: "⏭ След."},
		{ID: "prev", Label: "⏮ Пред."},
		{ID: "jump", Label: "🔢 Перейти к серии"},
		{ID: "redub", Label: "🎨 Сменить озвучку"},
		{ID: "info", Label: "📝 Изменить инфо"},
		{ID: "download", Label: "⬇ Скачать серии"},
		{ID: "refresh", Label: "🔄 Обновить источники"},
	}
	if s.resume != nil {
		choices = append(choices, Choice{ID: "rebind", Label: "🔗 Перепривязать"})
	}
	choices = append(choices, Choice{ID: "exit", Label: "🚪 Выход"})
	s.list = NewPinList(NewMenu(s.renderHeader(), "", choices...), defaultListHeight)
}

// episodeChoices builds the full jump-list choices (one per episode in
// watch order).
func (s *sessionScreen) episodeChoices() []Choice {
	choices := make([]Choice, 0, len(s.order))
	for _, num := range s.order {
		ep := s.episodes[num]
		label := fmt.Sprintf("Серия %s", num)
		if ep.Title != "" {
			label += " — " + ep.Title
		}
		choices = append(choices, Choice{ID: num, Label: label, Value: num})
	}
	return choices
}

// buildEpisodeList builds the jump list with local markers. The PR78
// type-to-search query narrows it live; the cursor stays parked on the
// same episode when the filter keeps it.
func (s *sessionScreen) buildEpisodeList() {
	prev := cursorID(s.episodeList)
	choices := filterChoices(s.episodeChoices(), s.episodeFilter.value())
	s.episodeList = NewPinList(NewMenu("Выберите серию:", "Нет серий", choices...), defaultListHeight)
	for i, ch := range choices {
		if s.localCounts[ch.ID] > 0 {
			s.episodeList.SetMarker(i, "★")
		}
	}
	restoreCursor(s.episodeList, prev)
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
// the header (PR41 B1, PR44 wording): an unopened episode says so
// (sources load on open), an attempted one shows the per-provider
// error summary (truncated) or the genuine no-results verdict.
func (s *sessionScreen) episodeCause(ep *contracts.Episode) string {
	if !s.hydrated[ep.Num] {
		return "источники не запрашивались — откроется при просмотре"
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
		body = theme.Title.Render(s.renderHeader()) + "\n\n" +
			filterLineAbove(s.episodeFilter, s.episodeList.Render())
	case sessionStateDubAudio:
		body = themedList(s.dubList)
	case sessionStateQuality:
		body = themedList(s.qualityList)
	case sessionStateFormat:
		body = themedList(s.formatList)
	case sessionStateBuffering:
		body = theme.Title.Render(s.renderHeader())
		if s.statusVisible() {
			body += "\n" + theme.Success.Render(s.status)
		}
	case sessionStatePlaying:
		body = theme.Title.Render(s.renderHeader())
		if s.statusVisible() {
			body += "\n" + theme.Success.Render(s.status)
		}
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
	if s.statusVisible() && s.state != sessionStatePlaying {
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
