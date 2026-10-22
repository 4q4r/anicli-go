package tui

// PR174 S3: the audio_sync ask prompt fires ONLY on MIXING downloads —
// a video+audio batch carrying a marked audio set (video + external
// dub tracks). A plain single-dub download has nothing to sync: no
// prompt, whatever the session's provider topology. The pre-PR174
// gate qualified on `len(parts) >= 2` — the SESSION shape — so every
// merged session asked on every plain batch, and answering «Да» ran
// no analysis at all (no donors ride a plain task).

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// pr174SyncSession builds the two-provider merged session of the
// PR165b ask tests (the topology the old gate keyed on) with the
// given config and optional marked download audio set.
func pr174SyncSession(t *testing.T, downloadSync string, marked []string) (*sessionScreen, *fakeDownload) {
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
	s.videoDub = "[animego] Дубль 1"
	s.markedAudio = marked
	return s, deps.Download.(*fakeDownload)
}

// pr174DriveToBatch walks range 1-2 → видео+аудио → the first mode
// row, running any directly-dispatched batch command.
func pr174DriveToBatch(t *testing.T, s *sessionScreen) (*sessionScreen, *fakeDownload) {
	t.Helper()
	dl := s.deps.Download.(*fakeDownload)
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(enter()) // видео+аудио, the default first row
	ss = next.(*sessionScreen)
	next, cmd := ss.Update(enter()) // the first mode row
	if cmd != nil {
		runLaunchBatch(t, cmd)
	}
	return next.(*sessionScreen), dl
}

// TestSyncAskAbsentOnPlainDownload (PR174 S3): a plain single-dub
// batch in a MERGED session — config "ask", no marked audio set —
// must NOT open the analysis prompt: nothing external rides the
// video, there is nothing to sync. The batch dispatches directly,
// every task carries AudioSync=false.
func TestSyncAskAbsentOnPlainDownload(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, _ := pr174SyncSession(t, "ask", nil)
	ss, dl := pr174DriveToBatch(t, s)
	if ss.state == sessionStateSyncAsk {
		t.Fatal("a plain single-dub download must not open the sync-ask prompt")
	}
	if len(dl.downloads) == 0 {
		t.Fatal("the plain batch must dispatch without the prompt")
	}
	for _, task := range dl.downloads {
		if task.AudioSync {
			t.Fatalf("plain task %s must not carry AudioSync", task.EpisodeNum)
		}
	}
}

// TestSyncAskFiresOnMarkedAudioSet (PR174 S3): the mixing batch —
// video+audio with a marked download audio set — is the one shape
// the analysis belongs to: config "ask" opens the two-choice prompt,
// «Да» starts the batch with AudioSync on every task.
func TestSyncAskFiresOnMarkedAudioSet(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, _ := pr174SyncSession(t, "ask", []string{"[anilib] AniLib"})
	ss, dl := pr174DriveToBatch(t, s)
	if ss.state != sessionStateSyncAsk {
		t.Fatalf("state = %v, want the sync-ask menu on the mixing batch", ss.state)
	}
	_, cmd := ss.Update(enter()) // «Да»
	if cmd == nil {
		t.Fatal("«Да» must dispatch the batch")
	}
	runLaunchBatch(t, cmd)
	if len(dl.downloads) == 0 {
		t.Fatal("the mixing batch must start")
	}
	for _, task := range dl.downloads {
		if !task.AudioSync {
			t.Fatalf("mixing task %s must carry AudioSync", task.EpisodeNum)
		}
		if len(task.AudioSet) == 1 && task.AudioSet[0] != "[anilib] AniLib" {
			t.Fatalf("task %s must carry the marked audio set, got %v", task.EpisodeNum, task.AudioSet)
		}
	}
}

// TestSyncAskYesConfigNeedsMixingToo (PR174 S3): config "yes" is a
// non-interactive decision about the SAME flow — a plain batch still
// runs unsynchronized (there is nothing to analyze), a mixing batch
// runs synced without any prompt.
func TestSyncAskYesConfigNeedsMixingToo(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, _ := pr174SyncSession(t, "yes", nil)
	ss, dl := pr174DriveToBatch(t, s)
	if ss.state == sessionStateSyncAsk {
		t.Fatal("config yes must not open the menu")
	}
	for _, task := range dl.downloads {
		if task.AudioSync {
			t.Fatalf("plain task %s under config yes must stay unsynchronized (nothing to analyze)", task.EpisodeNum)
		}
	}

	s2, _ := pr174SyncSession(t, "yes", []string{"[anilib] AniLib"})
	ss2, dl2 := pr174DriveToBatch(t, s2)
	if ss2.state == sessionStateSyncAsk {
		t.Fatal("config yes must not open the menu")
	}
	for _, task := range dl2.downloads {
		if !task.AudioSync {
			t.Fatalf("mixing task %s under config yes must carry AudioSync", task.EpisodeNum)
		}
	}
}
