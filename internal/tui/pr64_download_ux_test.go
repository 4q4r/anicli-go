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
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
// that produced the owner's screenshot («Playback finished»
// rendered under the download prompt).
func seedPlaybackVerdict(s *sessionScreen) *sessionScreen {
	next, _ := s.Update(playedMsg{})
	return next.(*sessionScreen)
}

// TestDownloadDialogHidesStalePlaybackStatus (PR64 #1): after a
// finished playback, the download-range prompt must render WITHOUT the
// stale «Playback finished» line.
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
	if !contains(v, "to download") {
		t.Fatalf("the range prompt must render:\n%s", v)
	}
	if contains(v, "Playback finished") {
		t.Fatalf("the playback verdict must not leak into the download dialog:\n%s", v)
	}
}

// TestStatusNeverSurvivesSessionTransition (PR64 #1 sweep): the
// playback verdict renders on the menu surface ONLY — every other
// substate the user can enter next starts clean.
func TestStatusNeverSurvivesSessionTransition(t *testing.T) {
	const stale = "Playback finished"
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
			next, _ := ss.Update(enter()) // cursor on «Status»
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
	const stale = "Playback finished"
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
		{"contiguous", []string{"1", "2", "3"}, "Episodes available: 3 (1–3)"},
		{"gaps", []string{"1", "2", "3", "7", "10"}, "Episodes available: 5 (1–3, 7, 10)"},
		{"single", []string{"5"}, "Episodes available: 1 (5)"},
		{"junk-label", []string{"1", "OVA"}, "Episodes available: 2 (1, OVA)"},
		{"capped", []string{"1", "3", "5", "7", "9", "11", "13", "15", "17", "19", "21", "23"},
			"Episodes available: 12 (1, 3, 5, 7, 9, 11, 13, 15, … +4 more)"},
		{"empty", nil, "No episodes available"},
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
	if !contains(v, "Episodes available: 3 (1–3)") {
		t.Fatalf("the prompt must show the available episodes:\n%s", v)
	}
}

// --- PR64 #3: range downloads resolve the dub PER EPISODE ---

// rotatingEpisode models the Anitaku-style per-episode mirror
// rotation: the dub is listed on the episode, but its stream resolve
// fails for specific (episode, dub) pairs.
type rotatingEpisode struct {
	fakeEpisode
	dead map[string]bool // keyed "ep|dub"
}

func (r *rotatingEpisode) ResolveStream(ctx context.Context, prov string, ep contracts.Episode, dub string) (contracts.MediaStream, error) {
	if r.dead[ep.Num+"|"+dub] {
		return contracts.MediaStream{}, errors.New("mirror dead")
	}
	return r.fakeEpisode.ResolveStream(ctx, prov, ep, dub)
}

// pr64RotatingSession builds a 3-episode session where every episode
// carries BOTH dubs («[animego] Дубль 1» resolvable, «[anilib]
// AniLib» resolvable — one dub per source, the merged shape) and the
// animego dub is remembered from a previous watch — the download's
// preferred dub.
func pr64RotatingSession(t *testing.T, dead map[string]bool) (*sessionScreen, *fakeDownload) {
	t.Helper()
	eps := map[string][]contracts.Episode{
		"animego": {
			{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
			{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
			{Num: "3", RawID: "a3", RawEmbeds: map[string][]string{"Дубль 1": {"u3v"}}},
		},
		"anilib": {
			{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"w1b"}}},
			{Num: "2", RawID: "b2", RawEmbeds: map[string][]string{"AniLib": {"w2b"}}},
			{Num: "3", RawID: "b3", RawEmbeds: map[string][]string{"AniLib": {"w3b"}}},
		},
	}
	deps := &Deps{
		Episode: &rotatingEpisode{
			fakeEpisode: fakeEpisode{
				episodes: eps,
				streams: map[string]contracts.MediaStream{
					"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
					"[anilib] AniLib":   {Links: map[string]contracts.VideoSource{"1080": {URL: "v-lib"}}},
				},
			},
			dead: dead,
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
	s.videoDub = "[animego] Дубль 1"
	return s, deps.Download.(*fakeDownload)
}

// driveDownloadRange types the range and picks the mode; for
// «Передний план» it settles the returned command and applies the
// settle message, returning it with the post-settle status.
func driveDownloadRange(t *testing.T, s *sessionScreen, rng, mode string) (tea.Msg, string) {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	ss.rangeInput.typeText(rng)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("after the range the mode menu opens, got %v", ss.state)
	}
	if mode == "background" {
		ss.modeList.Jump(1)
	}
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("the %s pick must dispatch a command", mode)
	}
	switch mode {
	case "foreground":
		// The pick arms a BATCH (the worker plus the progress pump) —
		// flatten it and keep the settle.
		var settled downloadSettledMsg
		for _, m := range runLaunchBatch(t, cmd) {
			if d, ok := m.(downloadSettledMsg); ok {
				settled = d
			}
		}
		applied, _ := ss.Update(settled)
		return settled, applied.(*sessionScreen).status
	case "background":
		msg := cmd()
		queued, ok := msg.(backgroundQueuedMsg)
		if !ok {
			t.Fatalf("background must settle into backgroundQueuedMsg, got %T", msg)
		}
		applied, _ := ss.Update(queued)
		return queued, applied.(*sessionScreen).status
	}
	t.Fatalf("unknown mode %q", mode)
	return nil, ""
}

// TestDownloadRangeResolvesDubPerEpisode (PR64 #3): the remembered
// dub serves where it is alive; a dead mirror on ONE episode falls
// back to the other dub FOR THAT EPISODE only; the report types the
// dub used per episode.
func TestDownloadRangeResolvesDubPerEpisode(t *testing.T) {
	s, dl := pr64RotatingSession(t, map[string]bool{"2|[animego] Дубль 1": true})

	msg, status := driveDownloadRange(t, s, "1-3", "foreground")
	settled := msg.(downloadSettledMsg)

	if settled.err != nil {
		t.Fatalf("every episode must resolve, got %v", settled.err)
	}
	if settled.count != 3 || settled.total != 3 {
		t.Fatalf("3 of 3 episodes must download, got %d/%d", settled.count, settled.total)
	}
	want := map[string]string{
		"1": "[animego] Дубль 1",
		"2": "[anilib] AniLib", // the per-episode fallback
		"3": "[animego] Дубль 1",
	}
	if len(dl.downloads) != 3 {
		t.Fatalf("3 tasks expected, got %d", len(dl.downloads))
	}
	for _, task := range dl.downloads {
		if task.DubID != want[task.EpisodeNum] {
			t.Fatalf("ep %s must use %q, got %q", task.EpisodeNum, want[task.EpisodeNum], task.DubID)
		}
		if task.ProviderID != providerOfTrackKey(task.DubID) {
			t.Fatalf("ep %s provider must match its dub, got %q for %q",
				task.EpisodeNum, task.ProviderID, task.DubID)
		}
	}
	// The report types the dub per episode.
	if !contains(status, "Ep. 2 — [anilib] AniLib") {
		t.Fatalf("the report must type the fallback dub for ep 2:\n%s", status)
	}
	if !contains(status, "Ep. 1 — [animego] Дубль 1") {
		t.Fatalf("the report must type the remembered dub for ep 1:\n%s", status)
	}
}

// TestDownloadRangeTypesNoViableEpisode (PR64 #3): an episode with NO
// viable dub is a TYPED per-episode failure — not a silent skip and
// not a whole-range abort.
func TestDownloadRangeTypesNoViableEpisode(t *testing.T) {
	s, dl := pr64RotatingSession(t, map[string]bool{
		"2|[animego] Дубль 1": true,
		"2|[anilib] AniLib":   true,
	})

	msg, status := driveDownloadRange(t, s, "1-3", "foreground")
	settled := msg.(downloadSettledMsg)

	if settled.count != 2 || settled.total != 3 {
		t.Fatalf("2 of 3 must download, got %d/%d", settled.count, settled.total)
	}
	if len(dl.downloads) != 2 {
		t.Fatalf("the range must NOT abort: 2 tasks expected, got %d", len(dl.downloads))
	}
	typed := false
	for _, r := range settled.report {
		if r.Episode == "2" && errors.Is(r.Err, errNoViableDub) {
			typed = true
		}
	}
	if !typed {
		t.Fatalf("ep 2's failure must be typed in the report, got %+v", settled.report)
	}
	if !contains(status, "Ep. 2") || !contains(status, "no viable dubs") {
		t.Fatalf("the status must type ep 2's failure:\n%s", status)
	}
	if !contains(status, "2 of 3") {
		t.Fatalf("the headline must carry the partial verdict:\n%s", status)
	}
}

// TestDownloadSettleReportRendersPerEpisode (PR64 #3): the compact
// report line shape — ep → dub → path / failure — plus the headline
// semantics the older tests pin («Episodes downloaded», «Download failed»).
func TestDownloadSettleReportRendersPerEpisode(t *testing.T) {
	s := pr64Session(t)
	msg := downloadSettledMsg{count: 1, total: 2, err: errors.New("ffmpeg exploded"),
		report: []downloadEpisodeReport{
			{Episode: "1", Dub: "[animego] Дубль 1", Path: "/dl/Тайтл/EP_1_Дубль_1_720p.mp4"},
			{Episode: "2", Dub: "[anilib] AniLib", Err: errors.New("ffmpeg exploded")},
		}}
	next, _ := s.Update(msg)
	status := next.(*sessionScreen).status
	for _, want := range []string{
		"Episodes downloaded: 1 of 2",
		"Ep. 1 — [animego] Дубль 1 — /dl/Тайтл/EP_1_Дубль_1_720p.mp4",
		"Ep. 2 — [anilib] AniLib — error: ffmpeg exploded",
	} {
		if !contains(status, want) {
			t.Fatalf("report line %q missing:\n%s", want, status)
		}
	}

	msg2 := downloadSettledMsg{count: 0, total: 1, err: errNoViableDub,
		report: []downloadEpisodeReport{{Episode: "7", Err: errNoViableDub}}}
	next, _ = s.Update(msg2)
	status = next.(*sessionScreen).status
	if !contains(status, "Download failed: 0 of 1 episodes") {
		t.Fatalf("an all-failed range keeps the «Download failed» headline:\n%s", status)
	}
	if !contains(status, "Ep. 7 — ✗ no viable dubs") {
		t.Fatalf("the no-dub verdict must be typed:\n%s", status)
	}

	// Review fix 6: the headline carries the first error verbatim
	// (the pre-PR64 «Error загрузки: …» contract); the typed
	// no-dub verdict stays on its per-episode line only.
	msg3 := downloadSettledMsg{count: 0, total: 2, err: errors.New("disk full"),
		report: []downloadEpisodeReport{
			{Episode: "1", Dub: "[animego] Дубль 1", Err: errors.New("disk full")},
			{Episode: "2", Err: errNoViableDub},
		}}
	next, _ = s.Update(msg3)
	status = next.(*sessionScreen).status
	if !contains(status, "Download failed: 0 of 2 episodes — disk full") {
		t.Fatalf("the headline must carry the first error verbatim:\n%s", status)
	}
}

// TestDownloadBackgroundResolvesDubPerEpisode (PR64 #3): the
// background mode resolves the dub per episode BEFORE queueing — the
// submitted tasks never carry a dead dub, and the settle types the
// per-episode verdicts.
func TestDownloadBackgroundResolvesDubPerEpisode(t *testing.T) {
	s, dl := pr64RotatingSession(t, map[string]bool{"2|[animego] Дубль 1": true})

	msg, status := driveDownloadRange(t, s, "1-2", "background")
	queued := msg.(backgroundQueuedMsg)

	if queued.queued != 2 || queued.total != 2 {
		t.Fatalf("both episodes must queue, got %d/%d", queued.queued, queued.total)
	}
	if len(dl.submitted) != 2 {
		t.Fatalf("2 submitted tasks expected, got %d", len(dl.submitted))
	}
	want := map[string]string{"1": "[animego] Дубль 1", "2": "[anilib] AniLib"}
	for _, task := range dl.submitted {
		if task.DubID != want[task.EpisodeNum] {
			t.Fatalf("ep %s must queue with %q, got %q",
				task.EpisodeNum, want[task.EpisodeNum], task.DubID)
		}
	}
	if !contains(status, "Queued to background: 2 episodes") {
		t.Fatalf("the queued headline expected:\n%s", status)
	}
	if !contains(status, "Ep. 2 — [anilib] AniLib") {
		t.Fatalf("the background report must type the fallback dub:\n%s", status)
	}
}

// TestDownloadRangeHydratesLazyEpisodes (PR64 #3): range episodes of
// a lazily-listing provider carry NO embeds until hydrated — the
// download resolution runs the same on-demand hydration round as the
// watch flow, or every range download would fail outright.
func TestDownloadRangeHydratesLazyEpisodes(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{
		"animego": {"Дубль 1": {"u1v", "u2v", "u3v"}},
	}}
	eps := map[string][]contracts.Episode{
		"animego": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
			{Num: "2", RawID: "17167", RawEmbeds: map[string][]string{}},
			{Num: "3", RawID: "17168", RawEmbeds: map[string][]string{}},
		},
	}
	deps := &Deps{
		Episode: &hydratingEpisode{
			fakeEpisode: fakeEpisode{
				episodes: eps,
				streams: map[string]contracts.MediaStream{
					"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				},
			},
			fix: fix,
		},
		Download: &fakeDownload{},
		Log:      testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animego] Дубль 1"

	dl := deps.Download.(*fakeDownload)
	msg, _ := driveDownloadRange(t, s, "1-3", "foreground")
	settled := msg.(downloadSettledMsg)

	if settled.count != 3 || len(dl.downloads) != 3 {
		t.Fatalf("hydration must make all 3 episodes downloadable, got %d/%d",
			settled.count, len(dl.downloads))
	}
	if len(fix.calls) == 0 {
		t.Fatal("the lazy providers must have been hydrated")
	}
}

// --- PR64 review fix 1: per-phase probe budgets ---

// slowEpisode delays hydration and per-dub stream probes (ctx-aware),
// modelling the latency that used to starve the fallback probes when
// one shared budget covered the whole resolution.
type slowEpisode struct {
	fakeEpisode
	hydrateDelay time.Duration
	probeDelay   func(dub string) time.Duration
}

func (s *slowEpisode) HydrateDubs(ctx context.Context, providerID string, episode contracts.Episode) (contracts.Episode, error) {
	if s.hydrateDelay > 0 {
		select {
		case <-time.After(s.hydrateDelay):
		case <-ctx.Done():
			return episode, ctx.Err()
		}
	}
	return episode, nil
}

func (s *slowEpisode) ResolveStream(ctx context.Context, prov string, ep contracts.Episode, dub string) (contracts.MediaStream, error) {
	if s.probeDelay != nil {
		if d := s.probeDelay(dub); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return contracts.MediaStream{}, ctx.Err()
			}
		}
	}
	return s.fakeEpisode.ResolveStream(ctx, prov, ep, dub)
}

// TestDownloadDubBudgetsArePerPhase (review fix 1): hydration and
// every candidate probe carry their OWN budget — a slow-to-fail
// preferred-dub probe must not starve the remaining candidates into a
// false «нет доступных озвучек» (the Anitaku-rotation scenario under
// latency).
func TestDownloadDubBudgetsArePerPhase(t *testing.T) {
	oldHydrate, oldProbe := downloadHydrateBudget, downloadProbeBudget
	downloadHydrateBudget = 100 * time.Millisecond
	downloadProbeBudget = 60 * time.Millisecond
	defer func() { downloadHydrateBudget, downloadProbeBudget = oldHydrate, oldProbe }()

	deps := &Deps{
		Episode: &slowEpisode{
			fakeEpisode: fakeEpisode{
				episodes: map[string][]contracts.Episode{
					"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{
						"Дубль 1": {"u1v"},
						"AniLib":  {"w1v"},
					}}},
				},
				streams: map[string]contracts.MediaStream{
					"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
					"[animego] AniLib":  {Links: map[string]contracts.VideoSource{"720": {URL: "v-alt"}}},
				},
			},
			hydrateDelay: 40 * time.Millisecond, // fits the hydration budget
			probeDelay: func(dub string) time.Duration {
				if dub == "[animego] Дубль 1" {
					return 90 * time.Millisecond // blows the probe's OWN budget
				}
				return 0 // the fallback probe is instant — must still run
			},
		},
		Log: testLogger(),
	}
	ep := contracts.Episode{Num: "1", RawID: "animego:a1", RawEmbeds: map[string][]string{
		"[animego] Дубль 1": {"u1v"},
		"[animego] AniLib":  {"w1v"},
	}}

	dub := resolveDownloadDub(context.Background(), deps, ep, "[animego] Дубль 1")
	if dub != "[animego] AniLib" {
		t.Fatalf("the fallback candidate must still be probed after a slow first probe, got %q", dub)
	}
}

// --- PR64 review fix 2: bounded-parallel resolution, ordered report,
// per-episode progress ---

// staggeredEpisode delays stream probes PER EPISODE (ctx-aware) so
// the resolution completion order can be forced out of episode order.
type staggeredEpisode struct {
	fakeEpisode
	delay func(episodeNum string) time.Duration
}

func (s *staggeredEpisode) ResolveStream(ctx context.Context, prov string, ep contracts.Episode, dub string) (contracts.MediaStream, error) {
	if s.delay != nil {
		if d := s.delay(ep.Num); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return contracts.MediaStream{}, ctx.Err()
			}
		}
	}
	return s.fakeEpisode.ResolveStream(ctx, prov, ep, dub)
}

// TestDownloadRangeReportOrdered (review fix 2): resolution completes
// out of order (ep 1 slowed, ep 2 instant) — the report lines and the
// download sequence stay in EPISODE order regardless.
func TestDownloadRangeReportOrdered(t *testing.T) {
	s, dl := pr64RotatingSession(t, nil)
	s.deps.Episode = &staggeredEpisode{
		fakeEpisode: fakeEpisode{
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
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v-go"}}},
				"[anilib] AniLib":   {Links: map[string]contracts.VideoSource{"1080": {URL: "v-lib"}}},
			},
		},
		delay: func(num string) time.Duration {
			if num == "1" {
				return 80 * time.Millisecond // ep 1 settles LAST
			}
			return 0
		},
	}

	msg, status := driveDownloadRange(t, s, "1-2", "foreground")
	settled := msg.(downloadSettledMsg)

	if settled.count != 2 || settled.err != nil {
		t.Fatalf("both episodes must download, got %d/%d err=%v", settled.count, settled.total, settled.err)
	}
	if len(dl.downloads) != 2 || dl.downloads[0].EpisodeNum != "1" || dl.downloads[1].EpisodeNum != "2" {
		t.Fatalf("downloads must run in episode order, got %+v", dl.downloads)
	}
	idx1, idx2 := strings.Index(status, "Ep. 1 —"), strings.Index(status, "Ep. 2 —")
	if idx1 == -1 || idx2 == -1 || idx1 > idx2 {
		t.Fatalf("the report must be in episode order:\n%s", status)
	}
}

// TestDownloadProgressLines (review fix 2): the batch ticks per
// episode — a resolve verdict per settled episode and the reviewer's
// exact download-start line «Загрузка i/N: эп X — [dub]…».
func TestDownloadProgressLines(t *testing.T) {
	s, _ := pr64RotatingSession(t, nil)
	preferred := "[animego] Дубль 1"
	tasks := []DownloadTask{
		{AnimeTitle: "Тайтл", EpisodeNum: "1", Episode: s.episodes["1"]},
		{AnimeTitle: "Тайтл", EpisodeNum: "2", Episode: s.episodes["2"]},
	}

	ch := make(chan string, 64)
	var settled downloadSettledMsg
	done := make(chan struct{})
	go func() {
		defer close(done)
		settled = runForegroundDownload(context.Background(), s.deps, tasks, preferred, ch)
		close(ch)
	}()
	var lines []string
	for line := range ch {
		lines = append(lines, line)
	}
	<-done

	if settled.count != 2 {
		t.Fatalf("both episodes must download, got %d", settled.count)
	}
	if len(lines) == 0 {
		t.Fatal("progress lines expected")
	}
	var start1, resolve1 bool
	for _, line := range lines {
		if strings.Contains(line, "Downloading 1/2: ep 1 — [animego] Дубль 1…") {
			start1 = true
		}
		if strings.HasPrefix(line, "Resolving dubs ") && strings.Contains(line, "ep 1 — [animego] Дубль 1") {
			resolve1 = true // the counter follows COMPLETION order (parallel)
		}
	}
	if !start1 {
		t.Fatalf("the download-start progress line missing:\n%s", strings.Join(lines, "\n"))
	}
	if !resolve1 {
		t.Fatalf("the resolve verdict line missing:\n%s", strings.Join(lines, "\n"))
	}
}

// TestDownloadProgressScopesToSurface (review fix 2 + #1 semantics):
// a progress line updates the status on the owning surface, the pump
// re-arms, and a stale generation (a newer batch) is dropped.
func TestDownloadProgressScopesToSurface(t *testing.T) {
	s := pr64Session(t)
	s.downloadProgCh = make(chan string, 8)

	next, cmd := s.Update(downloadProgressMsg{gen: s.downloadGen, line: "Загрузка 1/2: эп 1 — [d]…"})
	ss := next.(*sessionScreen)
	if ss.status != "Загрузка 1/2: эп 1 — [d]…" {
		t.Fatalf("progress must reach the status line, got %q", ss.status)
	}
	if cmd == nil {
		t.Fatal("the progress message must re-arm the pump")
	}
	ss.downloadProgCh <- "Загрузка 2/2: эп 2 — [d]…"
	m := cmd()
	pm, ok := m.(downloadProgressMsg)
	if !ok || pm.line != "Загрузка 2/2: эп 2 — [d]…" {
		t.Fatalf("the pump must deliver the next line, got %#v", m)
	}

	// A stale generation (a newer batch superseded this one) drops.
	next, cmd = ss.Update(downloadProgressMsg{gen: ss.downloadGen + 5, line: "устаревшее"})
	ss = next.(*sessionScreen)
	if cmd != nil || ss.status == "устаревшее" {
		t.Fatalf("a stale generation must be dropped, status %q", ss.status)
	}
}

// TestDownloadResolutionBounded (review fix 2): the per-episode
// resolution runs through the repo's bounded pool — at most 8
// concurrent probes, and actually concurrent (not sequential).
func TestDownloadResolutionBounded(t *testing.T) {
	const eps = 16
	tracking := &inflightEpisode{delay: 50 * time.Millisecond}
	tracking.fakeEpisode = fakeEpisode{
		episodes: map[string][]contracts.Episode{},
		streams: map[string]contracts.MediaStream{
			"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v"}}},
		},
	}
	epsSlice := make([]contracts.Episode, 0, eps)
	for i := 1; i <= eps; i++ {
		n := itoa(i)
		epsSlice = append(epsSlice, contracts.Episode{Num: n, RawID: "a" + n,
			RawEmbeds: map[string][]string{"Дубль 1": {"u" + n + "v"}}})
	}
	tracking.episodes = map[string][]contracts.Episode{"animego": epsSlice}
	deps := &Deps{Episode: tracking, Download: &fakeDownload{}, Log: testLogger()}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animego] Дубль 1"

	msg, _ := driveDownloadRange(t, s, "1-16", "foreground")
	settled := msg.(downloadSettledMsg)
	if settled.count != eps {
		t.Fatalf("all %d episodes must download, got %d", eps, settled.count)
	}
	if tracking.maxInFlight() > 8 {
		t.Fatalf("the resolution must be bounded at 8, peaked at %d", tracking.maxInFlight())
	}
	if tracking.maxInFlight() < 2 {
		t.Fatalf("the resolution must actually parallelize, peaked at %d", tracking.maxInFlight())
	}
}

// inflightEpisode counts concurrent stream probes (bounded-pool
// assertion) and delays each probe so the peak is observable.
type inflightEpisode struct {
	fakeEpisode
	delay time.Duration

	mu   sync.Mutex
	cur  int
	maxi int
}

func (f *inflightEpisode) maxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxi
}

func (f *inflightEpisode) ResolveStream(ctx context.Context, prov string, ep contracts.Episode, dub string) (contracts.MediaStream, error) {
	f.mu.Lock()
	f.cur++
	if f.cur > f.maxi {
		f.maxi = f.cur
	}
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.cur--
	f.mu.Unlock()
	return f.fakeEpisode.ResolveStream(ctx, prov, ep, dub)
}

// TestPlayingViewGatesStaleStatusBehindGen (review fix 4): the
// buffering/playing body renders must honor the surface generation —
// a status left over from an earlier surface must not leak through
// the body branch even when the fresh-status discipline is broken.
func TestPlayingViewGatesStaleStatusBehindGen(t *testing.T) {
	s := seedPlaybackVerdict(pr64Session(t))
	// A transition WITHOUT a fresh status: the old body branch would
	// render the stale menu verdict verbatim.
	s.setState(sessionStatePlaying)
	if contains(s.View().Content, "Playback finished") {
		t.Fatalf("the stale status leaked through the playing body render:\n%s", s.View().Content)
	}
	s.setState(sessionStateBuffering)
	if contains(s.View().Content, "Playback finished") {
		t.Fatalf("the stale status leaked through the buffering body render:\n%s", s.View().Content)
	}
	// And a fresh status on the CURRENT surface still renders.
	s.setStatus("▶ Launching mpv…")
	if !contains(s.View().Content, "▶ Launching mpv…") {
		t.Fatalf("a fresh status must render on the playing surface:\n%s", s.View().Content)
	}
}
