package tui

// PR165: download modes («видео+аудио» / «только видео» / «только
// аудио») and the multi-audio download set. The streams picker gains
// the established space-multiselect (the search-results CheckList
// pattern): SPACE toggles the cursor row's DUB into the ordered
// download audio set, `d` moves the default-disposition mark; the
// first marked audio is the default. The batch carries the set per
// task and degrades per episode: a dub an episode does not carry
// drops (loud per-episode row), a set that carries nowhere skips the
// episode with the compact #161 attribution.

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// pr165PickerSession loads a session and fills the merged stream
// picker with three deterministic rows (quality-descending): 1080
// animego, 720 anilib, 480 animevib. Languages feed the metadata.
func pr165PickerSession(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			langs:    map[string]string{"animego": "ru", "anilib": "ja"},
		},
		Playback: &fakePlayback{},
		Download: &fakeDownload{},
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.beginStreamResolve("")
	s.Update(streamResolvedMsg{gen: s.resolveGen, entries: []streamEntry{
		{Quality: "1080", DubKey: "[animego] Дубль 1", Source: contracts.VideoSource{URL: "v1080"}},
		{Quality: "720", DubKey: "[anilib] AniLib", Source: contracts.VideoSource{URL: "a720"}},
		{Quality: "480", DubKey: "[animevib] Vib", Source: contracts.VideoSource{URL: "x480"}},
	}})
	if s.state != sessionStateQuality {
		t.Fatalf("the settle must fill the merged list, got %v", s.state)
	}
	return s
}

// pr165Space presses space (the row under the cursor toggles).
func pr165Space(s *sessionScreen) *sessionScreen {
	next, _ := s.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	return next.(*sessionScreen)
}

// pr165Key presses a single rune key.
func pr165Key(s *sessionScreen, r rune) *sessionScreen {
	next, _ := s.Update(tea.KeyPressMsg{Code: r})
	return next.(*sessionScreen)
}

// TestPR165PickerSpaceMarksAudioSet: space toggles the row's DUB into
// the ordered download audio set — quality rows of one dub share the
// mark; the marker renders on every row of the marked dub.
func TestPR165PickerSpaceMarksAudioSet(t *testing.T) {
	s := pr165PickerSession(t)

	// Cursor on row 0 (1080 animego): space marks the animego dub.
	s = pr165Space(s)
	if len(s.markedAudio) != 1 || s.markedAudio[0] != "[animego] Дубль 1" {
		t.Fatalf("space must mark the cursor row's dub, got %v", s.markedAudio)
	}

	// Down to anilib, mark it too: the set keeps MARKING order.
	s = pr165Key(s, 'j')
	s = pr165Space(s)
	if len(s.markedAudio) != 2 || s.markedAudio[0] != "[animego] Дубль 1" || s.markedAudio[1] != "[anilib] AniLib" {
		t.Fatalf("the set must keep marking order, got %v", s.markedAudio)
	}

	// Space again on anilib: unmarked, the set keeps the rest.
	s = pr165Space(s)
	if len(s.markedAudio) != 1 || s.markedAudio[0] != "[animego] Дубль 1" {
		t.Fatalf("space must toggle off, got %v", s.markedAudio)
	}

	// The marker renders on the marked dub's rows: ● for marked.
	v := s.View().Content
	if !strings.Contains(v, "●") {
		t.Fatalf("the marked row must carry the ● marker:\n%s", v)
	}
}

// TestPR165PickerDefaultBinding pins the `d` state machine: the FIRST
// marked audio is the default; `d` on a marked row moves the default
// there; `d` on the default moves it to the earliest remaining
// marked; unmarking the default promotes the earliest remaining. A
// non-empty set always has exactly one default.
func TestPR165PickerDefaultBinding(t *testing.T) {
	s := pr165PickerSession(t)

	// Mark animego, anilib, animevib (rows 0, 1, 2): the first marked
	// is the default.
	s = pr165Space(s)
	s = pr165Key(s, 'j')
	s = pr165Space(s)
	s = pr165Key(s, 'j')
	s = pr165Space(s)
	if s.defaultAudio != "[animego] Дубль 1" {
		t.Fatalf("the first marked audio must be the default, got %q", s.defaultAudio)
	}

	// `d` on animevib (marked): the default moves there.
	if s.defaultAudio != "[animevib] Vib" {
		s = pr165Key(s, 'd')
	}
	if s.defaultAudio != "[animevib] Vib" {
		t.Fatalf("d on a marked row must move the default, got %q", s.defaultAudio)
	}

	// Unmark the default: the earliest remaining becomes the default.
	s = pr165Space(s)
	if len(s.markedAudio) != 2 || s.markedAudio[0] != "[animego] Дубль 1" {
		t.Fatalf("the set must keep the remaining dubs in order, got %v", s.markedAudio)
	}
	if s.defaultAudio != "[animego] Дубль 1" {
		t.Fatalf("unmarking the default must promote the earliest remaining, got %q", s.defaultAudio)
	}

	// `d` on the default again: back to the earliest remaining other.
	s = pr165Key(s, 'k')
	s = pr165Key(s, 'k') // up to animego (the default)
	s = pr165Key(s, 'd')
	if s.defaultAudio != "[anilib] AniLib" {
		t.Fatalf("d on the default must fall back to the earliest remaining, got %q", s.defaultAudio)
	}

	// Unmark a non-default row: the default stays.
	s = pr165Space(s) // cursor on animego (marked, non-default)
	if len(s.markedAudio) != 1 || s.markedAudio[0] != "[anilib] AniLib" {
		t.Fatalf("the set must keep the remaining dub, got %v", s.markedAudio)
	}
	if s.defaultAudio != "[anilib] AniLib" {
		t.Fatalf("unmarking a non-default row must keep the default, got %q", s.defaultAudio)
	}

	// Unmark the last one: the set and the default empty together.
	s = pr165Key(s, 'j')
	s = pr165Space(s)
	if len(s.markedAudio) != 0 || s.defaultAudio != "" {
		t.Fatalf("an empty set must have no default, got %v/%q", s.markedAudio, s.defaultAudio)
	}
}

// TestPR165PickerDefaultMarkerRenders pins the render contract: the
// default row shows ★, other marked rows ●, unmarked rows nothing.
func TestPR165PickerDefaultMarkerRenders(t *testing.T) {
	s := pr165PickerSession(t)

	s = pr165Space(s)    // animego marked + default
	s = pr165Key(s, 'j') // → anilib
	s = pr165Space(s)    // anilib marked
	s = pr165Key(s, 'd') // anilib becomes the default

	v := s.View().Content
	if !strings.Contains(v, "★") {
		t.Fatalf("the default row must carry the ★ marker:\n%s", v)
	}
	if !strings.Contains(v, "●") {
		t.Fatalf("the marked non-default row must carry ●:\n%s", v)
	}
}

// TestPR165PickerHintRenders pins the affordance: the picker renders
// the space/d hint so the marks are discoverable.
func TestPR165PickerHintRenders(t *testing.T) {
	s := pr165PickerSession(t)
	v := s.View().Content
	if !strings.Contains(v, "Space") && !strings.Contains(v, "Пробел") {
		t.Fatalf("the picker must hint the space marking:\n%s", v)
	}
}

// TestPR165DownloadContentModeMenu pins the new menu between the
// range prompt and the foreground/background pick: three modes,
// видео+аудио first (the default), Back returns to the session menu.
func TestPR165DownloadContentModeMenu(t *testing.T) {
	s := newSessionForTests(t)
	s.deps.Download = &fakeDownload{}

	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)

	if ss.state != sessionStateDownloadContent {
		t.Fatalf("after the range the content-mode menu opens, got %v", ss.state)
	}
	v := ss.View().Content
	for _, want := range []string{"Video+Audio", "Video only", "Audio only"} {
		if !strings.Contains(v, want) {
			t.Fatalf("the content modes must be offered (%q missing):\n%s", want, v)
		}
	}

	// Pick «Video only» (second row): the execution-mode menu follows.
	ss.contentList.Jump(1)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the content pick must open the execution-mode menu, got %v", ss.state)
	}
	if ss.downloadContentMode != "video_only" {
		t.Fatalf("the pick must be remembered, got %q", ss.downloadContentMode)
	}

	// Back from the content menu returns to the session menu.
	ss2 := newSessionForTests(t)
	ss2.deps.Download = &fakeDownload{}
	ss2.list.Jump(sessionActionIndex(ss2, "download"))
	next, _ = ss2.Update(enter())
	ss2 = next.(*sessionScreen)
	ss2.rangeInput.typeText("1")
	next, _ = ss2.Update(enter())
	ss2 = next.(*sessionScreen)
	next, _ = ss2.Update(esc())
	ss2 = next.(*sessionScreen)
	if ss2.state != sessionStateMenu {
		t.Fatalf("back from the content menu must return to the menu, got %v", ss2.state)
	}
}

// TestPR165BatchCarriesModeAndAudioSet pins the task plumbing: the
// batch tasks carry the picked content mode and the full marked audio
// set with the default mark — every episode muxes the whole set.
func TestPR165BatchCarriesModeAndAudioSet(t *testing.T) {
	s, _ := pr164AskSession(t)
	s.markedAudio = []string{"[anilib] AniLib", "[animego] Дубль 1"}
	s.defaultAudio = "[animego] Дубль 1"
	dl := s.deps.Download.(*fakeDownload)

	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-3")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	// видео+аудио is the first row: Enter picks the default mode and
	// lands on the execution-mode menu.
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the content pick must open the mode menu, got %v", ss.state)
	}

	// pr164AskSession has no remembered dub — the ask-first menu opens
	// on the execution pick.
	next, cmd := ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("no dub chosen must open the dub menu, got %v", ss.state)
	}
	if cmd != nil {
		t.Fatal("the execution pick must not dispatch before the dub pick")
	}
	_, cmd = ss.Update(enter()) // pick the first menu dub
	if cmd == nil {
		t.Fatal("the dub pick must dispatch the batch")
	}
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	_, _ = ss.Update(settled)

	if len(dl.downloads) == 0 {
		t.Fatal("the batch must download")
	}
	for _, task := range dl.downloads {
		if task.Mode != "video_audio" {
			t.Fatalf("task %s must carry the content mode, got %q", task.EpisodeNum, task.Mode)
		}
		if strings.Join(task.AudioSet, "|") != "[anilib] AniLib|[animego] Дубль 1" {
			t.Fatalf("task %s must carry the marked set, got %v", task.EpisodeNum, task.AudioSet)
		}
		if task.DefaultAudio != "[animego] Дубль 1" {
			t.Fatalf("task %s must carry the default, got %q", task.EpisodeNum, task.DefaultAudio)
		}
	}
}

// TestPR165PerEpisodeAudioDegradation pins mandate #7 at the batch
// level: the marked set resolves PER EPISODE — a dub the episode does
// not carry drops (the episode still downloads with the rest); an
// episode carrying NONE of the marked dubs skips with the compact
// attribution — never a silent video-only file.
func TestPR165PerEpisodeAudioDegradation(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
					{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
				},
				// ep1 alone carries AniLib: ep2 drops it (and still
				// downloads with animego), ep3 carries neither marked
				// dub (a third dub only) and skips entirely.
				"anilib": {
					{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1a"}}},
				},
				"animevib": {
					{Num: "3", RawID: "x3", RawEmbeds: map[string][]string{"Vib": {"u3x"}}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				"[anilib] AniLib":   {Links: map[string]contracts.VideoSource{"720": {URL: "a-lib"}}},
				"[animevib] Vib":    {Links: map[string]contracts.VideoSource{"720": {URL: "x-vib"}}},
			},
		},
		Download: &fakeDownload{},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
		{Title: "Тайтл", URL: "u3", SourceID: "animevib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	marked := []string{"[animego] Дубль 1", "[anilib] AniLib"}
	tasks := []DownloadTask{
		{AnimeTitle: "Тайтл", EpisodeNum: "1", Episode: s.episodes["1"], AudioSet: marked},
		{AnimeTitle: "Тайтл", EpisodeNum: "2", Episode: s.episodes["2"], AudioSet: marked},
		{AnimeTitle: "Тайтл", EpisodeNum: "3", Episode: s.episodes["3"], AudioSet: marked},
	}

	ch := make(chan string, 64)
	var settled downloadSettledMsg
	done := make(chan struct{})
	go func() {
		defer close(done)
		settled = runForegroundDownload(context.Background(), deps, tasks, "[animego] Дубль 1", false, ch)
		close(ch)
	}()
	for range ch {
	}
	<-done

	if settled.count != 2 || settled.total != 3 {
		t.Fatalf("ep1 and ep2 download (ep3 skips): got %d/%d", settled.count, settled.total)
	}
	if !settled.report[2].SkipAudioSet {
		t.Fatalf("ep3 must be typed as an audio-set skip, got %+v", settled.report[2])
	}
	if len(settled.report[0].DroppedAudio) != 0 {
		t.Fatalf("ep1 carries both dubs, got drops %v", settled.report[0].DroppedAudio)
	}
	if len(settled.report[1].DroppedAudio) != 1 || settled.report[1].DroppedAudio[0] != "[anilib] AniLib" {
		t.Fatalf("ep2 must drop the missing marked dub, got %v", settled.report[1].DroppedAudio)
	}

	status := renderDownloadSettle(settled)
	if !strings.Contains(status, "skipped: 3") || !strings.Contains(status, "no marked audios") {
		t.Fatalf("the compact audio-set skip line missing:\n%s", status)
	}
	if !strings.Contains(status, "without marked: AniLib") {
		t.Fatalf("the ep2 row must name the dropped dub:\n%s", status)
	}
}

// TestPR165PerEpisodeDroppedRowRenders pins the loud per-episode
// degradation row: an episode that downloaded without one of the
// marked dubs names what dropped right on its report line.
func TestPR165PerEpisodeDroppedRowRenders(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	msg := downloadSettledMsg{
		count: 1, total: 1,
		report: []downloadEpisodeReport{
			{Episode: "2", Dub: "[animego] Дубль 1", Path: "/dl/ep2.mkv",
				DroppedAudio: []string{"[anilib] AniLib"}},
		},
	}
	status := renderDownloadSettle(msg)
	if !strings.Contains(status, "AniLib") || !strings.Contains(status, "without marked") {
		t.Fatalf("the dropped-dub row must name the missing dub:\n%s", status)
	}
}

// droppingDownload fails marked dubs at DOWNLOAD time (the M2
// window): the resolve phase carried them, the download leg drops
// them — the only source of the row suffix in the pins below.
type droppingDownload struct {
	fakeDownload
	dropped []string
}

func (f *droppingDownload) Download(ctx context.Context, task DownloadTask) (string, []string, error) {
	path, _, err := f.fakeDownload.Download(ctx, task)
	return path, f.dropped, err
}

// mergedEpisodeOne runs the session merge for one animego episode and
// returns the merged episode 1 (the tagged-embed shape the resolve
// probes and the report flow consume).
func mergedEpisodeOne(t *testing.T, deps *Deps) contracts.Episode {
	t.Helper()
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s.episodes["1"]
}

// downloadTimeDropDeps builds a one-episode session whose resolve
// phase carries BOTH marked dubs (nothing drops at resolve) while the
// download service reports the given download-time drops.
func downloadTimeDropDeps(dropped []string) *Deps {
	return &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{
						"Дубль 1": {"u1v"}, "AniDub": {"u1x"},
					}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				"[animego] AniDub":  {Links: map[string]contracts.VideoSource{"720": {URL: "x-go"}}},
			},
		},
		Download: &droppingDownload{dropped: dropped},
		Log:      testLogger(),
	}
}

// TestDownloadTimeDropLandsOnReportRow pins M2 end to end: a dub the
// resolve phase carried but the download leg dropped lands on the
// path row in the established suffix («without marked: …») — a
// download-time drop is never invisible. The no-drop row stays
// unchanged.
func TestDownloadTimeDropLandsOnReportRow(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	deps := downloadTimeDropDeps([]string{"[animego] AniDub"})
	marked := []string{"[animego] Дубль 1", "[animego] AniDub"}
	tasks := []DownloadTask{
		{AnimeTitle: "Тайтл", EpisodeNum: "1", Episode: mergedEpisodeOne(t, deps), AudioSet: marked},
	}

	ch := make(chan string, 64)
	var settled downloadSettledMsg
	done := make(chan struct{})
	go func() {
		defer close(done)
		settled = runForegroundDownload(context.Background(), deps, tasks, "[animego] Дубль 1", false, ch)
		close(ch)
	}()
	for range ch {
	}
	<-done

	if settled.err != nil || settled.count != 1 {
		t.Fatalf("the episode downloads: err %v count %d", settled.err, settled.count)
	}
	if len(settled.report[0].DroppedAudio) != 1 || settled.report[0].DroppedAudio[0] != "[animego] AniDub" {
		t.Fatalf("the download-time drop must merge into the report, got %v", settled.report[0].DroppedAudio)
	}
	status := renderDownloadSettle(settled)
	if !strings.Contains(status, "without marked: AniDub") {
		t.Fatalf("the path row must name the download-time drop:\n%s", status)
	}

	// The no-drop twin: the same flow without download-time drops
	// keeps the row suffix-free.
	deps2 := downloadTimeDropDeps(nil)
	tasks2 := []DownloadTask{
		{AnimeTitle: "Тайтл", EpisodeNum: "1", Episode: mergedEpisodeOne(t, deps2), AudioSet: marked},
	}
	ch2 := make(chan string, 64)
	done2 := make(chan struct{})
	var settled2 downloadSettledMsg
	go func() {
		defer close(done2)
		settled2 = runForegroundDownload(context.Background(), deps2, tasks2, "[animego] Дубль 1", false, ch2)
		close(ch2)
	}()
	for range ch2 {
	}
	<-done2
	status2 := renderDownloadSettle(settled2)
	if strings.Contains(status2, "without marked") {
		t.Fatalf("a no-drop download must keep the row clean:\n%s", status2)
	}
}

// TestAllDroppedAtDownloadRowNamesEveryDub pins the M2 edge wording:
// every marked dub dead at download time leaves the video-only file
// and the row names them all in the established suffix.
func TestAllDroppedAtDownloadRowNamesEveryDub(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	deps := downloadTimeDropDeps([]string{"[animego] Дубль 1", "[animego] AniDub"})
	marked := []string{"[animego] Дубль 1", "[animego] AniDub"}
	tasks := []DownloadTask{
		{AnimeTitle: "Тайтл", EpisodeNum: "1", Episode: mergedEpisodeOne(t, deps), AudioSet: marked},
	}

	ch := make(chan string, 64)
	var settled downloadSettledMsg
	done := make(chan struct{})
	go func() {
		defer close(done)
		settled = runForegroundDownload(context.Background(), deps, tasks, "[animego] Дубль 1", false, ch)
		close(ch)
	}()
	for range ch {
	}
	<-done

	if settled.err != nil || settled.count != 1 {
		t.Fatalf("the episode downloads video-only: err %v count %d", settled.err, settled.count)
	}
	status := renderDownloadSettle(settled)
	if !strings.Contains(status, "without marked: Дубль 1, AniDub") {
		t.Fatalf("the video-only row must name every dropped dub:\n%s", status)
	}
}

// TestPR165AudioOnlyModeSkipsAudioSet pins the mode semantics: the
// «только аудио» batch resolves the CHOSEN dub (ask-first doctrine)
// and carries no audio set — one codec-preserving file per episode.
func TestPR165AudioOnlyModeSkipsAudioSet(t *testing.T) {
	s, dl := pr164AskSession(t)
	s.markedAudio = []string{"[anilib] AniLib"} // must be ignored
	_ = dl

	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-3")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(contentPick(t, ss, "audio_only")) // «только аудио»
	next, _ = next.Update(enter())                        // foreground
	ss = next.(*sessionScreen)
	// Ask-first: the dub menu opens; pick the SECOND row ([animego]
	// Дубль 1 — only ep1 carries it).
	ss.redubList.Jump(1)
	next, cmd := ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("the pick must leave the menu, got %v", ss.state)
	}
	if cmd == nil {
		t.Fatal("the dub pick must dispatch the audio-only batch")
	}
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	_, _ = ss.Update(settled)

	if len(dl.downloads) != 1 || dl.downloads[0].EpisodeNum != "1" {
		t.Fatalf("only ep1 carries the picked dub, got %+v", dl.downloads)
	}
	if dl.downloads[0].Mode != "audio_only" {
		t.Fatalf("the task must carry the audio-only mode, got %q", dl.downloads[0].Mode)
	}
	if dl.downloads[0].AudioSet != nil {
		t.Fatalf("audio-only must not carry the marked set, got %v", dl.downloads[0].AudioSet)
	}
}

// contentPick drives the content-mode menu onto the row with the
// given Value (a test affordance mirroring cursor movement).
func contentPick(t *testing.T, ss *sessionScreen, value string) tea.KeyPressMsg {
	t.Helper()
	for i, c := range ss.contentList.Menu().Items {
		if c.Value == value {
			ss.contentList.Jump(i)
			return enter()
		}
	}
	t.Fatalf("content mode %q not offered", value)
	return enter()
}
