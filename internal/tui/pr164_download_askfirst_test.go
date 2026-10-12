package tui

// PR164 owner-reported: «скачать все серии» without a chosen dub
// silently downloaded an arbitrary dub — with an empty preferred the
// per-episode ladder walks sortedEmbedKeys, so the first LEXICOGRAPHIC
// dub with live streams won (deterministic, but random to the owner:
// «вообще рандомную озвучку даже не спросив какую я хочу»).
//
// The ask-first doctrine (the PR #10/#11 playback settle semantics)
// ports to the download path here: no dub chosen → the dub menu opens
// BEFORE anything downloads (typed warning + menu); cancel aborts with
// nothing downloaded; the picked dub drives the whole batch STRICTLY —
// episodes not carrying it are skipped with the compact «пропущены»
// attribution (#161 format), never substituted per episode.

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// pr164AskSession builds a loaded session with per-episode dub
// variance: ep1 carries both dubs, ep2 and ep3 carry only AniLib.
func pr164AskSession(t *testing.T) (*sessionScreen, *fakeDownload) {
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
					{Num: "3", RawID: "b3", RawEmbeds: map[string][]string{"AniLib": {"w3b"}}},
				},
			},
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
	return s, deps.Download.(*fakeDownload)
}

// openDownloadAsk drives the real path to the ask-first surface:
// «Скачать серии» → the range 1-3 → the mode pick (cursor on
// foreground unless jumpBackground).
func openDownloadAsk(t *testing.T, s *sessionScreen, jumpBackground bool) *sessionScreen {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	ss.rangeInput.typeText("1-3")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("after the range the mode menu opens, got %v", ss.state)
	}
	if jumpBackground {
		ss.modeList.Jump(1)
	}
	next, _ = ss.Update(enter())
	return next.(*sessionScreen)
}

// TestPR164AskFirstNoDubChosenOpensDubMenu: the owner's scenario — a
// fresh session (never played, no resumed dub) downloads all episodes.
// The mode pick must NOT dispatch a batch: the dub menu opens first,
// listing the batch's dubs, and nothing has been downloaded yet.
func TestPR164AskFirstNoDubChosenOpensDubMenu(t *testing.T) {
	s, dl := pr164AskSession(t)
	if s.videoDub != "" {
		t.Fatalf("precondition: a fresh session has no remembered dub, got %q", s.videoDub)
	}
	ss := openDownloadAsk(t, s, false)

	if ss.state != sessionStateDownloadDub {
		t.Fatalf("no dub chosen must open the download dub menu, got %v", ss.state)
	}
	v := ss.View().Content
	if !contains(v, "Dub to download") {
		t.Fatalf("the dub menu must render:\n%s", v)
	}
	if !contains(v, "Дубль 1 · animego") || !contains(v, "AniLib · anilib") {
		t.Fatalf("the menu must list the batch's dubs:\n%s", v)
	}
	if len(dl.downloads) != 0 || len(dl.submitted) != 0 {
		t.Fatalf("nothing may download before the pick, got %d downloads/%d submitted",
			len(dl.downloads), len(dl.submitted))
	}
}

// TestPR164AskFirstCancelDownloadsNothing: Back from the dub menu
// aborts the download — nothing is downloaded or queued.
func TestPR164AskFirstCancelDownloadsNothing(t *testing.T) {
	s, dl := pr164AskSession(t)
	ss := openDownloadAsk(t, s, false)

	next, _ := ss.Update(esc())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("cancel must return to the session menu, got %v", ss.state)
	}
	if len(dl.downloads) != 0 || len(dl.submitted) != 0 {
		t.Fatalf("cancel must download nothing, got %d downloads/%d submitted",
			len(dl.downloads), len(dl.submitted))
	}
}

// TestPR164AskFirstPickedDubDrivesBatch: the picked dub drives the
// WHOLE batch strictly — the episodes carrying it download with it,
// the others are skipped with the compact «пропущены» attribution.
// No episode may fall back to another dub (no silent substitution).
func TestPR164AskFirstPickedDubDrivesBatch(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	s, dl := pr164AskSession(t)
	ss := openDownloadAsk(t, s, false)

	// The menu lists the batch's dubs in sortedEmbedKeys order —
	// [anilib] AniLib first, [animego] Дубль 1 second. Pick the animego
	// dub — the one only ep1 carries.
	ss.redubList.Jump(1)
	next, cmd := ss.Update(enter())
	ss = next.(*sessionScreen)
	if cmd == nil {
		t.Fatal("the dub pick must dispatch the download batch")
	}

	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	applied, _ := ss.Update(settled)
	status := applied.(*sessionScreen).status

	if settled.count != 1 || settled.total != 3 {
		t.Fatalf("only ep 1 carries the picked dub: 1 of 3 must download, got %d/%d",
			settled.count, settled.total)
	}
	if len(dl.downloads) != 1 || dl.downloads[0].EpisodeNum != "1" || dl.downloads[0].DubID != "[animego] Дубль 1" {
		t.Fatalf("ep 1 must download with the PICKED dub, got %+v", dl.downloads)
	}
	// The compact skip line: episodes 2–3 skipped run-compressed, the
	// #161 «пропущены:» shape naming the missing dub.
	want := "пропущены: 2–3 (нет озвучки [animego] Дубль 1)"
	if !contains(status, want) {
		t.Fatalf("the settle must carry the compact skip line %q:\n%s", want, status)
	}
}

// TestPR164AskFirstPickRememberedForBackground: ask-first covers the
// background mode too — the picked mode survives the menu and the
// batch queues strictly (carrying episodes only, the picked dub).
func TestPR164AskFirstPickRememberedForBackground(t *testing.T) {
	s, dl := pr164AskSession(t)
	ss := openDownloadAsk(t, s, true)

	if ss.state != sessionStateDownloadDub {
		t.Fatalf("no dub chosen must open the download dub menu, got %v", ss.state)
	}
	ss.redubList.Jump(1) // [animego] Дубль 1
	next, cmd := ss.Update(enter())
	ss = next.(*sessionScreen)
	if cmd == nil {
		t.Fatal("the dub pick must dispatch the queued batch")
	}
	msg := cmd()
	queued, ok := msg.(backgroundQueuedMsg)
	if !ok {
		t.Fatalf("the background pick must queue the batch, got %T", msg)
	}
	_, _ = ss.Update(queued)

	if queued.queued != 1 || queued.total != 3 {
		t.Fatalf("only the carrying episode must queue, got %d/%d", queued.queued, queued.total)
	}
	if len(dl.submitted) != 1 || dl.submitted[0].DubID != "[animego] Дубль 1" {
		t.Fatalf("the queued task must carry the picked dub, got %+v", dl.submitted)
	}
}
