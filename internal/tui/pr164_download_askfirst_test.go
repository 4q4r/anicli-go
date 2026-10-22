package tui

// PR164 owner-reported: «скачать все серии» without a chosen dub
// silently downloaded an arbitrary dub — with an empty preferred the
// per-episode ladder walks sortedEmbedKeys, so the first LEXICOGRAPHIC
// dub with live streams won (deterministic, but random to the owner:
// «вообще рандомную озвучку даже не спросив какую я хочу»).
//
// The ask-first doctrine survives the PR174 stream picker: no dub
// chosen → the STREAM picker opens BEFORE anything downloads (the
// merged quality·dub·provider list — the owner picks streams, not
// «one studio for everything»); cancel aborts with nothing
// downloaded; the picked stream drives the whole batch STRICTLY —
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
		Download:     &fakeDownload{},
		DownloadSync: "no", // the #164/#165 dub semantics under test — sync stays out
		Log:          testLogger(),
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s, deps.Download.(*fakeDownload)
}

// openDownloadPicker drives the real path to the PR174 stream picker
// and settles its resolve: «Скачать серии» → the range 1-3 → the
// content-mode pick (the видео+аудио default) → the picker with the
// merged stream rows. jumpBackground is applied later, on the mode
// menu (the returned screen sits on the picker).
func openDownloadPicker(t *testing.T, s *sessionScreen) *sessionScreen {
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
	if ss.state != sessionStateDownloadContent {
		t.Fatalf("after the range the content-mode menu opens, got %v", ss.state)
	}
	next, cmd := ss.Update(enter()) // видео+аудио, the default first row
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadStream {
		t.Fatalf("the content pick must open the stream picker, got %v", ss.state)
	}
	if cmd == nil {
		t.Fatal("the content pick must start the stream resolve command")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("the stream resolve must settle with a msg")
	}
	next, _ = ss.Update(msg)
	return next.(*sessionScreen)
}

// pickDownloadStream picks the picker row of the given quality+dub
// and lands the screen on the execution-mode menu.
func pickDownloadStream(t *testing.T, ss *sessionScreen, quality, dub string) *sessionScreen {
	t.Helper()
	found := false
	for i, e := range ss.streamEntries {
		if e.Quality == quality && e.DubKey == dub {
			ss.qualityList.Jump(i)
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no %s %s row in the picker: %+v", quality, dub, ss.streamEntries)
	}
	next, _ := ss.Update(enter())
	picked := next.(*sessionScreen)
	if picked.state != sessionStateDownloadMode {
		t.Fatalf("the stream pick must open the mode menu, got %v", picked.state)
	}
	return picked
}

// TestPR164AskFirstNoDubChosenOpensStreamPicker: the owner's scenario
// — a fresh session (never played, no resumed dub) downloads episodes.
// The content pick must NOT dispatch a batch: the stream picker opens
// first, listing the merged quality·dub·provider rows, and nothing
// has been downloaded yet.
func TestPR164AskFirstNoDubChosenOpensStreamPicker(t *testing.T) {
	s, dl := pr164AskSession(t)
	if s.videoDub != "" {
		t.Fatalf("precondition: a fresh session has no remembered dub, got %q", s.videoDub)
	}
	ss := openDownloadPicker(t, s)

	if ss.state != sessionStateDownloadStream {
		t.Fatalf("no dub chosen must open the stream picker, got %v", ss.state)
	}
	v := ss.View().Content
	if !contains(v, "Pick a stream (quality · dub · provider):") {
		t.Fatalf("the stream picker must render:\n%s", v)
	}
	for _, want := range []string{"1080p", "720p", "AniLib", "Дубль 1"} {
		if !contains(v, want) {
			t.Fatalf("the picker rows must carry %q:\n%s", want, v)
		}
	}
	if len(dl.downloads) != 0 || len(dl.submitted) != 0 {
		t.Fatalf("nothing may download before the pick, got %d downloads/%d submitted",
			len(dl.downloads), len(dl.submitted))
	}
}

// TestPR164AskFirstCancelDownloadsNothing: Esc from the stream
// picker aborts the download — nothing is downloaded or queued.
func TestPR164AskFirstCancelDownloadsNothing(t *testing.T) {
	s, dl := pr164AskSession(t)
	ss := openDownloadPicker(t, s)

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

// TestPR164AskFirstPickedDubDrivesBatch: the picked stream drives the
// WHOLE batch strictly — the episodes carrying its dub download, the
// others are skipped with the compact «пропущены» attribution. No
// episode may fall back to another dub (no silent substitution).
func TestPR164AskFirstPickedDubDrivesBatch(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	s, dl := pr164AskSession(t)
	ss := openDownloadPicker(t, s)

	// Pick the animego dub — the one only ep1 carries.
	ss = pickDownloadStream(t, ss, "720", "[animego] Дубль 1")
	if ss.videoDub != "[animego] Дубль 1" || ss.lastQuality != "720" {
		t.Fatalf("the pick must record dub+quality, got %q/%q", ss.videoDub, ss.lastQuality)
	}
	next, cmd := ss.Update(enter()) // foreground, the first mode row
	if cmd == nil {
		t.Fatal("the mode pick must dispatch the download batch")
	}
	ss = next.(*sessionScreen)

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

// TestPR164AskFirstPickSettlesOnMenuSurface (review round 2, blocker):
// the mode pick must LEAVE the picker — the batch and its settle run
// on the session menu surface, the same owner surface the
// remembered-dub path uses. A stranded menu would render dead for the
// whole batch and turn Enter-after-settle into the previously
// unreachable silent-nothing default (PR41 B2: never silently
// nothing); the next Enter re-opens the download prompt instead —
// visible, and never a second batch.
func TestPR164AskFirstPickSettlesOnMenuSurface(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr164AskSession(t)
	ss := openDownloadPicker(t, s)
	ss = pickDownloadStream(t, ss, "720", "[animego] Дубль 1")

	next, cmd := ss.Update(enter()) // foreground
	if cmd == nil {
		t.Fatal("the mode pick must dispatch the download batch")
	}
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("the mode pick must return to the session menu, got %v", ss.state)
	}

	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	applied, _ := ss.Update(settled)
	ss = applied.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("the settle must land on the session menu, got %v", ss.state)
	}
	if !contains(ss.status, "skipped: 2–3 (no dub [animego] Дубль 1)") {
		t.Fatalf("the settle report must render on the menu surface:\n%s", ss.status)
	}

	// Enter-after-settle: a visible surface (the download prompt
	// re-entry), never a second batch, never a silent nothing.
	downloads := len(dl.downloads)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("enter after the settle must open a visible surface, got %v", ss.state)
	}
	if len(dl.downloads) != downloads {
		t.Fatalf("enter after the settle must not re-dispatch, got %d downloads", len(dl.downloads))
	}
}

// pr164LazySession builds a loaded session whose listing knows NO
// dubs (empty RawEmbeds everywhere — the lazy-provider shape).
func pr164LazySession(t *testing.T) (*sessionScreen, *fakeDownload) {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{}},
					{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{}},
				},
			},
		},
		Download:     &fakeDownload{},
		DownloadSync: "no", // the #164/#165 dub semantics under test — sync stays out
		Log:          testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s, deps.Download.(*fakeDownload)
}

// TestPR164AskFirstEmptyUnionSuppressesWarning (review round 2): an
// empty picker AND an empty dub union render the honest «Нет
// доступных озвучек» empty state ALONE — the «⚠ Озвучка не выбрана»
// ask note must not pair with it (a contradictory pair begs a choice
// the menu cannot offer).
func TestPR164AskFirstEmptyUnionSuppressesWarning(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr164LazySession(t)
	ss := openDownloadPicker(t, s)

	// The empty resolve falls back to the #164 union menu — itself
	// empty: the honest empty state alone.
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("the empty-union ask menu must still open, got %v", ss.state)
	}
	v := ss.View().Content
	if !contains(v, "No dubs available") {
		t.Fatalf("the empty state must render:\n%s", v)
	}
	if contains(v, "No dub chosen") {
		t.Fatalf("the ask warning must be suppressed on an empty union:\n%s", v)
	}
	if len(dl.downloads) != 0 || len(dl.submitted) != 0 {
		t.Fatalf("nothing may download, got %d downloads/%d submitted",
			len(dl.downloads), len(dl.submitted))
	}
	next, _ := ss.Update(esc())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("cancel must return to the menu, got %v", ss.state)
	}
}

// TestPR164AskFirstPickRememberedForBackground: ask-first covers the
// background mode too — the stream pick precedes the mode choice and
// the batch queues strictly (carrying episodes only, the picked dub).
func TestPR164AskFirstPickRememberedForBackground(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s, dl := pr164AskSession(t)
	ss := openDownloadPicker(t, s)
	ss = pickDownloadStream(t, ss, "720", "[animego] Дубль 1")

	ss.modeList.Jump(1) // background
	next, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatal("the background pick must dispatch the queued batch")
	}
	ss = next.(*sessionScreen)
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
