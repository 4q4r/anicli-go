package tui

// PR165b T11 tests: the audio_sync ask prompt on download batches —
// the config gate (ask/yes/no), the two-choice menu in the #164
// dub-menu form, the mixing-only qualifier (PR174: the marked
// download audio set — a plain single-dub batch never asks), and the
// strict-restore through the two-menu chain.

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

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
	next, cmd := ss.Update(enter()) // foreground, the default first row
	if cmd != nil {
		// A directly-dispatched batch (config yes/no, single-provider)
		// runs here; a menu opening leaves cmd nil.
		runLaunchBatch(t, cmd)
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

	// «Да»: the first choice — the batch starts, every task carries
	// AudioSync.
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatal("«Да» must dispatch the batch")
	}
	runLaunchBatch(t, cmd)
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
	_, cmd2 := ss2b.Update(enter())
	if cmd2 == nil {
		t.Fatal("«Нет» must dispatch the batch")
	}
	runLaunchBatch(t, cmd2)
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
	ss := pr165bDriveToStart(t, s)
	_, cmd := ss.Update(esc())
	runLaunchBatch(t, cmd)
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

// TestSyncAskStrictRestoredThroughTheDubMenu: the #164 dub-menu pick
// re-enters startRangeDownload with the strict mode; the sync ask
// stashes it and the answer restores the STRICT batch. The ask
// qualifies through the marked audio set (PR174: mixing-only).
func TestSyncAskStrictRestoredThroughTheDubMenu(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr165bAskSession(t, "ask")
	// NO remembered dub: the #164 dub menu opens first. The marked
	// audio set qualifies the batch as mixing — the ask follows the
	// pick.
	s.markedAudio = []string{"[anilib] AniLib"}
	ss := pr165bDriveToStart(t, s)
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("state = %v, want the #164 dub menu", ss.state)
	}
	// Pick the dub (Jump(1): sortedEmbedKeys lists AniLib first): the
	// batch re-enters startRangeDownload strict, the sync ask opens
	// on top (stashing mode+strict).
	ss.redubList.Jump(1)
	next, _ := ss.Update(enter())
	ss2 := next.(*sessionScreen)
	if ss2.state != sessionStateSyncAsk {
		t.Fatalf("state = %v, want the sync ask after the dub pick", ss2.state)
	}
	// «Да»: the batch runs STRICT (the picked dub alone) and synced.
	_, cmd2 := ss2.Update(enter())
	if cmd2 == nil {
		t.Fatal("the strict batch must dispatch")
	}
	runLaunchBatch(t, cmd2)
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
