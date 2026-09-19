package tui

// PR64 owner-reported defects: transient statuses must belong to the
// surface that set them (no leak across screens), the download-range
// prompt must describe the available episodes, and range downloads
// must resolve the dub per episode (PR63 watch-flow semantics).
//
// The tests here cover defect #1: a status set by one surface (the
// playback verdict of the owner's screenshot) renders on THAT surface
// only — entering any other surface starts with a clean status line.
// The file logger keeps the full verdict history instead.

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// pr64Session builds a loaded session whose stream resolves succeed,
// so the sweep can drive the watch pipeline onto the picker surfaces.
func pr64Session(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				"[anilib] AniLib":   {Links: map[string]contracts.VideoSource{"1080": {URL: "v-lib"}}},
			},
		},
		Download: &fakeDownload{},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s
}

// seedPlaybackVerdict drives the REAL playback settle — the exact path
// that produced the owner's screenshot («Воспроизведение завершено»
// rendered under the download prompt).
func seedPlaybackVerdict(s *sessionScreen) *sessionScreen {
	next, _ := s.Update(playedMsg{})
	return next.(*sessionScreen)
}

// TestDownloadDialogHidesStalePlaybackStatus (PR64 #1): after a
// finished playback, the download-range prompt must render WITHOUT the
// stale «Воспроизведение завершено» line.
func TestDownloadDialogHidesStalePlaybackStatus(t *testing.T) {
	s := pr64Session(t)
	s.deps.Download = &fakeDownload{}
	s = seedPlaybackVerdict(s)

	idx := sessionActionIndex(s, "download")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	v := ss.View().Content
	if !contains(v, "для загрузки") {
		t.Fatalf("the range prompt must render:\n%s", v)
	}
	if contains(v, "Воспроизведение завершено") {
		t.Fatalf("the playback verdict must not leak into the download dialog:\n%s", v)
	}
}

// TestStatusNeverSurvivesSessionTransition (PR64 #1 sweep): the
// playback verdict renders on the menu surface ONLY — every other
// substate the user can enter next starts clean.
func TestStatusNeverSurvivesSessionTransition(t *testing.T) {
	const stale = "Воспроизведение завершено"
	transitions := map[string]func(t *testing.T, s *sessionScreen) *sessionScreen{
		"download-range": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			s.list.Jump(sessionActionIndex(s, "download"))
			next, _ := s.Update(enter())
			return next.(*sessionScreen)
		},
		"download-mode": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			s.list.Jump(sessionActionIndex(s, "download"))
			next, _ := s.Update(enter())
			ss := next.(*sessionScreen)
			ss.rangeInput.typeText("1")
			next, _ = ss.Update(enter())
			return next.(*sessionScreen)
		},
		"info-menu": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			return openInfo(s)
		},
		"info-status": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			ss := openInfo(s)
			next, _ := ss.Update(enter()) // cursor on «Статус»
			return next.(*sessionScreen)
		},
		"episode-list": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			s.list.Jump(sessionActionIndex(s, "jump"))
			next, _ := s.Update(enter())
			return next.(*sessionScreen)
		},
		"format": func(_ *testing.T, s *sessionScreen) *sessionScreen {
			s.list.Jump(sessionActionIndex(s, "watch"))
			next, _ := s.Update(enter())
			return next.(*sessionScreen)
		},
		"quality": watchStreaming,
	}
	for name, drive := range transitions {
		t.Run(name, func(t *testing.T) {
			s := seedPlaybackVerdict(pr64Session(t))
			// Positive control: on the menu surface the verdict IS
			// visible — the sweep proves scoping, not suppression.
			if !contains(s.View().Content, stale) {
				t.Fatalf("precondition: the menu surface must show the verdict:\n%s", s.View().Content)
			}
			ss := drive(t, s)
			if contains(ss.View().Content, stale) {
				t.Fatalf("stale playback status leaked into %q:\n%s", name, ss.View().Content)
			}
		})
	}
}

// TestOfflineStatusNeverSurvivesSurfaceSwitch (PR64 #1 audit): the
// offline session carries the same transient-status model — its play
// verdict must not leak onto the episode picker or the variant picker.
func TestOfflineStatusNeverSurvivesSurfaceSwitch(t *testing.T) {
	const stale = "Воспроизведение завершено"
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}, Playback: &fakePlayback{}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])

	s.episodeList.Jump(0)
	next, _ := s.Update(enter())
	ss := next.(*offlineSession)

	ss.list.Jump(offlineActionIndex(ss, "watch"))
	_, cmd := ss.Update(enter())
	pm, ok := cmd().(offlinePlayMsg)
	if !ok {
		t.Fatalf("offline watch must emit offlinePlayMsg, got %T", cmd())
	}
	_, cmd = ss.Update(pm)
	settled, ok := cmd().(offlinePlayedMsg)
	if !ok {
		t.Fatalf("play must settle into offlinePlayedMsg, got %T", cmd())
	}
	next, _ = ss.Update(settled)
	ss = next.(*offlineSession)
	if !contains(ss.View().Content, stale) {
		t.Fatalf("precondition: the menu surface must show the verdict:\n%s", ss.View().Content)
	}

	// Onto the episode picker surface.
	ss.list.Jump(offlineActionIndex(ss, "jump"))
	next, _ = ss.Update(enter())
	ss = next.(*offlineSession)
	if contains(ss.View().Content, stale) {
		t.Fatalf("stale playback status leaked into the episode picker:\n%s", ss.View().Content)
	}

	// Back to the menu, onto the variant picker surface.
	ss.episodeList.Jump(0)
	next, _ = ss.Update(enter())
	ss = next.(*offlineSession)
	ss.list.Jump(offlineActionIndex(ss, "variant"))
	next, _ = ss.Update(enter())
	ss = next.(*offlineSession)
	if contains(ss.View().Content, stale) {
		t.Fatalf("stale playback status leaked into the variant picker:\n%s", ss.View().Content)
	}
}

// --- PR64 #2: the download-range prompt must describe what EXISTS ---

// TestDescribeAvailableEpisodes: the compact availability line —
// count plus the real set, consecutive episodes collapsed into runs,
// gaps and non-numeric labels listed as-is.
func TestDescribeAvailableEpisodes(t *testing.T) {
	cases := []struct {
		name  string
		order []string
		want  string
	}{
		{"contiguous", []string{"1", "2", "3"}, "Доступно серий: 3 (1–3)"},
		{"gaps", []string{"1", "2", "3", "7", "10"}, "Доступно серий: 5 (1–3, 7, 10)"},
		{"single", []string{"5"}, "Доступно серий: 1 (5)"},
		{"junk-label", []string{"1", "OVA"}, "Доступно серий: 2 (1, OVA)"},
		{"empty", nil, "Доступных серий нет"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeAvailableEpisodes(tc.order); got != tc.want {
				t.Fatalf("describeAvailableEpisodes(%v) = %q, want %q", tc.order, got, tc.want)
			}
		})
	}
}

// TestDownloadPromptShowsAvailableEpisodes (PR64 #2): the range prompt
// carries the availability line so the user never guesses what exists.
func TestDownloadPromptShowsAvailableEpisodes(t *testing.T) {
	s := pr64Session(t) // merged episodes 1, 2, 3

	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	v := ss.View().Content
	if !contains(v, "Доступно серий: 3 (1–3)") {
		t.Fatalf("the prompt must show the available episodes:\n%s", v)
	}
}
