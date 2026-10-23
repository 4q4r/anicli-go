package tui

// PR174 S1/S2: the download flow offers the STREAM picker — the
// merged quality·dub·provider list of the watch flow — when the batch
// has no remembered stream pair. The owner picks streams (video
// quality + dub), never «one studio for everything», and the picked
// quality rides every task. A remembered pair keeps the zero-prompt
// rotation. An empty live resolve falls back to the #164 listing
// union menu so a dead mirror never walls the batch.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// pr174PickerSession builds a two-provider session whose episodes
// carry two dubs at different qualities. qualityFor optionally
// remaps the resolved qualities (nil = the default map).
func pr174PickerSession(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
					{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
				},
				"anilib": {
					{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"w1b"}}},
					{Num: "2", RawID: "b2", RawEmbeds: map[string][]string{"AniLib": {"w2b"}}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"720": {URL: "v-go-720"}, "1080": {URL: "v-go-1080"},
				}},
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v-lib-1080"},
				}},
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

// pr174DriveToContent walks «⬇ Скачать серии» → range 1-2 → the
// content-mode pick (видео+аудио), returning the screen after the
// content pick (whatever state that produced) plus the pending cmd.
func pr174DriveToContent(t *testing.T, s *sessionScreen) (*sessionScreen, tea.Cmd) {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, cmd := ss.Update(enter()) // видео+аудио, the default first row
	return next.(*sessionScreen), cmd
}

// pr174FeedSettle runs one async settle command and feeds its msg
// back into the screen (the bubbletea settle convention).
func pr174FeedSettle(t *testing.T, ss *sessionScreen, cmd tea.Cmd) *sessionScreen {
	t.Helper()
	if cmd == nil {
		t.Fatal("the content pick must start the stream resolve command")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("the resolve command must settle with a msg")
	}
	next, _ := ss.Update(msg)
	return next.(*sessionScreen)
}

// TestDownloadOffersStreamPicker (PR174 S1/S2): a fresh session
// without a remembered pair opens the merged stream picker between
// the content pick and the mode menu — the SAME quality·dub·provider
// rows the watch flow offers — and the pick records both the dub and
// the quality before the mode menu opens.
func TestDownloadOffersStreamPicker(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174PickerSession(t)
	if s.videoDub != "" {
		t.Fatalf("precondition: no remembered dub, got %q", s.videoDub)
	}
	ss, cmd := pr174DriveToContent(t, s)
	if ss.state != sessionStateDownloadStream {
		t.Fatalf("after the content pick the stream picker must open (loading), got %v", ss.state)
	}
	ss = pr174FeedSettle(t, ss, cmd)
	if ss.state != sessionStateDownloadStream {
		t.Fatalf("the picker must survive its settle, got %v", ss.state)
	}
	v := ss.View().Content
	if !strings.Contains(v, "Pick a stream (quality · dub · provider):") {
		t.Fatalf("the picker title must render:\n%s", v)
	}
	for _, want := range []string{"1080p", "720p", "AniLib [anilib]", "Дубль 1 [animego]"} {
		if !strings.Contains(v, want) {
			t.Fatalf("the picker rows must carry %q:\n%s", want, v)
		}
	}

	// Jump to the 1080p AniLib row and pick: dub + quality recorded,
	// the mode menu follows.
	picked := false
	for i, e := range ss.streamEntries {
		if e.Quality == "1080" && e.DubKey == "[anilib] AniLib" {
			ss.qualityList.Jump(i)
			picked = true
			break
		}
	}
	if !picked {
		t.Fatalf("no 1080p AniLib row in %+v", ss.streamEntries)
	}
	next, _ := ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the stream pick must open the mode menu, got %v", ss.state)
	}
	if ss.videoDub != "[anilib] AniLib" || ss.lastQuality != "1080" {
		t.Fatalf("pick must record dub+quality, got %q / %q", ss.videoDub, ss.lastQuality)
	}
}

// TestDownloadPickerQualityRidesTasks (PR174 S2): the picked row's
// quality rides every task of the batch — the download no longer
// falls to a silent best-quality guess.
func TestDownloadPickerQualityRidesTasks(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174PickerSession(t)
	ss, cmd := pr174DriveToContent(t, s)
	ss = pr174FeedSettle(t, ss, cmd)
	for i, e := range ss.streamEntries {
		if e.Quality == "720" && e.DubKey == "[animego] Дубль 1" {
			ss.qualityList.Jump(i)
			break
		}
	}
	next, _ := ss.Update(enter())
	ss = next.(*sessionScreen)
	// The direct mode row pushes the download screen; drive its batch
	// to the settle (the fake records the downloads).
	_, modeCmd := ss.Update(enter())
	if modeCmd == nil {
		t.Fatal("the mode pick must push the download screen")
	}
	pushedMsg := modeCmd()
	pushed, ok := pushedMsg.(pushMsg)
	if !ok {
		t.Fatalf("the mode pick must push the screen, got %T", pushedMsg)
	}
	ds, ok := pushed.screen.(*directDownloadScreen)
	if !ok {
		t.Fatalf("the pushed screen must be the direct screen, got %T", pushed.screen)
	}
	driveDirectScreen(t, ds)
	dl := ss.deps.Download.(*fakeDownload)
	joined := len(dl.downloads) + len(dl.submitted)
	if joined == 0 {
		t.Fatal("the batch must dispatch after the mode pick")
	}
	for _, task := range dl.downloads {
		if task.Quality != "720" {
			t.Fatalf("task %s must carry the picked quality, got %q", task.EpisodeNum, task.Quality)
		}
	}
	for _, task := range dl.submitted {
		if task.Quality != "720" {
			t.Fatalf("submitted task %s must carry the picked quality, got %q", task.EpisodeNum, task.Quality)
		}
	}
}

// TestDownloadPickerMarksAudioSet: SPACE in the download picker marks
// the row's dub into the download audio set (the PR165 marking,
// available in the download flow too — it feeds the mixing mux and
// the sync ask).
func TestDownloadPickerMarksAudioSet(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174PickerSession(t)
	ss, cmd := pr174DriveToContent(t, s)
	ss = pr174FeedSettle(t, ss, cmd)
	for i, e := range ss.streamEntries {
		if e.DubKey == "[anilib] AniLib" {
			ss.qualityList.Jump(i)
			break
		}
	}
	next, _ := ss.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	ss = next.(*sessionScreen)
	if !containsDub(ss.markedAudio, "[anilib] AniLib") {
		t.Fatalf("SPACE must mark the dub into the audio set, got %v", ss.markedAudio)
	}
	v := ss.View().Content
	// The FIRST marked dub carries the default disposition — its
	// marker is ★ (the PR165 invariant); a second marked dub would
	// paint ●.
	if !strings.Contains(v, "★") {
		t.Fatalf("the marked row must paint its default marker:\n%s", v)
	}
}

// TestDownloadRememberedDubSkipsPicker: a remembered pair keeps the
// zero-prompt rotation — the content pick goes straight to the mode
// menu, no picker.
func TestDownloadRememberedDubSkipsPicker(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174PickerSession(t)
	s.videoDub = "[animego] Дубль 1"
	s.lastQuality = "720"
	ss, cmd := pr174DriveToContent(t, s)
	if cmd != nil {
		t.Fatalf("a remembered pair must not start a resolve, got cmd")
	}
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the remembered pair must go straight to the mode menu, got %v", ss.state)
	}
}

// TestDownloadPickerEmptyResolveFallsBackToUnionMenu: when the live
// resolve of the representative episode yields nothing (dead
// mirrors), the #164 listing union menu opens — the batch is never
// walled, the listing-level dubs remain pickable.
func TestDownloadPickerEmptyResolveFallsBackToUnionMenu(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174PickerSession(t)
	fe := s.deps.Episode.(*fakeEpisode)
	fe.streams = nil
	fe.streamErr = map[string]error{
		"[animego] Дубль 1": contracts.ErrExtractFailed,
		"[anilib] AniLib":   contracts.ErrExtractFailed,
	}
	ss, cmd := pr174DriveToContent(t, s)
	ss = pr174FeedSettle(t, ss, cmd)
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("an empty resolve must fall back to the union dub menu, got %v", ss.state)
	}
	v := ss.View().Content
	if !strings.Contains(v, "Dub to download:") || !strings.Contains(v, "AniLib") {
		t.Fatalf("the union menu must render the listing dubs:\n%s", v)
	}
}
