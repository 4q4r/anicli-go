package tui

// PR165b T11 tests: the audio_sync ask prompt on download batches —
// the config gate (ask/yes/no), the two-choice menu in the #164
// dub-menu form, the mixing-only qualifier (PR174: the marked
// download audio set — a plain single-dub batch never asks), and the
// strict-restore through the two-menu chain.

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"

	tea "charm.land/bubbletea/v2"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// pr165bAnswerDrive answers the sync ask with the given key (Enter
// on the cursor row, or Esc): both answers PUSH the direct download
// screen — only AudioSync differs — and the helper drives the pushed
// batch to its settle.
func pr165bAnswerDrive(t *testing.T, ss *sessionScreen, key tea.KeyPressMsg) (*sessionScreen, *fakeDownload) {
	t.Helper()
	_, cmd := ss.Update(key)
	if cmd == nil {
		t.Fatal("the sync answer must push the download screen")
	}
	pushedMsg := cmd()
	pushed, ok := pushedMsg.(pushMsg)
	if !ok {
		t.Fatalf("the sync answer must push the screen, got %T", pushedMsg)
	}
	ds, ok := pushed.screen.(*directDownloadScreen)
	if !ok {
		t.Fatalf("the pushed screen must be the direct screen, got %T", pushed.screen)
	}
	driveDirectScreen(t, ds)
	return ss, ss.deps.Download.(*fakeDownload)
}

// pr165bAskSession builds a loaded two-provider session (the merge
// parts ≥2 — the ask gate qualifies) with the given config.
func pr165bAskSession(t *testing.T, downloadSync string) (*sessionScreen, *fakeDownload) {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{
					"Дубль 1": {"u1v"},
				}}},
				"anilib": {
					{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"w1b"}}},
					{Num: "2", RawID: "b2", RawEmbeds: map[string][]string{"AniLib": {"w2b"}}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				"[anilib] AniLib":   {Links: map[string]contracts.VideoSource{"1080": {URL: "v-lib"}}},
			},
		},
		Download:     &fakeDownload{},
		Log:          testLogger(),
		DownloadSync: downloadSync,
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s, deps.Download.(*fakeDownload)
}

// pr165bDriveToStart drives the REAL path to the batch start: «Скачать
// серии» → range 1-2 → content видео+аудио → mode foreground. With a
// remembered dub the #164 dub menu stays out of the way; the returned
// screen is the session in whatever state the config produced.
func pr165bDriveToStart(t *testing.T, s *sessionScreen) *sessionScreen {
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadContent {
		t.Fatalf("after the range the content-mode menu opens, got %v", ss.state)
	}
	next, _ = ss.Update(enter()) // видео+аудио, the default first row
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the content pick must open the mode menu, got %v", ss.state)
	}
	next, cmd := ss.Update(enter()) // the direct download row
	if cmd != nil {
		// A directly-pushed batch (config yes/no) runs here; a menu
		// opening (config ask) leaves cmd nil.
		pushedMsg := cmd()
		if pushed, ok := pushedMsg.(pushMsg); ok {
			if ds, ok := pushed.screen.(*directDownloadScreen); ok {
				driveDirectScreen(t, ds)
			}
		}
	}
	return next.(*sessionScreen)
}

// TestSyncAskMenuOpensOnAskConfig: config "ask" + a MIXING batch
// (видео+аудио with a marked download audio set — external dub
// tracks beyond the video's own audio) → the two-choice menu opens;
// «Да» starts the batch with AudioSync on every task; «Нет» starts
// it with sync off (не отмена).
func TestSyncAskMenuOpensOnAskConfig(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr165bAskSession(t, "ask")
	s.videoDub = "[animego] Дубль 1"
	s.markedAudio = []string{"[anilib] AniLib"}
	ss := pr165bDriveToStart(t, s)
	if ss.state != sessionStateSyncAsk {
		t.Fatalf("state = %v, want the sync-ask menu", ss.state)
	}

	// «Да»: the first choice — the screen pushes, every task carries
	// AudioSync.
	_, dl = pr165bAnswerDrive(t, ss, enter())
	if len(dl.downloads) == 0 {
		t.Fatalf("«Да» must start the batch, got %d downloads", len(dl.downloads))
	}
	for _, task := range dl.downloads {
		if !task.AudioSync {
			t.Fatalf("task %s must carry AudioSync", task.EpisodeNum)
		}
	}

	// «Нет»: a fresh batch — the second choice, sync off, still
	// downloaded.
	s2, dl2 := pr165bAskSession(t, "ask")
	s2.videoDub = "[animego] Дубль 1"
	s2.markedAudio = []string{"[anilib] AniLib"}
	ss2b := pr165bDriveToStart(t, s2)
	ss2b.syncAskList.Jump(1)
	_, dl2 = pr165bAnswerDrive(t, ss2b, enter())
	if len(dl2.downloads) == 0 {
		t.Fatalf("«Нет» must still start the batch (не отмена), got %d", len(dl2.downloads))
	}
	for _, task := range dl2.downloads {
		if task.AudioSync {
			t.Fatalf("«Нет» task %s must not carry AudioSync", task.EpisodeNum)
		}
	}
}

// TestSyncAskEscMeansNo: Esc answers «Нет» — the batch continues
// unsynchronized (never an abort).
func TestSyncAskEscMeansNo(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr165bAskSession(t, "ask")
	s.videoDub = "[animego] Дубль 1"
	s.markedAudio = []string{"[anilib] AniLib"} // mixing — the ask qualifier
	ss := pr165bDriveToStart(t, s)
	_, dl = pr165bAnswerDrive(t, ss, esc())
	if len(dl.downloads) == 0 {
		t.Fatalf("Esc must continue the batch, got %d downloads", len(dl.downloads))
	}
	for _, task := range dl.downloads {
		if task.AudioSync {
			t.Fatalf("Esc task %s must not carry AudioSync", task.EpisodeNum)
		}
	}
}

// TestSyncAskConfigYesAndNoSkipTheMenu: "yes" starts a MIXING batch
// synced without any menu (a plain batch runs unsynchronized —
// nothing to analyze); "no" starts it unsynchronized without one.
func TestSyncAskConfigYesAndNoSkipTheMenu(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr165bAskSession(t, "yes")
	s.videoDub = "[animego] Дубль 1"
	s.markedAudio = []string{"[anilib] AniLib"}
	ss := pr165bDriveToStart(t, s)
	if ss.state == sessionStateSyncAsk {
		t.Fatal("config yes must not open the menu")
	}
	if len(dl.downloads) == 0 {
		t.Fatal("config yes must start the batch directly")
	}
	for _, task := range dl.downloads {
		if !task.AudioSync {
			t.Fatalf("yes task %s must carry AudioSync", task.EpisodeNum)
		}
	}

	s2, dl2 := pr165bAskSession(t, "no")
	s2.videoDub = "[animego] Дубль 1"
	s2.markedAudio = []string{"[anilib] AniLib"}
	ss2 := pr165bDriveToStart(t, s2)
	if ss2.state == sessionStateSyncAsk {
		t.Fatal("config no must not open the menu")
	}
	for _, task := range dl2.downloads {
		if task.AudioSync {
			t.Fatalf("no task %s must not carry AudioSync", task.EpisodeNum)
		}
	}
}

// TestSyncAskSingleProviderSkips: a plain batch (no marked audio set)
// never opens the menu — the single-provider session here is the
// plainest case of the PR174 mixing-only gate: no external tracks,
// nothing to sync, sync off without any prompt.
func TestSyncAskSingleProviderSkips(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{
					"Дубль 1": {"u1v"},
				}}},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
			},
		},
		Download: &fakeDownload{},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animego] Дубль 1"
	ss := pr165bDriveToStart(t, s)
	if ss.state == sessionStateSyncAsk {
		t.Fatal("a single-provider session must not open the sync menu")
	}
	dl := deps.Download.(*fakeDownload)
	for _, task := range dl.downloads {
		if task.AudioSync {
			t.Fatalf("single-provider task %s must not carry AudioSync", task.EpisodeNum)
		}
	}
}

// TestSyncAskStrictRestoredThroughTheDubMenu: the stream pick
// re-enters startRangeDownload with the strict mode; the sync ask
// stashes it and the answer restores the STRICT batch. The ask
// qualifies through the marked audio set (PR174: mixing-only).
func TestSyncAskStrictRestoredThroughTheDubMenu(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr165bAskSession(t, "ask")
	// NO remembered dub: the PR174 stream picker opens first. The
	// marked audio set qualifies the batch as mixing — the ask
	// follows the pick.
	s.markedAudio = []string{"[anilib] AniLib"}
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, cmd0 := ss.Update(enter()) // видео+аудио → the stream picker
	ss = next.(*sessionScreen)
	if cmd0 == nil {
		t.Fatal("the content pick must start the stream resolve")
	}
	next, _ = ss.Update(cmd0())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadStream {
		t.Fatalf("state = %v, want the stream picker", ss.state)
	}
	// Pick the animego stream (Jump to its row): the batch re-enters
	// startRangeDownload strict, the sync ask opens on top (stashing
	// mode+strict).
	picked := false
	for i, e := range ss.streamEntries {
		if e.DubKey == "[animego] Дубль 1" {
			ss.qualityList.Jump(i)
			picked = true
			break
		}
	}
	if !picked {
		t.Fatalf("no animego stream row: %+v", ss.streamEntries)
	}
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("state = %v, want the mode menu after the stream pick", ss.state)
	}
	// The mode pick re-enters startRangeDownload strict; the sync ask
	// opens on top (stashing mode+strict).
	next, _ = ss.Update(enter()) // foreground, the first row
	ss2 := next.(*sessionScreen)
	if ss2.state != sessionStateSyncAsk {
		t.Fatalf("state = %v, want the sync ask after the mode pick", ss2.state)
	}
	// «Да»: the batch runs STRICT (the picked dub alone) and synced.
	_, dl = pr165bAnswerDrive(t, ss2, enter())
	if len(dl.downloads) == 0 {
		t.Fatal("the strict batch must start")
	}
	for _, task := range dl.downloads {
		if task.DubID != "[animego] Дубль 1" {
			t.Fatalf("strict batch must carry the picked dub alone, got %q", task.DubID)
		}
		if !task.AudioSync {
			t.Fatalf("strict task %s must carry AudioSync", task.EpisodeNum)
		}
	}
}
