package tui

// PR174 fix-round pins: the single-flight resolve-window guard, the
// renderBar clamp, the panic-hardened direct screen, the
// resolved-rows-wait-their-turn rendering, and the honest sync note
// of an all-same-provider mixing batch.

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// blockingEpisode scripts the resolve window: the first ResolveStream
// call signals `entered` and every call blocks until `release` closes
// (the fake resolve that blocks on a channel — the window
// queueBackgroundDownloads runs before the manager Submit lights the
// banner).
type blockingEpisode struct {
	fakeEpisode
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingEpisode) ResolveStream(ctx context.Context, providerID string, ep contracts.Episode, dubID string) (contracts.MediaStream, error) {
	f.once.Do(func() { close(f.entered) })
	<-f.release
	return f.fakeEpisode.ResolveStream(ctx, providerID, ep, dubID)
}

// blockingSession builds the pr174DirectSession shape over the
// blocking episode fake (a remembered dub — the mode menu follows the
// content pick directly).
func blockingSession(t *testing.T, dl DownloadService, eps *blockingEpisode) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode:      eps,
		Download:     dl,
		DownloadSync: "no",
		Log:          testLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animego] Дубль 1"
	s.lastQuality = "720"
	return s
}

// walkToBackgroundPick walks range 1-2 → content default → the
// background row of the mode menu, returning the dispatch command
// (the safeCmd that runs queueBackgroundDownloads).
func walkToBackgroundPick(t *testing.T, s *sessionScreen) tea.Cmd {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(enter()) // видео+аудио (remembered pair → mode menu)
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the content pick must open the mode menu, got %v", ss.state)
	}
	ss.modeList.Jump(1) // the background row
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatal("the background pick must dispatch the resolution command")
	}
	return cmd
}

// TestSingleFlightRejectsDuringBackgroundResolve (fix-round HIGH-1):
// the background dispatch resolves dubs for seconds-to-tens-of-
// seconds BEFORE the manager Submit lights the active banner — in
// that window a second request must be rejected at BOTH guards (the
// action-menu entry and the mode dispatch): exactly one batch ever
// resolves and submits. The marker clears when the queue settles.
func TestSingleFlightRejectsDuringBackgroundResolve(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &fakeDownload{}
	eps := &blockingEpisode{
		fakeEpisode: fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
					{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v1"}}},
			},
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	s := blockingSession(t, dl, eps)

	// The first dispatch: the resolve runs asynchronously and blocks
	// inside ResolveStream (the resolve window is deterministically
	// open from here).
	cmd := walkToBackgroundPick(t, s)
	settled := make(chan tea.Msg, 1)
	go func() { settled <- cmd() }()
	select {
	case <-eps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the resolve goroutine must reach the blocking probe")
	}

	// The second request DURING the resolve window: rejected at the
	// entry guard — the session stays on the menu with the busy
	// status and nothing dispatches.
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("a second request during the resolve window must be rejected, got state %v", ss.state)
	}
	if !strings.Contains(ss.status, "already running") {
		t.Fatalf("the rejection must carry the busy status, got %q", ss.status)
	}
	if len(dl.submitted) != 0 {
		t.Fatalf("nothing may be submitted while the resolve runs, got %d", len(dl.submitted))
	}

	// Release the resolve: the queue settles, the marker clears.
	close(eps.release)
	msg := <-settled
	queued, ok := msg.(backgroundQueuedMsg)
	if !ok {
		t.Fatalf("the background dispatch must settle into backgroundQueuedMsg, got %T", msg)
	}
	if queued.queued != 2 {
		t.Fatalf("exactly one batch of 2 episodes may queue, got %d", queued.queued)
	}
	next, _ = ss.Update(queued)
	if len(dl.submitted) != 2 {
		t.Fatalf("exactly one batch of 2 episodes may submit, got %d", len(dl.submitted))
	}

	// The busy marker cleared with the settle: the flow re-opens.
	s = next.(*sessionScreen)
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ = s.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("after the queue settles the download flow must re-open, got %v", ss.state)
	}
}

// TestRenderBarClampsOutOfRangeFraction (fix-round MED-1): a negative
// or over-1 fraction must clamp to the bar, never panic the TUI
// (strings.Repeat with a negative count crashes the whole process).
func TestRenderBarClampsOutOfRangeFraction(t *testing.T) {
	bar := renderBar(directRow{pct: -0.5, hasPct: true})
	if !strings.HasSuffix(bar, "  0%") || strings.Contains(bar, "-") {
		t.Fatalf("a negative fraction must clamp to 0%%, got %q", bar)
	}
	bar = renderBar(directRow{pct: 1.9, hasPct: true})
	if !strings.HasSuffix(bar, "100%") {
		t.Fatalf("an over-1 fraction must clamp to 100%%, got %q", bar)
	}
	// A NaN sample (a corrupted progress line) renders degenerate
	// input without a panic too.
	_ = renderBar(directRow{pct: math.NaN(), hasPct: true})
}

// panickingDownload aborts the first episode's download with a panic
// — the batch goroutine's safeCmd converts it into an errMsg.
type panickingDownload struct {
	fakeDownload
	calls int
}

func (f *panickingDownload) DownloadDetailed(_ context.Context, _ DownloadTask, _ func(DownloadProgress)) (string, []string, []SyncNote, error) {
	f.calls++
	panic("boom: synthetic batch failure")
}

// TestDirectScreenBatchPanicSettlesScreen (fix-round MED-2): a panic
// inside the batch goroutine must not strand the locked screen — the
// errMsg settles the DIRECT screen itself (batch context cancelled,
// honest error status, any key returns to browsing) instead of
// parking a recovery screen on top of a frozen dashboard with an
// orphaned ffmpeg underneath.
func TestDirectScreenBatchPanicSettlesScreen(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &panickingDownload{}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)
	app := NewApp(s, s.deps, testLogger())
	model, initCmd := app.Update(pushMsg{screen: ds})
	a := model.(App)

	msg := initCmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init must arm the batch, got %T", msg)
	}
	res := make(chan tea.Msg, 1)
	go func() { res <- batch[0]() }()
	em := <-res
	emsg, ok := em.(errMsg)
	if !ok || emsg.screen != downloadScreenID {
		t.Fatalf("the panicking batch must surface errMsg of the direct screen, got %T %+v", em, em)
	}

	cancelObserved := make(chan struct{})
	context.AfterFunc(ds.ctx, func() { close(cancelObserved) })

	// The App routes the direct screen's own errMsg to the screen —
	// it settles itself, no error screen stacks on the locked
	// surface.
	next, _ := a.Update(emsg)
	a = next.(App)
	if top := a.stack[len(a.stack)-1]; top.ID() != downloadScreenID {
		t.Fatalf("the direct screen must settle itself, got %s on top", top.ID())
	}
	if !ds.done {
		t.Fatal("the panicked batch must settle the screen")
	}
	select {
	case <-cancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("the batch context must be cancelled (the orphaned ffmpeg dies)")
	}
	if dl.calls != 1 {
		t.Fatalf("the panic must abort the batch at the first episode, got %d calls", dl.calls)
	}
	v := ds.View().Content
	if !strings.Contains(v, "internal error occurred") {
		t.Fatalf("the honest error status must render:\n%s", v)
	}
	if !strings.Contains(v, "Download stopped") {
		t.Fatalf("the interrupted rows must render the stopped verdict:\n%s", v)
	}
	// After the settle any key returns to browsing.
	_, cmd := ds.Update(enter())
	if cmd == nil {
		t.Fatal("a key on the settled screen must pop back to the session")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("the settled screen must pop, got %T", cmd())
	}
}

// TestDirectScreenResolvedRowsWaitForTheirTurn (fix-round MED-3): a
// resolved episode renders as PENDING until its FIRST progress tick
// — a 16-episode batch must not look like 16 simultaneous downloads.
func TestDirectScreenResolvedRowsWaitForTheirTurn(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	ds := newDirectDownloadScreen(&Deps{}, directBatch{
		tasks:     []DownloadTask{{EpisodeNum: "1"}, {EpisodeNum: "2"}},
		preferred: "[animego] Дубль 1",
		episodes:  []string{"1", "2"},
	})
	ds.Update(directResolveMsg{done: 1, total: 2, idx: 0, episode: "1", verdict: "[animego] Дубль 1"})
	v := ds.View().Content
	if strings.Contains(v, "▸ episode 1") {
		t.Fatalf("a resolved row must not render as actively downloading before its turn:\n%s", v)
	}
	if !strings.Contains(v, "… episode 1") {
		t.Fatalf("the resolved row must render the pending shape:\n%s", v)
	}
	// The first tick promotes the row to active.
	ds.Update(directTickMsg{idx: 0, pct: 0.1, hasPct: true, timeSec: 6})
	v = ds.View().Content
	if !strings.Contains(v, "▸ episode 1") {
		t.Fatalf("the first progress tick must promote the row to active:\n%s", v)
	}
	// A settled row never regresses: a late tick keeps it done.
	ds.Update(directRowMsg{idx: 1, row: directRow{episode: "2", state: directRowDone}})
	ds.Update(directTickMsg{idx: 1, pct: 0.9, hasPct: true, timeSec: 54})
	if ds.rows[1].state != directRowDone {
		t.Fatalf("a late tick must not un-settle a done row, got %v", ds.rows[1].state)
	}
}

// TestSyncAllSameProvidersEmitsHonestNote (fix-round MED-4): a sync-ON
// mixing batch whose every donor is the video provider's own track
// analyzes NOTHING (the v2 same-provider skip) — the user answered
// «yes» and must see the honest «nothing was performed» note, not a
// silent no-op.
func TestSyncAllSameProvidersEmitsHonestNote(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)
	task := DownloadTask{
		ProviderID: "anilib",
		DubID:      "[anilib] DUB1",
		Mode:       "video_audio",
		AudioSync:  true,
	}
	donors := []syncDonor{
		{key: "[anilib] DUB1", dub: "DUB1", lang: "rus", src: contracts.VideoSource{URL: "https://cdn/d1.m3u8"}},
		{key: "[anilib] DUB2", dub: "DUB2", lang: "rus", src: contracts.VideoSource{URL: "https://cdn/d2.m3u8"}},
	}
	_, notes, err, handled := c.trySyncDownload(context.Background(), task,
		contracts.VideoSource{URL: "https://cdn/video.m3u8"}, donors,
		"", t.TempDir(), "720", "title", nil)
	if err != nil || handled {
		t.Fatalf("the all-same-provider batch degrades to the single pass, got err=%v handled=%v", err, handled)
	}
	if len(analyzer.analyzed) != 0 {
		t.Fatalf("same-provider donors must not be analyzed, got %v", analyzer.analyzed)
	}
	if len(notes) != 1 || notes[0].Kind != SyncNoExternal {
		t.Fatalf("the batch must emit exactly one honest no-external note, got %+v", notes)
	}
	if s := renderSyncSuffix(notes); !strings.Contains(s, "no external tracks") {
		t.Fatalf("the note must render on the report row, got %q", s)
	}
}
