package tui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
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
	langs    map[string]string
}

func (f *fakeEpisode) GetEpisodes(_ context.Context, providerID, _ string) ([]contracts.Episode, error) {
	if err := f.errs[providerID]; err != nil {
		return nil, err
	}
	return f.episodes[providerID], nil
}

func (f *fakeEpisode) ContentLanguage(providerID string) string {
	return f.langs[providerID]
}

// HydrateDubs is a no-op: by default test fixtures list eager embeds.
func (f *fakeEpisode) HydrateDubs(_ context.Context, _ string, episode contracts.Episode) (contracts.Episode, error) {
	return episode, nil
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
	skipNote string // PR61: the note surfaced next to «Запуск mpv…»
	err      error
	// playHook runs before the request is recorded; returning an error
	// fails the play (tests capture at-play-time file state here).
	playHook func(PlayRequest) error
}

func (f *fakePlayback) ResolveSkips(_ context.Context, shikimoriID int64, _ float64) (string, func(), string, error) {
	f.skipIDs = append(f.skipIDs, shikimoriID)
	cleanup := func() {}
	if f.skipPath != "" {
		return f.skipPath, cleanup, f.skipNote, nil
	}
	return "", cleanup, f.skipNote, nil
}

func (f *fakePlayback) Play(ctx context.Context, req PlayRequest) error {
	if f.playHook != nil {
		if err := f.playHook(req); err != nil {
			return err
		}
	}
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
// session_loop entries PLUS the PR43 «🔄 Обновить источники» recovery
// action (the PR44 «Формат» toggle moved into the pre-play selector),
// in order, with the pinned exit row last; exit pops to root.
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
		"🔄 Обновить источники",
		"🚪 Выход",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("session menu must contain %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Формат:") {
		t.Fatalf("session menu must NOT carry the format toggle:\n%s", v)
	}
	// 9 actions + the pinned Back row (I1).
	if got := len(s.list.Menu().Items); got != 10 {
		t.Fatalf("session menu rows = %d, want 10 (9 actions + Back)", got)
	}
	last := s.list.Menu().Items[9]
	if last.ID != BackID {
		t.Fatalf("last menu row = %q, want the pinned Back entry", last.ID)
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
		// Move to episode 3 (index 2 = ep1, ep2, ep3).
		ss.episodeList.Jump(2)
		next, _ = ss.Update(enter())
		if next.(*sessionScreen).currentEpisode() != "3" {
			t.Fatalf("pick must set current episode, got %q", next.(*sessionScreen).currentEpisode())
		}
		if next.(*sessionScreen).state != sessionStateMenu {
			t.Fatalf("after pick the menu returns, got %v", next.(*sessionScreen).state)
		}
	})
}

// TestSessionMergedStreamList (PR61): with two providers contributing
// dubs, the fresh watch flow opens ONE merged picker — no dub-video
// prompt, no provider gate — whose entries are labeled
// quality · dub [provider] · coverage and sort quality-descending.
func TestSessionMergedStreamList(t *testing.T) {
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

	ss := watchStreaming(t, s)
	if ss.state != sessionStateQuality {
		t.Fatalf("watch without dubs must open the merged stream list, got %v", ss.state)
	}
	labels := []string{}
	for _, c := range ss.qualityList.Menu().Items {
		labels = append(labels, c.Label)
	}
	want := []string{
		"1080p · AniLib [anilib] · 2 сер.",
		"1080p · Дубль 1 [animego] · 2 сер.",
		"720p · Дубль 1 [animego] · 2 сер.",
	}
	if len(labels) != len(want)+1 { // entries + the pinned Back row
		t.Fatalf("merged list labels = %v, want %v (+Back)", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("merged list labels = %v, want %v", labels, want)
		}
	}
}

// TestSessionMergedPickMuxedSingleURL (PR61): picking a merged entry
// continues to the audio prompt; «⭐ Как видео» plays ONE url — the
// muxed case, no separate audio file.
func TestSessionMergedPickMuxedSingleURL(t *testing.T) {
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

	// Watch → merged list → pick the animego 720 entry (sorted last).
	ss := watchStreaming(t, s)
	ss.qualityList.Jump(2)
	next, _ := ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDubAudio {
		t.Fatalf("a fresh pick must continue to the audio prompt, got %v", ss.state)
	}
	if ss.videoDub != "[animego] Дубль 1" || ss.lastQuality != "720" {
		t.Fatalf("the pick must record dub and quality, got %q/%q",
			ss.videoDub, ss.lastQuality)
	}
	// «⭐ Как видео» is the first row.
	ss.dubList.Jump(0)
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("audio pick must schedule the play")
	}
	msg := cmd()
	pm, ok := msg.(playedMsg)
	if !ok {
		t.Fatalf("play must settle into playedMsg, got %#v", msg)
	}
	if pm.err != nil {
		t.Fatalf("fake playback must succeed, got %v", pm.err)
	}
	if ss.audioDub != "[animego] Дубль 1" {
		t.Fatalf("⭐ must reuse the video dub, got %q", ss.audioDub)
	}
	pb := deps.Playback.(*fakePlayback)
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback must run, got %d", len(pb.played))
	}
	if pb.played[0].URL != "v720" {
		t.Fatalf("the picked quality must play, got %q", pb.played[0].URL)
	}
	if pb.played[0].AudioURL != "" {
		t.Fatalf("the muxed case must play a single url, got audio %q",
			pb.played[0].AudioURL)
	}
	if !strings.Contains(pb.played[0].Title, "Тайтл - 1") {
		t.Fatalf("player title must carry anime and episode, got %q", pb.played[0].Title)
	}
}

// TestSessionMergedPickSeparateAudio (PR61): picking another dub as
// the audio track feeds the player BOTH urls (the SEPARATE case —
// mpv joins them via --audio-file).
func TestSessionMergedPickSeparateAudio(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
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

	ss := watchStreaming(t, s)
	ss.qualityList.Jump(0) // anilib 1080 (lexicographic dub order)
	ss.Update(enter())
	if ss.state != sessionStateDubAudio {
		t.Fatalf("audio prompt expected, got %v", ss.state)
	}
	// The second row is the animego dub (the first is ⭐).
	items := ss.dubList.Menu().Items
	if len(items) != 3 { // ⭐ + animego + Back
		t.Fatalf("audio rows = %d, want 3 (⭐ + animego + Back)", len(items))
	}
	if !strings.Contains(items[0].Label, "⭐ Как видео") {
		t.Fatalf("the ⭐ row must lead the audio prompt, got %q", items[0].Label)
	}
	ss.dubList.Jump(1)
	_, cmd := ss.Update(enter())
	pm, ok := cmd().(playedMsg)
	if !ok || pm.err != nil {
		t.Fatalf("play must settle clean, got %#v", cmd())
	}
	pb := deps.Playback.(*fakePlayback)
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback must run, got %d", len(pb.played))
	}
	if pb.played[0].URL != "a1080" || pb.played[0].AudioURL != "v1080" {
		t.Fatalf("separate audio must feed both urls, got %q + %q",
			pb.played[0].URL, pb.played[0].AudioURL)
	}
}

// TestSessionBackFromAudioReturnsToStreamList (PR61): Back from the
// audio prompt reopens the merged stream list from cache — python's
// loop back to the video prompt, without a re-resolve.
func TestSessionBackFromAudioReturnsToStreamList(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
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

	ss := watchStreaming(t, s)
	ss.qualityList.Jump(0)
	ss.Update(enter())
	if ss.state != sessionStateDubAudio {
		t.Fatalf("audio prompt expected, got %v", ss.state)
	}
	// The pinned Back row returns to the merged list (Esc would cancel
	// the whole substate to the menu by design).
	items := ss.dubList.Menu().Items
	ss.dubList.Jump(len(items) - 1)
	ss.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if ss.state != sessionStateQuality {
		t.Fatalf("Back from audio must reopen the stream list, got %v", ss.state)
	}
	if len(ss.qualityList.Menu().Items) != 3 { // 2 entries + Back
		t.Fatalf("the cached entries must render, got %d rows",
			len(ss.qualityList.Menu().Items))
	}
}

// TestSessionRememberedDubsPlayStraight (PR61 regression): with both
// dubs remembered and available the watch flow plays with NO pick
// prompts — no provider gate, no dub lists, no quality list (python
// resolve_dubs_smart parity); the remembered quality is reused.
func TestSessionRememberedDubsPlayStraight(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
					"720":  {URL: "v720"},
				}},
			},
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	s.lastQuality = "720"

	// Watch → format pick → scoped resolve settles → plays at once.
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateFormat {
		t.Fatalf("watch must open the format selector, got %v", ss.state)
	}
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("remembered dubs must resolve straight away")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("scoped resolve expected, got %T", cmd())
	}
	_, play := ss.Update(sr)
	if play == nil {
		t.Fatalf("the scoped settle must launch playback")
	}
	pm, ok := play().(playedMsg)
	if !ok {
		t.Fatalf("play must settle into playedMsg, got %#v", play())
	}
	if pm.err != nil {
		t.Fatalf("playback must succeed, got %v", pm.err)
	}
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback must run, got %d", len(pb.played))
	}
	if pb.played[0].URL != "v720" {
		t.Fatalf("the remembered quality must be reused, got %q", pb.played[0].URL)
	}
	if pb.played[0].AudioURL != "" {
		t.Fatalf("muxed remembered pair plays a single url, got %q", pb.played[0].AudioURL)
	}
}

// TestSessionDubSelectLanguageTags pins the [RU]/[JA] dub prefixes
// (PR23) on the merged stream list rows (PR61): a provider with a
// known content language tags its entries, one without renders the
// plain name.
func TestSessionDubSelectLanguageTags(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			langs:    map[string]string{"animego": "ru", "anilib": ""},
		},
		Playback: &fakePlayback{},
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	s.beginStreamResolve("")
	// The settle must carry its round's generation — a stale round is
	// dropped by the leak guard (PR61 review R1c).
	s.Update(streamResolvedMsg{gen: s.resolveGen, entries: []streamEntry{
		{Quality: "1080", DubKey: "[animego] Дубль 1", Source: contracts.VideoSource{URL: "v1080"}},
		{Quality: "1080", DubKey: "[anilib] AniLib", Source: contracts.VideoSource{URL: "a1080"}},
	}})
	if s.state != sessionStateQuality {
		t.Fatalf("the settle must fill the merged list, got %v", s.state)
	}

	labels := map[string]string{}
	for _, c := range s.qualityList.Menu().Items {
		labels[c.ID] = c.Label
	}
	if got := labels["s0"]; got != "1080p · [RU] Дубль 1 [animego] · 2 сер." {
		t.Errorf("tagged dub label = %q", got)
	}
	if got := labels["s1"]; got != "1080p · AniLib [anilib] · 2 сер." {
		t.Errorf("plain dub label = %q", got)
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

func (f *fakeDownload) Download(_ context.Context, task DownloadTask) (string, error) {
	f.downloads = append(f.downloads, task)
	return "/dl/" + task.AnimeTitle + "/EP_" + task.EpisodeNum + ".mp4", nil
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
	next, _ := ss.Update(down()) // cursor → Оценка (Статус is index 0)
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
	next, _ := ss.Update(down())  // → Оценка
	next, _ = next.Update(down()) // → Пересмотры
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
	next, _ := ss.Update(enter())
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
	next, _ := ss.Update(enter())
	sp := next.(*sessionScreen)
	if sp.state != sessionStateInfoStatus {
		t.Fatalf("must open the status picker, got %v", sp.state)
	}
	// The cursor starts on «Смотрю»; Enter resolves it (not Back).
	_, cmd := sp.Update(enter())
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
// and the settle reaches the status line. The dub is resolved per
// episode before each download (PR64 #3).
func TestSessionDownloadForegroundDispatch(t *testing.T) {
	dl := &fakeDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl
	s.deps.Episode = &fakeEpisode{
		episodes: testEpisodeSet(),
		streams: map[string]contracts.MediaStream{
			"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v"}}},
		},
	}
	s.videoDub = "[animego] Дубль 1"

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
	// The cursor starts on «Передний план» (first item), Enter
	// dispatches.
	next, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("foreground pick must dispatch the download")
	}
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	if settled.err != nil {
		t.Fatalf("fake download must succeed, got %v", settled.err)
	}
	if settled.count != 2 || settled.total != 2 {
		t.Fatalf("both episodes must download, got %d/%d", settled.count, settled.total)
	}
	if len(dl.downloads) != 2 {
		t.Fatalf("foreground must download both episodes, got %d", len(dl.downloads))
	}
	for _, task := range dl.downloads {
		if task.DubID != "[animego] Дубль 1" {
			t.Fatalf("ep %s must carry its resolved dub, got %q", task.EpisodeNum, task.DubID)
		}
	}
	next, _ = next.Update(settled)
	if !contains(next.(*sessionScreen).status, "Загружено") {
		t.Fatalf("settle must update the status line, got %q", next.(*sessionScreen).status)
	}
}

// TestSessionDownloadBackgroundSubmits (C2): the background mode
// resolves the dubs per episode, then submits the tasks to the
// manager-backed service (PR64 #3 — the settle types the verdicts).
func TestSessionDownloadBackgroundSubmits(t *testing.T) {
	dl := &fakeDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl
	s.deps.Episode = &fakeEpisode{
		episodes: testEpisodeSet(),
		streams: map[string]contracts.MediaStream{
			"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v"}}},
		},
	}
	s.videoDub = "[animego] Дубль 1"

	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(down()) // «Фон»
	next, cmd := next.Update(enter())
	if cmd == nil {
		t.Fatalf("the background pick must dispatch the resolution")
	}
	queued, ok := cmd().(backgroundQueuedMsg)
	if !ok {
		t.Fatalf("background must settle into backgroundQueuedMsg, got %T", cmd())
	}
	next, _ = next.Update(queued)
	if len(dl.submitted) != 1 {
		t.Fatalf("background pick must submit one task, got %d", len(dl.submitted))
	}
	if !contains(next.(*sessionScreen).status, "фон") {
		t.Fatalf("background status expected, got %q", next.(*sessionScreen).status)
	}
}

// TestSessionDownloadSettledFailure (I8): a failed foreground download
// surfaces its error on the status line. The dub resolution must
// succeed first so the failure is genuinely the download's (PR64 #3).
func TestSessionDownloadSettledFailure(t *testing.T) {
	dl := &errDownload{}
	s := newSessionForTests(t)
	s.deps.Download = dl
	s.deps.Episode = &fakeEpisode{
		episodes: testEpisodeSet(),
		streams: map[string]contracts.MediaStream{
			"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v"}}},
		},
	}
	s.videoDub = "[animego] Дубль 1"
	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, cmd := ss.Update(enter())
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
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

func (f *errDownload) Download(_ context.Context, _ DownloadTask) (string, error) {
	return "", errors.New("disk full")
}

// watchStreaming drives «▶ Смотреть» through the PR44 format selector
// picking «Потоковый» and settles the fresh-session merged resolve
// (PR61), landing on the filled stream list.
func watchStreaming(t *testing.T, s *sessionScreen) *sessionScreen {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateFormat {
		t.Fatalf("watch must open the format selector, got %v", ss.state)
	}
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	next, cmd := ss.Update(enter())
	ss = next.(*sessionScreen)
	if cmd == nil {
		t.Fatalf("a fresh watch must schedule the stream resolve")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	next, _ = ss.Update(sr)
	return next.(*sessionScreen)
}

// watchToQuality drives the fresh watch through the merged stream
// list (PR61): settles the resolve and picks the entry at idx,
// landing on the audio prompt.
func watchToQuality(t *testing.T, s *sessionScreen, idx int) *sessionScreen {
	t.Helper()
	ss := watchStreaming(t, s)
	if ss.state != sessionStateQuality {
		t.Fatalf("the merged stream list expected, got %v", ss.state)
	}
	ss.qualityList.Jump(idx)
	next, _ := ss.Update(enter())
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

	// First watch: pick 720 explicitly (merged rows: 1080, 720).
	ss := watchToQuality(t, s, 0)
	if ss.state != sessionStateDubAudio {
		t.Fatalf("audio prompt expected, got %v", ss.state)
	}
	ss.dubList.Jump(0) // ⭐ Как видео
	_, cmd := ss.Update(enter())
	msg := cmd().(playedMsg)
	if msg.err != nil {
		t.Fatalf("play must succeed, got %v", msg.err)
	}
	if msg.quality != "1080" {
		t.Fatalf("playedMsg must carry the used quality, got %q", msg.quality)
	}
	next, _ := ss.Update(msg)
	ss = next.(*sessionScreen)
	if ss.lastQuality != "1080" {
		t.Fatalf("quality must be remembered on the model, got %q", ss.lastQuality)
	}

	// Second watch: the format selector opens again (dubs remembered,
	// so the scoped resolve settles straight into playback with the
	// remembered quality — no pick prompts, PR61).
	ss.list.Jump(sessionActionIndex(ss, "watch"))
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateFormat {
		t.Fatalf("watch must open the format selector, got %v", ss.state)
	}
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	next, cmd = ss.Update(enter())
	srMsg := cmd()
	if _, ok := srMsg.(streamResolvedMsg); !ok {
		t.Fatalf("stream resolve expected, got %T", srMsg)
	}
	next, play := next.Update(srMsg)
	if play == nil {
		t.Fatalf("the scoped settle must launch playback")
	}
	if _, ok := play().(playedMsg); !ok {
		t.Fatalf("second play must settle, got %T", play())
	}
	if len(pb.played) != 2 {
		t.Fatalf("two playbacks expected, got %d", len(pb.played))
	}
	if pb.played[1].URL != "v1080" {
		t.Fatalf("the remembered quality must be reused, got %q", pb.played[1].URL)
	}
	_ = next
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
		// The info menu cursor starts on «Статус» (PR24 layout).
		next, _ := ss.Update(enter())
		sp := next.(*sessionScreen)
		// The status picker cursor starts on «Смотрю».
		screen, cmd := sp.Update(enter())
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

	// Watch straight to dispatch: dubs are already set, so the
	// format selector's streaming pick resolves the scoped dub and
	// settles straight into playback (PR61, no pick prompts).
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss0 := next.(*sessionScreen)
	if ss0.state != sessionStateFormat {
		t.Fatalf("watch must open the format selector, got %v", ss0.state)
	}
	ss0.formatList.Jump(indexOfDayFormatList(ss0, "stream"))
	next, cmd := ss0.Update(enter())
	msg := cmd()
	if _, ok := msg.(streamResolvedMsg); !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	_, play := next.Update(msg)
	if play == nil {
		t.Fatalf("the scoped settle must launch playback")
	}
	if _, ok := play().(playedMsg); !ok {
		t.Fatalf("play must settle, got %T", play())
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

// TestSessionSkipNoteComposedAtLaunch (PR61): the skip verdict fetched
// during the stream resolve rides the launch line — «▶ Запуск mpv… ·
// ⏭ …» — and auto-clears when playback settles.
func TestSessionSkipNoteComposedAtLaunch(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: &fakePlayback{skipNote: "скипы: op 0:00–1:30"},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	// Watch → merged list settle carries the skip note.
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	if sr.skipNote != "скипы: op 0:00–1:30" {
		t.Fatalf("the resolve settle must carry the skip note, got %q", sr.skipNote)
	}
	next, _ = ss.Update(sr)
	ss = next.(*sessionScreen)

	// Pick the entry; ⭐ audio; the launch line composes the note.
	ss.qualityList.Jump(0)
	ss.Update(enter())
	ss.dubList.Jump(0)
	ss.Update(enter())
	if !strings.Contains(ss.status, "▶ Запуск mpv…") ||
		!strings.Contains(ss.status, "⏭ скипы: op 0:00–1:30") {
		t.Fatalf("launch line must compose the skip note, got %q", ss.status)
	}
	// Playback settling auto-clears the note (the completion verdict
	// replaces it).
	ss.Update(playedMsg{})
	if strings.Contains(ss.status, "⏭") {
		t.Fatalf("the skip note must auto-clear on settle, got %q", ss.status)
	}
}

// TestSessionSkipNoteAbsentKeepsPlainLaunch (PR61): without a note the
// launch line stays the plain «▶ Запуск mpv…».
func TestSessionSkipNoteAbsentKeepsPlainLaunch(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: &fakePlayback{},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	ss.Update(cmd())
	ss.qualityList.Jump(0)
	ss.Update(enter())
	ss.dubList.Jump(0)
	ss.Update(enter())
	if ss.status != "▶ Запуск mpv…" {
		t.Fatalf("launch line = %q, want the plain form", ss.status)
	}
}

// shikiWatchSession builds a bound, watch-ready session: streams for
// the animego dub, shikimori binding id 21, the given shiki/history
// fakes and a captured log buffer.
func shikiWatchSession(t *testing.T, shiki *fakeShiki, hist *fakeHistory, logs *bytes.Buffer) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: &fakePlayback{},
		Shiki:    shiki,
		History:  hist,
		Log:      slog.New(slog.NewTextHandler(logs, nil)),
	}
	group := []contracts.SearchResult{{
		Title: "Тайтл", URL: "u1", SourceID: "animego",
		Meta: map[string]any{"shikimori_id": int64(21)},
	}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s
}

// shikiPlayToLaunch drives the fresh watch through the merged list and
// the ⭐ audio pick, returning the launch commands' messages.
func shikiPlayToLaunch(t *testing.T, s *sessionScreen) []tea.Msg {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	next, _ = ss.Update(sr)
	ss = next.(*sessionScreen)
	ss.qualityList.Jump(0)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss.dubList.Jump(0)
	_, launch := ss.Update(enter())
	if launch == nil {
		t.Fatalf("the audio pick must launch playback")
	}
	msg := launch()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	out := []tea.Msg{}
	for _, c := range batch {
		out = append(out, c())
	}
	return out
}

// TestSessionWatchSyncsShikiProgress (PR61): the launch pushes
// {episodes: N, status: watching} — the CREATE path when no rate id is
// known — and the verdict lands on the status line.
func TestSessionWatchSyncsShikiProgress(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	logs := &bytes.Buffer{}
	s := shikiWatchSession(t, shiki, &fakeHistory{}, logs)

	msgs := shikiPlayToLaunch(t, s)
	synced := false
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok {
			synced = true
			if sm.err != nil {
				t.Fatalf("sync must succeed, got %v", sm.err)
			}
			if !strings.Contains(sm.note, "Shikimori: прогресс синхронизирован (эп 1)") {
				t.Fatalf("note = %q, want the synced verdict with the full product name", sm.note)
			}
		}
	}
	if !synced {
		t.Fatalf("the launch batch must carry shikiSyncedMsg, got %v", msgs)
	}
	if len(shiki.epPushes) != 1 {
		t.Fatalf("exactly one push expected, got %+v", shiki.epPushes)
	}
	push := shiki.epPushes[0]
	if push.shikimoriID != 21 || push.rateID != 0 || push.episodes != 1 || push.status != "watching" {
		t.Fatalf("push = %+v, want {21 0 1 watching}", push)
	}
	if !strings.Contains(logs.String(), "shiki: progress synced") {
		t.Fatalf("success must reach the file logger, got:\n%s", logs.String())
	}
}

// TestSessionWatchSyncCreatePersistsRateID (PR61): the created rate id
// is persisted on the history row so the next push PATCHes it.
func TestSessionWatchSyncCreatePersistsRateID(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	hist := &fakeHistory{byShiki: map[int64]*storage.AnimeProgress{
		21: {ID: 7},
	}}
	s := shikiWatchSession(t, shiki, hist, &bytes.Buffer{})

	shikiPlayToLaunch(t, s)
	if shiki.epPushes[0].rateID != 0 {
		t.Fatalf("first push must CREATE (rate 0), got %+v", shiki.epPushes[0])
	}
	if hist.rateIDs[7] == 0 {
		t.Fatalf("the created rate id must be persisted, got %v", hist.rateIDs)
	}
}

// TestSessionWatchSyncPatchKnownRate (PR61): a known rate id rides the
// push — the PATCH path (idempotent SET, no duplicate rates).
func TestSessionWatchSyncPatchKnownRate(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	rate := int64(55)
	hist := &fakeHistory{byShiki: map[int64]*storage.AnimeProgress{
		21: {ID: 7, CurrentEpisode: "1", ShikimoriRateID: &rate},
	}}
	s := shikiWatchSession(t, shiki, hist, &bytes.Buffer{})

	shikiPlayToLaunch(t, s)
	if len(shiki.epPushes) != 1 || shiki.epPushes[0].rateID != 55 {
		t.Fatalf("pushes = %+v, want one PATCH with rate 55", shiki.epPushes)
	}
}

// TestSessionWatchSyncSkipsUnauthenticated (PR61): an authenticated-
// lacking integration is a typed skip — one status line, no push, a
// file-logger note.
func TestSessionWatchSyncSkipsUnauthenticated(t *testing.T) {
	shiki := &fakeShiki{enabled: true, mode: "none"}
	logs := &bytes.Buffer{}
	s := shikiWatchSession(t, shiki, &fakeHistory{}, logs)

	msgs := shikiPlayToLaunch(t, s)
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok {
			if !strings.Contains(sm.note, "нет авторизации") {
				t.Fatalf("note = %q, want the typed unauth skip", sm.note)
			}
		}
	}
	if len(shiki.epPushes) != 0 {
		t.Fatalf("no push may run unauthenticated, got %+v", shiki.epPushes)
	}
	if !strings.Contains(logs.String(), "shiki: no auth") {
		t.Fatalf("the skip must reach the file logger, got:\n%s", logs.String())
	}
}

// TestSessionWatchSyncDisabledTyped (PR61): a disabled tracker is a
// typed skip with a status note and a log entry — never silent.
func TestSessionWatchSyncDisabledTyped(t *testing.T) {
	shiki := &fakeShiki{enabled: false}
	logs := &bytes.Buffer{}
	s := shikiWatchSession(t, shiki, &fakeHistory{}, logs)

	msgs := shikiPlayToLaunch(t, s)
	found := false
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok {
			found = true
			if !strings.Contains(sm.note, "трекер отключён") {
				t.Fatalf("note = %q, want the disabled skip", sm.note)
			}
		}
	}
	if !found {
		t.Fatalf("the disabled skip must be typed, got %v", msgs)
	}
	if len(shiki.epPushes) != 0 {
		t.Fatalf("no push may run disabled, got %+v", shiki.epPushes)
	}
	if !strings.Contains(logs.String(), "tracker disabled") {
		t.Fatalf("the skip must reach the file logger, got:\n%s", logs.String())
	}
}

// TestSessionWatchSyncNeverRollsBack (PR61): the counter only moves
// forward — an episode behind the local progress is a typed skip, no
// push (idempotence + monotonicity).
func TestSessionWatchSyncNeverRollsBack(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	hist := &fakeHistory{byShiki: map[int64]*storage.AnimeProgress{
		21: {ID: 7, CurrentEpisode: "5"},
	}}
	s := shikiWatchSession(t, shiki, hist, &bytes.Buffer{})

	msgs := shikiPlayToLaunch(t, s)
	sawNote := false
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok {
			if !strings.Contains(sm.note, "не откатывается") {
				t.Fatalf("note = %q, want the rollback guard", sm.note)
			}
			sawNote = true
		}
	}
	if !sawNote {
		t.Fatalf("the rollback guard must be typed, got %v", msgs)
	}
	if len(shiki.epPushes) != 0 {
		t.Fatalf("no push may run for an older episode, got %+v", shiki.epPushes)
	}
}

// TestSessionWatchSyncReplaySameEpisode (PR61): re-playing the SAME
// episode pushes the same SET again (episodes=N is idempotent — no
// double increment is possible).
func TestSessionWatchSyncReplaySameEpisode(t *testing.T) {
	shiki := &fakeShiki{enabled: true}
	hist := &fakeHistory{byShiki: map[int64]*storage.AnimeProgress{
		21: {ID: 7, CurrentEpisode: "1"},
	}}
	s := shikiWatchSession(t, shiki, hist, &bytes.Buffer{})

	shikiPlayToLaunch(t, s)
	if len(shiki.epPushes) != 1 || shiki.epPushes[0].episodes != 1 {
		t.Fatalf("replay pushes = %+v, want one episodes=1 SET", shiki.epPushes)
	}
}

// TestSessionWatchSyncFailureLogged (PR61): a failed push surfaces on
// the status line and in the file logger.
func TestSessionWatchSyncFailureLogged(t *testing.T) {
	shiki := &fakeShiki{enabled: true, epErr: errors.New("shiki down")}
	logs := &bytes.Buffer{}
	s := shikiWatchSession(t, shiki, &fakeHistory{}, logs)

	msgs := shikiPlayToLaunch(t, s)
	sawErr := false
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok && sm.err != nil {
			sawErr = true
			if !strings.Contains(sm.err.Error(), "ошибка синхронизации") {
				t.Fatalf("err = %v, want the sync failure prefix", sm.err)
			}
		}
	}
	if !sawErr {
		t.Fatalf("the failure must be carried, got %v", msgs)
	}
	if !strings.Contains(logs.String(), "shiki: progress push failed") {
		t.Fatalf("the failure must reach the file logger, got:\n%s", logs.String())
	}
}

// leakProbePlayback extends fakePlayback with REAL temp chapters
// files: every ResolveSkips call creates one in dir and its cleanup
// removes it — the leak probe counts survivors.
type leakProbePlayback struct {
	fakePlayback
	dir string
}

func (f *leakProbePlayback) ResolveSkips(_ context.Context, shikimoriID int64, _ float64) (string, func(), string, error) {
	f.skipIDs = append(f.skipIDs, shikimoriID)
	fh, err := os.CreateTemp(f.dir, "anicli-leak-probe-*")
	if err != nil {
		return "", func() {}, "", err
	}
	path := fh.Name()
	_ = fh.Close()
	return path, func() { _ = os.Remove(path) }, "скипы: op 0:00–1:00", nil
}

func leakProbeSession(t *testing.T) (*sessionScreen, *leakProbePlayback, string) {
	t.Helper()
	pb := &leakProbePlayback{dir: t.TempDir()}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: pb,
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s, pb, pb.dir
}

func leakProbeLeftovers(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read probe dir: %v", err)
	}
	return len(entries)
}

// TestSessionRedubRewatchCleansChaptersFile (PR61 review R1b): a
// same-episode re-resolve («Сменить озвучку» → re-watch) must retire
// the previous chapters file — no unique temp file may survive.
func TestSessionRedubRewatchCleansChaptersFile(t *testing.T) {
	s, _, dir := leakProbeSession(t)

	// First watch settles: file A is pending.
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	ss.Update(sr)
	if leakProbeLeftovers(t, dir) != 1 {
		t.Fatalf("one pending chapters file expected, got %d", leakProbeLeftovers(t, dir))
	}

	// Back to the menu, «Сменить озвучку», re-watch same episode: the
	// second resolve supersedes the first — file A must go, file B
	// pending.
	ss.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if ss.state != sessionStateMenu {
		t.Fatalf("Esc must return to the menu, got %v", ss.state)
	}
	ss.list.Jump(sessionActionIndex(ss, "redub"))
	ss.Update(enter())
	ss.list.Jump(sessionActionIndex(ss, "watch"))
	ss.Update(enter())
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd = ss.Update(enter())
	sr2 := cmd().(streamResolvedMsg)
	ss.Update(sr2)
	if n := leakProbeLeftovers(t, dir); n != 1 {
		t.Fatalf("supersede must keep exactly the newest chapters file, got %d", n)
	}

	// Backing out of the session retires the last pending file.
	ss.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if n := leakProbeLeftovers(t, dir); n != 0 {
		t.Fatalf("cancel must remove the pending chapters file, got %d survivors", n)
	}
}

// TestSessionResolveErrorCleansChaptersFile (PR61 review R1a): the
// error branch of the resolve settle must drop that round's chapters
// file — the skip fetch runs even when the streams fail.
func TestSessionResolveErrorCleansChaptersFile(t *testing.T) {
	pb := &leakProbePlayback{dir: t.TempDir()}
	deps := &Deps{
		Episode: &fakeEpisode{
			// Episodes list fine, but ResolveStream has no fixture —
			// every provider resolve fails ("no stream") while the
			// skip fetch still runs.
			episodes: testEpisodeSet(),
		},
		Playback: pb,
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	msg := cmd()
	sr, ok := msg.(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	if sr.err == nil {
		t.Fatalf("the resolve must fail with the provider down")
	}
	ss.Update(sr)
	if n := leakProbeLeftovers(t, pb.dir); n != 0 {
		t.Fatalf("the error settle must remove its chapters file, got %d survivors", n)
	}
}

// TestSessionCancelMidResolveCleansChaptersFile (PR61 review R1c):
// cancelling while a resolve is in flight invalidates that round —
// its late settle must drop its own chapters file instead of leaking
// it into the pending slot.
func TestSessionCancelMidResolveCleansChaptersFile(t *testing.T) {
	s, _, dir := leakProbeSession(t)

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("the resolve must be scheduled")
	}
	// Cancel while the resolve goroutine is in flight.
	ss.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if ss.state != sessionStateMenu {
		t.Fatalf("cancel must return to the menu, got %v", ss.state)
	}
	// The late settle lands: a stale round cleans up after itself.
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	ss.Update(sr)
	if n := leakProbeLeftovers(t, dir); n != 0 {
		t.Fatalf("the stale settle must remove its chapters file, got %d survivors", n)
	}
}
