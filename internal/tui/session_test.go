package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
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
	skipPath string
	err      error
}

func (f *fakePlayback) ResolveSkips(_ context.Context, _ int64, _ float64) (string, func(), error) {
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
