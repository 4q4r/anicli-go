package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// fakeEpisode implements EpisodeService.
type fakeEpisode struct {
	episodes map[string][]contracts.Episode
	streams  map[string]contracts.MediaStream
	errs     map[string]error
}

func (f *fakeEpisode) GetEpisodes(_ context.Context, providerID, _ string) ([]contracts.Episode, error) {
	if err := f.errs[providerID]; err != nil {
		return nil, err
	}
	return f.episodes[providerID], nil
}

func (f *fakeEpisode) ResolveStream(_ context.Context, _ string, _ contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if s, ok := f.streams[dubID]; ok {
		return s, nil
	}
	return contracts.MediaStream{}, errors.New("no stream")
}

var _ EpisodeService = (*fakeEpisode)(nil)

// fakePlayback implements PlaybackService.
type fakePlayback struct {
	played   []PlayRequest
	skipIDs  []int64 // shikimori ids seen by ResolveSkips
	skipPath string
	err      error
}

func (f *fakePlayback) ResolveSkips(_ context.Context, shikimoriID int64, _ float64) (string, func(), error) {
	f.skipIDs = append(f.skipIDs, shikimoriID)
	cleanup := func() {}
	if f.skipPath != "" {
		return f.skipPath, cleanup, nil
	}
	return "", cleanup, nil
}

func (f *fakePlayback) Play(_ context.Context, req PlayRequest) error {
	f.played = append(f.played, req)
	return f.err
}

var _ PlaybackService = (*fakePlayback)(nil)

func testEpisodeSet() map[string][]contracts.Episode {
	return map[string][]contracts.Episode{
		"animego": {
			{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
			{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
		},
		"anilib": {
			{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1b"}}},
			{Num: "3", RawID: "b3", RawEmbeds: map[string][]string{"AniLib": {"u3b"}}},
		},
	}
}

// newSessionForTests builds a session screen with merged episodes
// loaded synchronously (the async path is covered by the search
// tests).
func newSessionForTests(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	// Synchronously load episodes (the Init cmd in production).
	s.loadEpisodesSync()
	return s
}

// TestSessionEpisodesMerged: the session merges episodes from every
// source with provider-prefixed dubs.
func TestSessionEpisodesMerged(t *testing.T) {
	s := newSessionForTests(t)
	if len(s.order) != 3 || s.order[0] != "1" || s.order[1] != "2" || s.order[2] != "3" {
		t.Fatalf("want merged episodes [1 2 3], got %v", s.order)
	}
	if s.dubStats["[animego] Дубль 1"] != 2 || s.dubStats["[anilib] AniLib"] != 2 {
		t.Fatalf("wrong dub stats: %v", s.dubStats)
	}
}

// TestSessionMenuActions: the action menu covers the Python
// session_loop entries; exit pops to root.
func TestSessionMenuActions(t *testing.T) {
	s := newSessionForTests(t)
	v := s.View().Content
	for _, want := range []string{
		"▶ Смотреть",
		"⏭ След.",
		"⏮ Пред.",
		"🔢 Перейти к серии",
		"🎨 Сменить озвучку",
		"📝 Изменить инфо",
		"⬇ Скачать серии",
		"🚪 Выход",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("session menu must contain %q:\n%s", want, v)
		}
	}

	t.Run("exit pops to root", func(t *testing.T) {
		idx := sessionActionIndex(s, "exit")
		s.list.Jump(idx)
		_, cmd := s.Update(enter())
		msg := cmd()
		if _, ok := msg.(popToRootMsg); !ok {
			t.Fatalf("Выход must pop to root, got %#v", msg)
		}
	})

	t.Run("esc pops to root (I2 back from session)", func(t *testing.T) {
		_, cmd := s.Update(esc())
		msg := cmd()
		if _, ok := msg.(popMsg); !ok {
			t.Fatalf("esc must leave the session, got %#v", msg)
		}
	})
}

// TestSessionEpisodeNavigation: next/prev clamp and jump works.
func TestSessionEpisodeNavigation(t *testing.T) {
	s := newSessionForTests(t)

	t.Run("next clamps at last", func(t *testing.T) {
		s.jumpTo("3")
		idx := sessionActionIndex(s, "next")
		s.list.Jump(idx)
		next, _ := s.Update(enter())
		if next.(*sessionScreen).currentEpisode() != "3" {
			t.Fatalf("next at last must clamp, got %q", next.(*sessionScreen).currentEpisode())
		}
	})

	t.Run("prev clamps at first", func(t *testing.T) {
		s.jumpTo("1")
		idx := sessionActionIndex(s, "prev")
		s.list.Jump(idx)
		next, _ := s.Update(enter())
		if next.(*sessionScreen).currentEpisode() != "1" {
			t.Fatalf("prev at first must clamp, got %q", next.(*sessionScreen).currentEpisode())
		}
	})

	t.Run("jump via episode list", func(t *testing.T) {
		s.jumpTo("1")
		idx := sessionActionIndex(s, "jump")
		s.list.Jump(idx)
		next, _ := s.Update(enter())
		ss := next.(*sessionScreen)
		if ss.state != sessionStateEpisodeList {
			t.Fatalf("jump must open the episode list, got %v", ss.state)
		}
		// Move to episode 3 (index 3 = Back0, ep1, ep2, ep3).
		ss.episodeList.Jump(3)
		next, _ = ss.Update(enter())
		if next.(*sessionScreen).currentEpisode() != "3" {
			t.Fatalf("pick must set current episode, got %q", next.(*sessionScreen).currentEpisode())
		}
		if next.(*sessionScreen).state != sessionStateMenu {
			t.Fatalf("after pick the menu returns, got %v", next.(*sessionScreen).state)
		}
	})
}

// TestSessionDubSelect: watching without dubs runs the interactive
// video-then-audio selection, then quality, then plays.
func TestSessionDubSelect(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
					"720":  {URL: "v720"},
				}},
				"[anilib] AniLib": {DubName: "d2", Links: map[string]contracts.VideoSource{
					"1080": {URL: "a1080"},
				}},
			},
		},
		Playback: &fakePlayback{},
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	// Смотреть without dubs → video dub select.
	idx := sessionActionIndex(s, "watch")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	if next.(*sessionScreen).state != sessionStateDubVideo {
		t.Fatalf("watch without dubs must open the video dub select, got %v", next.(*sessionScreen).state)
	}

	// Pick the animego dub (episode 1 embeds: [animego] Дубль 1, [anilib] AniLib).
	ss := next.(*sessionScreen)
	found := false
	for i, c := range ss.dubList.Menu().Items {
		if c.ID == "[animego] Дубль 1" {
			ss.dubList.Jump(i)
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("video dub list must contain the animego dub: %+v", ss.dubList.Menu().Items)
	}
	next, _ = ss.Update(enter())
	if next.(*sessionScreen).state != sessionStateDubAudio {
		t.Fatalf("after video dub the audio dub select opens, got %v", next.(*sessionScreen).state)
	}

	// Audio pick: «⭐ Как видео» style shortcut = same key.
	ss = next.(*sessionScreen)
	ss.dubList.Jump(1) // first non-Back entry is the "как видео" option
	next, _ = ss.Update(enter())
	if next.(*sessionScreen).state != sessionStateQuality {
		t.Fatalf("after audio dub the quality picker opens, got %v", next.(*sessionScreen).state)
	}

	// Quality pick triggers the play command; with the fake services
	// the whole chain resolves synchronously into playedMsg.
	ss = next.(*sessionScreen)
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("quality pick must schedule the play command")
	}
	msg := cmd()
	pm, ok := msg.(playedMsg)
	if !ok {
		t.Fatalf("play must settle into playedMsg, got %#v", msg)
	}
	if pm.err != nil {
		t.Fatalf("fake playback must succeed, got %v", pm.err)
	}
	if ss.videoDub != "[animego] Дубль 1" || ss.audioDub != "[animego] Дубль 1" {
		t.Fatalf("dubs must be recorded, got %q/%q", ss.videoDub, ss.audioDub)
	}
	pb := deps.Playback.(*fakePlayback)
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback must run, got %d", len(pb.played))
	}
	if pb.played[0].URL != "v1080" {
		t.Fatalf("auto quality must pick the best variant, got %q", pb.played[0].URL)
	}
	if !strings.Contains(pb.played[0].Title, "Тайтл - 1") {
		t.Fatalf("player title must carry anime and episode, got %q", pb.played[0].Title)
	}
}

// TestSessionChangeDub: «Сменить озвучку» resets dub preferences.
func TestSessionChangeDub(t *testing.T) {
	s := newSessionForTests(t)
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	idx := sessionActionIndex(s, "redub")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.videoDub != "" || ss.audioDub != "" {
		t.Fatalf("сменить озвучку must reset dubs, got %q/%q", ss.videoDub, ss.audioDub)
	}
}

// TestSessionEpisodeListMarkers: episodes available locally carry the
// ★ marker and the header shows «локально: N».
func TestSessionEpisodeListMarkers(t *testing.T) {
	s := newSessionForTests(t)
	s.localCounts = map[string]int{"1": 2}
	v := s.renderEpisodeList()
	if !strings.Contains(v, "★") {
		t.Fatalf("local episodes must be starred:\n%s", v)
	}
	if !strings.Contains(s.renderHeader(), "локально: 2") {
		t.Fatalf("header must show the local count: %q", s.renderHeader())
	}
}

// TestSessionInfoMenu: «Изменить инфо» opens the manual shikimori
// update submenu with the RU status labels.
func TestSessionInfoMenu(t *testing.T) {
	s := newSessionForTests(t)
	idx := sessionActionIndex(s, "info")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateInfoMenu {
		t.Fatalf("info must open the submenu, got %v", ss.state)
	}
	v := ss.renderInfoMenu()
	for _, want := range []string{"Статус", "Оценка", "Пересмотры"} {
		if !strings.Contains(v, want) {
			t.Fatalf("info menu must contain %q:\n%s", want, v)
		}
	}
}

// fakeDownload implements DownloadService.
type fakeDownload struct {
	submitted []DownloadTask
	downloads []DownloadTask
}

func (f *fakeDownload) Download(_ context.Context, task DownloadTask) error {
	f.downloads = append(f.downloads, task)
	return nil
}

func (f *fakeDownload) Submit(task DownloadTask) { f.submitted = append(f.submitted, task) }

func (f *fakeDownload) ActiveBanner() string { return "" }

var _ DownloadService = (*fakeDownload)(nil)

// TestSessionDownloadRange: «Скачать серии» asks a range, parses it,
// and resolves episodes for the current dub.
func TestSessionDownloadRange(t *testing.T) {
	s := newSessionForTests(t)
	s.deps.Download = &fakeDownload{}
	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	ss.rangeInput.typeText("1-2")
	next, cmd := ss.Update(enter())
	_ = cmd
	ss = next.(*sessionScreen)
	if len(ss.downloadEpisodes) != 2 {
		t.Fatalf("range 1-2 must select two episodes, got %v", ss.downloadEpisodes)
	}
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("after the range the mode menu opens, got %v", ss.state)
	}
	v := ss.View().Content
	if !strings.Contains(v, "Передний план") || !strings.Contains(v, "Фон") {
		t.Fatalf("download modes must be offered:\n%s", v)
	}
}

// jumpTo positions the session on an episode number (test helper).
func (s *sessionScreen) jumpTo(num string) {
	for i, n := range s.order {
		if n == num {
			s.currentIdx = i
			s.buildActionMenu()
			return
		}
	}
}

func sessionActionIndex(s *sessionScreen, id string) int {
	for i, c := range s.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func down() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyDown} }

// openInfo enters the «Изменить инфо» submenu from the action menu.
func openInfo(s *sessionScreen) *sessionScreen {
	idx := sessionActionIndex(s, "info")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	return next.(*sessionScreen)
}

// shikiSessionForTests builds a loaded session whose primary carries a
// shikimori binding, with the given shiki/history fakes attached.
func shikiSessionForTests(t *testing.T, shiki *fakeShiki, hist *fakeHistory) *sessionScreen {
	t.Helper()
	if hist == nil {
		hist = &fakeHistory{} // avoid a typed-nil interface trap
	}
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}, Shiki: shiki, History: hist, Log: testLogger()}
	group := []contracts.SearchResult{{
		Title: "Тайтл", URL: "u1", SourceID: "animego",
		Meta: map[string]any{"shikimori_id": int64(21)},
	}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s
}

// TestSessionScoreSubmitsAsScore (C1): a score entered through the
// info menu reaches UpdateStatus as Score with nil Rewatches — not as
// a rewatch count.
func TestSessionScoreSubmitsAsScore(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	s := shikiSessionForTests(t, shiki, nil)

	ss := openInfo(s)
	next, _ := ss.Update(down())  // cursor → Статус
	next, _ = next.Update(down()) // cursor → Оценка
	next, _ = next.Update(enter())
	prompt := next.(*sessionScreen)
	if prompt.state != sessionStateInfoScore {
		t.Fatalf("info menu must reach the score prompt, got %v", prompt.state)
	}
	prompt.infoPrompt.typeText("9")
	next, cmd := prompt.Update(enter())
	if cmd == nil {
		t.Fatalf("score submit must dispatch the shikimori update")
	}
	if _, ok := cmd().(shikiUpdatedMsg); !ok {
		t.Fatalf("status patch must settle into shikiUpdatedMsg, got %T", cmd())
	}
	if len(shiki.updates) != 1 {
		t.Fatalf("exactly one update expected, got %d", len(shiki.updates))
	}
	u := shiki.updates[0]
	if u.shikimoriID != 21 || u.score == nil || *u.score != 9 || u.rewatches != nil {
		t.Fatalf("score must submit as score (rewatches nil), got %+v", u)
	}
	// The prompt returns to the menu with the dedicated status line.
	next, _ = next.Update(shikiUpdatedMsg{})
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("after submit the menu returns, got %v", ss.state)
	}
	if !contains(ss.status, "Информация обновлена") {
		t.Fatalf("dedicated success status expected, got %q", ss.status)
	}
}

// TestSessionRewatchesSubmit (C1 complement): the rewatches prompt
// submits as Rewatches with nil Score.
func TestSessionRewatchesSubmit(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	s := shikiSessionForTests(t, shiki, nil)

	ss := openInfo(s)
	next, _ := ss.Update(down())  // Статус
	next, _ = next.Update(down()) // Оценка
	next, _ = next.Update(down()) // Пересмотры
	next, _ = next.Update(enter())
	prompt := next.(*sessionScreen)
	if prompt.state != sessionStateInfoRewatches {
		t.Fatalf("must reach the rewatches prompt, got %v", prompt.state)
	}
	prompt.infoPrompt.typeText("3")
	_, cmd := prompt.Update(enter())
	if cmd == nil {
		t.Fatalf("rewatches submit must dispatch")
	}
	cmd()
	u := shiki.updates[0]
	if u.rewatches == nil || *u.rewatches != 3 || u.score != nil {
		t.Fatalf("rewatches must submit as rewatches (score nil), got %+v", u)
	}
}

// TestSessionInfoMenuPersists (C2): the info submenu survives
// keypresses — Down moves the cursor instead of resetting it.
func TestSessionInfoMenuPersists(t *testing.T) {
	s := newSessionForTests(t)
	ss := openInfo(s)
	next, _ := ss.Update(down())
	next, _ = next.Update(enter())
	got := next.(*sessionScreen)
	if got.state != sessionStateInfoStatus {
		t.Fatalf("Down+Enter in the info menu must reach the status picker, got %v", got.state)
	}
}

// TestSessionStatusPickDispatches (C2): the status picker persists
// across keypresses; selecting a status dispatches UpdateStatus with
// the picked key.
func TestSessionStatusPickDispatches(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	s := shikiSessionForTests(t, shiki, nil)

	ss := openInfo(s)
	next, _ := ss.Update(down()) // Статус
	next, _ = next.Update(enter())
	sp := next.(*sessionScreen)
	if sp.state != sessionStateInfoStatus {
		t.Fatalf("must open the status picker, got %v", sp.state)
	}
	// Down lands on «Смотрю»; Enter resolves it (not Back).
	next, _ = sp.Update(down())
	_, cmd := next.Update(enter())
	if cmd == nil {
		t.Fatalf("status pick must dispatch UpdateStatus")
	}
	if _, ok := cmd().(shikiUpdatedMsg); !ok {
		t.Fatalf("status pick must settle into shikiUpdatedMsg, got %T", cmd())
	}
	if len(shiki.updates) != 1 || shiki.updates[0].status != "watching" {
		t.Fatalf("watching must be dispatched, got %+v", shiki.updates)
	}
}

// TestSessionDownloadForegroundDispatch (C2 + I8): the download-mode
// menu persists; picking «Передний план» actually downloads the range
// and the settle reaches the status line.
func TestSessionDownloadForegroundDispatch(t *testing.T) {
	dl := &fakeDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl

	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("after the range the mode menu opens, got %v", ss.state)
	}
	// Down → «Передний план» (first body item), Enter dispatches.
	next, _ = ss.Update(down())
	next, cmd := next.Update(enter())
	if cmd == nil {
		t.Fatalf("foreground pick must dispatch the download")
	}
	settled, ok := cmd().(downloadSettledMsg)
	if !ok {
		t.Fatalf("foreground download must settle into downloadSettledMsg, got %T", cmd())
	}
	if settled.err != nil {
		t.Fatalf("fake download must succeed, got %v", settled.err)
	}
	if len(dl.downloads) != 2 {
		t.Fatalf("foreground must download both episodes, got %d", len(dl.downloads))
	}
	next, _ = next.Update(settled)
	if !contains(next.(*sessionScreen).status, "Загружено") {
		t.Fatalf("settle must update the status line, got %q", next.(*sessionScreen).status)
	}
}

// TestSessionDownloadBackgroundSubmits (C2): the background mode
// submits tasks to the manager-backed service.
func TestSessionDownloadBackgroundSubmits(t *testing.T) {
	dl := &fakeDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl

	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(down())
	next, _ = next.Update(down()) // «Фон»
	next, _ = next.Update(enter())
	if len(dl.submitted) != 1 {
		t.Fatalf("background pick must submit one task, got %d", len(dl.submitted))
	}
	if !contains(next.(*sessionScreen).status, "фон") {
		t.Fatalf("background status expected, got %q", next.(*sessionScreen).status)
	}
}

// TestSessionDownloadSettledFailure (I8): a failed foreground download
// surfaces its error on the status line.
func TestSessionDownloadSettledFailure(t *testing.T) {
	dl := &errDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl
	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(down())
	next, cmd := next.Update(enter())
	settled := cmd().(downloadSettledMsg)
	if settled.err == nil {
		t.Fatalf("download failure must be carried")
	}
	next, _ = next.Update(settled)
	if !contains(next.(*sessionScreen).status, "Ошибка загрузки") {
		t.Fatalf("failure must surface on the status line, got %q", next.(*sessionScreen).status)
	}
}

// errDownload fails every foreground download.
type errDownload struct{ fakeDownload }

func (f *errDownload) Download(_ context.Context, _ DownloadTask) error {
	return errors.New("disk full")
}

// watchToQuality drives the dub video→audio picks and the stream
// resolve, landing on the quality picker with links loaded.
func watchToQuality(t *testing.T, s *sessionScreen) *sessionScreen {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.dubList.Jump(1)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss.dubList.Jump(1)
	next, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("audio pick must schedule the stream resolve")
	}
	msg := cmd()
	if _, ok := msg.(streamResolvedMsg); !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	next, _ = next.Update(msg)
	return next.(*sessionScreen)
}

// TestSessionQualityMemory (I9): the quality used by one playback is
// remembered on the live model, so the next auto play prefers it.
func TestSessionQualityMemory(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
					"720":  {URL: "v720"},
				}},
			},
		},
		Playback: pb,
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	// First watch: pick 720 explicitly (items: Back, auto, 1080, 720).
	ss := watchToQuality(t, s)
	ss.qualityList.Jump(3)
	next, cmd := ss.Update(enter())
	msg := cmd().(playedMsg)
	if msg.err != nil {
		t.Fatalf("play must succeed, got %v", msg.err)
	}
	if msg.quality != "720" {
		t.Fatalf("playedMsg must carry the used quality, got %q", msg.quality)
	}
	next, _ = next.Update(msg)
	ss = next.(*sessionScreen)
	if ss.lastQuality != "720" {
		t.Fatalf("quality must be remembered on the model, got %q", ss.lastQuality)
	}

	// Second watch: auto quality must resolve to the remembered 720.
	ss.list.Jump(sessionActionIndex(ss, "watch"))
	next, cmd = ss.Update(enter())
	srMsg := cmd()
	if _, ok := srMsg.(streamResolvedMsg); !ok {
		t.Fatalf("stream resolve expected, got %T", srMsg)
	}
	next, _ = next.Update(srMsg)
	ss = next.(*sessionScreen)
	ss.qualityList.Jump(1) // Авто
	_, cmd = ss.Update(enter())
	if _, ok := cmd().(playedMsg); !ok {
		t.Fatalf("second play must settle, got %T", cmd())
	}
	if len(pb.played) != 2 {
		t.Fatalf("two playbacks expected, got %d", len(pb.played))
	}
	if pb.played[1].URL != "v720" {
		t.Fatalf("auto must reuse the remembered quality, got %q", pb.played[1].URL)
	}
}

// TestSessionStatusUpdateRateIDReuse (I10): the first patch creates
// the rate and persists its id; the second patch PATCHes the stored
// id instead of creating a duplicate.
func TestSessionStatusUpdateRateIDReuse(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	rec := &storage.AnimeProgress{ID: 5, Title: "Тайтл", ShikimoriID: ptrTo(int64(21))}
	hist := &fakeHistory{byShiki: map[int64]*storage.AnimeProgress{21: rec}}
	s := shikiSessionForTests(t, shiki, hist)

	pickStatus := func(sess *sessionScreen) (*sessionScreen, tea.Cmd) {
		ss := openInfo(sess)
		next, _ := ss.Update(down()) // Статус
		next, _ = next.Update(enter())
		sp := next.(*sessionScreen)
		next, _ = sp.Update(down()) // «Смотрю»
		screen, cmd := next.Update(enter())
		return screen.(*sessionScreen), cmd
	}

	// First patch: drive the real dispatch — the create path must
	// persist the new rate id against the history row.
	next, cmd := pickStatus(s)
	msg := cmd().(shikiUpdatedMsg)
	if msg.rateID == 0 {
		t.Fatalf("create must return a rate id")
	}
	if hist.rateIDs[5] != msg.rateID {
		t.Fatalf("rate id must be persisted after create, got %v want %d", hist.rateIDs, msg.rateID)
	}
	screen, _ := next.Update(msg)
	next = screen.(*sessionScreen)

	// Second patch on the same session: PATCH the stored id.
	_, cmd2 := pickStatus(next)
	_ = cmd2()
	if len(shiki.rateIDs) != 2 {
		t.Fatalf("two updates expected, got %v", shiki.rateIDs)
	}
	if shiki.rateIDs[0] != 0 {
		t.Fatalf("first update must be a create (rate id 0), got %d", shiki.rateIDs[0])
	}
	if shiki.rateIDs[1] != msg.rateID {
		t.Fatalf("second update must PATCH the stored rate id, got %d want %d",
			shiki.rateIDs[1], msg.rateID)
	}
}

// TestSessionStatusMessageDistinct (I10): a status patch never
// reports playback verdicts.
func TestSessionStatusMessageDistinct(t *testing.T) {
	s := newSessionForTests(t)
	next, _ := s.Update(shikiUpdatedMsg{})
	ss := next.(*sessionScreen)
	if contains(ss.status, "Воспроизведение") {
		t.Fatalf("status patch must not report playback verdicts, got %q", ss.status)
	}
	if !contains(ss.status, "Информация обновлена") {
		t.Fatalf("dedicated status expected, got %q", ss.status)
	}

	next, _ = s.Update(shikiUpdatedMsg{err: errors.New("boom")})
	if !contains(next.(*sessionScreen).status, "Ошибка") {
		t.Fatalf("failure must surface, got %q", next.(*sessionScreen).status)
	}
}

// TestSessionResumeCarriesShikimoriBinding (I5): a resumed session
// restores episode and dubs from the record and passes the bound
// shikimori id to the skip resolver.
func TestSessionResumeCarriesShikimoriBinding(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"1080": {URL: "v1080"}}},
			},
		},
		Playback: pb,
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	rec := storage.AnimeProgress{
		ID: 9, Title: "Тайтл", CurrentEpisode: "2",
		ShikimoriID: ptrTo(int64(33)),
		VideoDub:    ptrTo("[animego] Дубль 1"),
		AudioDub:    ptrTo("[animego] Дубль 1"),
	}
	s := newResumedSession(deps, group[0], group, rec)
	s.loadEpisodesSync()

	if s.currentEpisode() != "2" {
		t.Fatalf("resume must restore the saved episode, got %q", s.currentEpisode())
	}
	if s.videoDub != "[animego] Дубль 1" || s.audioDub != "[animego] Дубль 1" {
		t.Fatalf("resume must restore dubs, got %q/%q", s.videoDub, s.audioDub)
	}
	if s.shikimoriID() != 33 {
		t.Fatalf("resume must carry the shikimori binding, got %d", s.shikimoriID())
	}

	// Watch straight to dispatch: dubs are already set, so watch goes
	// directly to the stream resolve.
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, cmd := s.Update(enter())
	msg := cmd()
	if _, ok := msg.(streamResolvedMsg); !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	next, _ = next.Update(msg)
	ss := next.(*sessionScreen)
	ss.qualityList.Jump(1) // Авто
	_, cmd = ss.Update(enter())
	if _, ok := cmd().(playedMsg); !ok {
		t.Fatalf("play must settle, got %T", cmd())
	}
	if len(pb.skipIDs) == 0 || pb.skipIDs[0] != 33 {
		t.Fatalf("skip resolver must receive the bound id, got %v", pb.skipIDs)
	}
}

// TestSessionShikiResolveFreshSearch (I5): fresh sessions resolve the
// shikimori id in the background (python _resolve_shikimori_info:
// best similarity > 0.6 wins; a weak match skips the binding
// quietly).
func TestSessionShikiResolveFreshSearch(t *testing.T) {
	collect := func(cmd tea.Cmd) shikiBoundMsg {
		var bound shikiBoundMsg
		grab := func(m tea.Msg) {
			if b, ok := m.(shikiBoundMsg); ok {
				bound = b
			}
		}
		switch v := cmd().(type) {
		case tea.BatchMsg:
			for _, sub := range v {
				grab(sub())
			}
		default:
			grab(v)
		}
		return bound
	}

	t.Run("confident match binds", func(t *testing.T) {
		shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Тайтл": 55}}
		deps := &Deps{
			Episode: &fakeEpisode{episodes: testEpisodeSet()},
			Shiki:   shiki,
			Log:     testLogger(),
		}
		group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
		s := NewSessionScreen(deps, group[0], group)
		bound := collect(s.Init())
		if bound.id != 55 {
			t.Fatalf("exact title must bind, got %+v", bound)
		}
		next, _ := s.Update(bound)
		if got := next.(*sessionScreen).shikimoriID(); got != 55 {
			t.Fatalf("binding must land in primary.Meta, got %d", got)
		}
		if len(shiki.queries) != 1 || shiki.queries[0] != "Тайтл" {
			t.Fatalf("resolver must search by title, got %v", shiki.queries)
		}
	})

	t.Run("weak match skips quietly", func(t *testing.T) {
		shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Совсем Другое Название": 77}}
		deps := &Deps{
			Episode: &fakeEpisode{episodes: testEpisodeSet()},
			Shiki:   shiki,
			Log:     testLogger(),
		}
		group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
		s := NewSessionScreen(deps, group[0], group)
		bound := collect(s.Init())
		if bound.id != 0 {
			t.Fatalf("weak match must not bind, got %+v", bound)
		}
		next, _ := s.Update(bound)
		if got := next.(*sessionScreen).shikimoriID(); got != 0 {
			t.Fatalf("no binding expected, got %d", got)
		}
	})
}
