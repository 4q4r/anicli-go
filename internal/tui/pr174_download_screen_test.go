package tui

// PR174 S4/S5: the direct download screen pins — the per-episode
// progress dashboard, the locked navigation, the confirmed cancel,
// the single-flight guard (one active download globally), and the
// S4 core invariant: a batch of N episodes dispatches EXACTLY N
// downloads, never 2N.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// countingDownload ticks progress and counts its dispatches; it honors
// the context (the cancel proof) and records the tasks it saw.
type countingDownload struct {
	fakeDownload
	ticks      []DownloadProgress
	calls      []DownloadTask
	cancelled  bool
	blockOnCtx bool
	// entered signals the test the batch is INSIDE the blocking
	// download (the deterministic handoff before a cancel).
	entered chan struct{}
}

func (f *countingDownload) DownloadDetailed(ctx context.Context, task DownloadTask, progress func(DownloadProgress)) (string, []string, []SyncNote, error) {
	f.calls = append(f.calls, task)
	if f.blockOnCtx {
		if f.entered != nil {
			close(f.entered)
		}
		<-ctx.Done()
		f.cancelled = true
		return "", nil, nil, ctx.Err()
	}
	for _, p := range f.ticks {
		progress(p)
	}
	return "/dl/EP_" + task.EpisodeNum + ".mp4", nil, nil, nil
}

// pr174DirectSession builds a loaded two-provider session with a
// remembered dub (no picker: the mode menu follows the content pick
// directly) wired to the given download fake.
func pr174DirectSession(t *testing.T, dl DownloadService) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}},
					{Num: "2", RawID: "a2", RawEmbeds: map[string][]string{"Дубль 1": {"u2v"}}},
					{Num: "3", RawID: "a3", RawEmbeds: map[string][]string{"Дубль 1": {"u3v"}}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"720": {URL: "v1"}}},
			},
		},
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

// pr174PushDirectScreen walks range 1-3 → content default → the
// direct mode row, returning the pushed screen (via the push cmd).
func pr174PushDirectScreen(t *testing.T, s *sessionScreen) *directDownloadScreen {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-3")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(enter()) // видео+аудио (remembered pair → mode menu)
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("the content pick must open the mode menu, got %v", ss.state)
	}
	_, cmd := ss.Update(enter()) // the direct row
	if cmd == nil {
		t.Fatal("the direct pick must push the download screen")
	}
	msg := cmd()
	pushed, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("the direct pick must push the screen, got %T", msg)
	}
	ds, ok := pushed.screen.(*directDownloadScreen)
	if !ok {
		t.Fatalf("the pushed screen must be the direct download screen, got %T", pushed.screen)
	}
	return ds
}

// driveDirectScreen runs the screen's command loop to completion,
// feeding every settled msg back into Update (the bubbletea settle
// convention). Returns the settle message.
func driveDirectScreen(t *testing.T, ds *directDownloadScreen) directSettleMsg {
	t.Helper()
	cmds := []tea.Cmd{ds.Init()}
	var settle directSettleMsg
	seen := false
	for len(cmds) > 0 {
		c := cmds[0]
		cmds = cmds[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			cmds = append(cmds, batch...)
			continue
		}
		if s, ok := msg.(directSettleMsg); ok {
			settle, seen = s, true
		}
		_, cmd := ds.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if !seen {
		t.Fatal("the direct batch must settle")
	}
	return settle
}

// TestDownloadModesOfferDirectAndBackground: the mode menu offers the
// direct screen first and the background queue second (the owner's
// two entry points).
func TestDownloadModesOfferDirectAndBackground(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	s := pr174DirectSession(t, &fakeDownload{})
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("want the mode menu, got %v", ss.state)
	}
	v := ss.View().Content
	if !strings.Contains(v, "Download (progress screen)") || !strings.Contains(v, "Download in background") {
		t.Fatalf("both download modes must be offered:\n%s", v)
	}
}

// TestDirectScreenDispatchesEachEpisodeExactlyOnce (PR174 S4): a
// batch of N episodes produces exactly N download dispatches — the
// double-dispatch class (two concurrent batches racing one .part) is
// structurally impossible through the direct screen.
func TestDirectScreenDispatchesEachEpisodeExactlyOnce(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)
	settle := driveDirectScreen(t, ds)

	if len(dl.calls) != 3 {
		t.Fatalf("a 3-episode batch must dispatch exactly 3 downloads, got %d", len(dl.calls))
	}
	seen := map[string]bool{}
	for _, task := range dl.calls {
		if seen[task.EpisodeNum] {
			t.Fatalf("episode %s dispatched twice: %+v", task.EpisodeNum, dl.calls)
		}
		seen[task.EpisodeNum] = true
	}
	if settle.count != 3 || settle.total != 3 {
		t.Fatalf("all three episodes must settle ok, got %d/%d", settle.count, settle.total)
	}
	if settle.cancelled {
		t.Fatal("an uncancelled batch must not report cancelled")
	}
}

// TestDirectScreenRendersProgressRows: the screen renders one row per
// episode with the live bar advancing (a hermetic fake progress
// source) and the settle summary at the end.
func TestDirectScreenRendersProgressRows(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{ticks: []DownloadProgress{
		{Fraction: 0.5, Known: true, TimeSec: 60},
	}}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)

	// Drive manually to catch a mid-flight view: the settle arrives
	// through the pump; after settle the summary renders.
	settle := driveDirectScreen(t, ds)
	if settle.count != 3 {
		t.Fatalf("all episodes must download, got %d/%d", settle.count, settle.total)
	}
	v := ds.View().Content
	if !strings.Contains(v, "✓ episode 1") || !strings.Contains(v, "✓ episode 3") {
		t.Fatalf("the done rows must render:\n%s", v)
	}
	if !strings.Contains(v, "Episodes downloaded: 3") {
		t.Fatalf("the settle summary must render:\n%s", v)
	}
	if !strings.Contains(v, "any key returns") {
		t.Fatalf("the done hint must render:\n%s", v)
	}
}

// TestDirectScreenMidFlightViewRendersBar: mid-batch the active row
// renders the live bar with the honest fraction.
func TestDirectScreenMidFlightViewRendersBar(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{ticks: []DownloadProgress{
		{Fraction: 0.42, Known: true, TimeSec: 42},
	}}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)

	// Run Init's batch command synchronously (it blocks to completion)
	// but sample the view after the FIRST tick lands.
	initCmd := ds.Init()
	_ = initCmd
	// The batch ran inline: the full settle already flowed; instead
	// pin the replay through the recorded ticks: re-drive a fresh
	// screen and assert the bar rendering against a tick message.
	ds2 := newDirectDownloadScreen(s.deps, directBatch{
		tasks:     []DownloadTask{{EpisodeNum: "1"}},
		preferred: "dub",
		episodes:  []string{"1"},
	})
	ds2.Update(directResolveMsg{done: 1, total: 1, idx: 0, episode: "1", verdict: "dub"})
	ds2.Update(directTickMsg{idx: 0, pct: 0.42, hasPct: true, timeSec: 42})
	v := ds2.View().Content
	if !strings.Contains(v, "42%") {
		t.Fatalf("the live bar must render the honest fraction:\n%s", v)
	}
	ds2.Update(directTickMsg{idx: 0, pct: 0, hasPct: false, timeSec: 63})
	v = ds2.View().Content
	if !strings.Contains(v, "1:03") {
		t.Fatalf("an indeterminate row must render the elapsed time, not a fake percent:\n%s", v)
	}
}

// TestDirectScreenLocksNavigation: while the batch runs, no key
// leaves the screen and nothing but the cancel pair opens the
// confirm; «Нет» keeps downloading.
func TestDirectScreenLocksNavigation(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)
	ds.Init()

	for _, key := range []tea.KeyPressMsg{
		{Code: 'j'}, {Code: 'k'}, {Code: tea.KeyEnter}, {Code: 'q'},
	} {
		next, cmd := ds.Update(key)
		if cmd != nil {
			t.Fatalf("a locked screen must not emit commands, got %v for %v", cmd, key.Code)
		}
		if next != Screen(ds) {
			t.Fatal("a locked screen must never replace itself")
		}
	}
	// The cancel pair opens the confirmation.
	next, _ := ds.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	d := next.(*directDownloadScreen)
	if d.confirm == nil {
		t.Fatal("Esc must open the cancel confirmation")
	}
	// «Нет — продолжать» (the second row) keeps the batch.
	d.confirm.Jump(1)
	next, _ = d.Update(enter())
	d = next.(*directDownloadScreen)
	if d.confirm != nil {
		t.Fatal("the answer must close the confirmation")
	}
	v := d.View().Content
	if !strings.Contains(v, "stop the download") {
		t.Fatalf("the running hint must render after a declined cancel:\n%s", v)
	}
}

// TestDirectScreenCancelStopsBatch: the confirmed cancel kills the
// batch context (the fake honors it), the running episode reports
// the cancellation, and the settle lands as cancelled on the screen.
func TestDirectScreenCancelStopsBatch(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{blockOnCtx: true, entered: make(chan struct{})}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)

	// Init arms the batch + its pump; run ONLY the batch child here —
	// the test itself is the pump (waitOne + Update below).
	initCmd := ds.Init()
	msg := initCmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init must arm a batch, got %T", msg)
	}
	go func() { _ = batch[0]() }()
	// Deterministic handoff: wait until the batch is INSIDE the
	// first episode's blocking download before cancelling.
	<-dl.entered
	// The batch goroutine is inside episode 1's DownloadDetailed; the
	// confirm + stop must cancel the context even mid-episode.
	d := ds
	d.Update(tea.KeyPressMsg{Code: 'c'})
	next, _ := d.Update(enter()) // the first confirm row = «Остановить»
	d = next.(*directDownloadScreen)

	// Wait for the settle to flow through the events channel.
	deadline := 0
	for !d.done && deadline < 200 {
		msg := d.waitOne()
		if msg != nil {
			d.Update(msg)
		}
		deadline++
	}
	if !d.done {
		t.Fatal("the cancelled batch must settle on the screen")
	}
	if !d.cancelled {
		t.Fatal("the settle must be typed as cancelled")
	}
	if !dl.cancelled {
		t.Fatal("the download context must be cancelled (ffmpeg killed)")
	}
	if len(dl.calls) != 1 {
		t.Fatalf("the cancel must stop the batch after the running episode, got %d calls", len(dl.calls))
	}
	v := d.View().Content
	if !strings.Contains(v, "Download stopped") {
		t.Fatalf("the cancelled settle must render:\n%s", v)
	}
	// Fix-round MED-3: the episode that was mid-download when the
	// cancel landed reads the same «stopped» line as its siblings —
	// the «context canceled» mechanics stay out of the surface.
	if strings.Contains(v, "context canceled") {
		t.Fatalf("the cancelled rows must not render the raw mechanics:\n%s", v)
	}
	// After the settle any key pops back.
	next, cmd := d.Update(enter())
	_ = next
	if cmd == nil {
		t.Fatal("a key on the settled screen must pop back to the session")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("the settled screen must pop, got %T", cmd)
	}
}

// waitOne pumps one event, blocking briefly (test-only affordance
// over the events channel).
func (d *directDownloadScreen) waitOne() tea.Msg {
	select {
	case msg, ok := <-d.events:
		if !ok {
			return nil
		}
		return msg
	case <-time.After(2 * time.Second):
		return nil
	}
}

// TestSingleFlightRejectsSecondRequest (PR174 S4): while a background
// batch is active, every download entry — the action-menu pick and
// the mode dispatch — is REJECTED with the i18n status; the first
// request keeps running untouched (nothing re-dispatches).
func TestSingleFlightRejectsSecondRequest(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &fakeDownload{banner: "⬇ Background downloads: 3"}
	s := pr174DirectSession(t, dl)

	// The action-menu entry rejects.
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("a busy download must keep the session on the menu, got %v", ss.state)
	}
	if !strings.Contains(ss.status, "already running") {
		t.Fatalf("the rejection must carry the busy status:\n%s", ss.status)
	}
	if len(dl.downloads) != 0 || len(dl.submitted) != 0 {
		t.Fatalf("the rejection must dispatch nothing, got %d/%d",
			len(dl.downloads), len(dl.submitted))
	}
}

// TestSingleFlightRejectsAtModePick: a background batch that started
// while the prompts were open rejects at the dispatch seam too.
func TestSingleFlightRejectsAtModePick(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &fakeDownload{}
	s := pr174DirectSession(t, dl)
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.rangeInput.typeText("1-2")
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	next, _ = ss.Update(enter()) // content → mode menu (not busy YET)
	ss = next.(*sessionScreen)
	// A background batch lands while the mode menu is open.
	dl.banner = "⬇ Background downloads: 5"
	next, cmd := ss.Update(enter()) // the direct pick
	if cmd != nil {
		t.Fatal("the mode pick must reject while a download is busy")
	}
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("the rejected pick must return to the menu, got %v", ss.state)
	}
	if !strings.Contains(ss.status, "already running") {
		t.Fatalf("the rejection status must render:\n%s", ss.status)
	}
}

// TestDirectScreenLocksAppStack: while the direct screen runs, keys
// never reach the session below (App routes only to the top) — the
// structural half of the single-flight guard.
func TestDirectScreenLocksAppStack(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	dl := &countingDownload{}
	s := pr174DirectSession(t, dl)
	ds := pr174PushDirectScreen(t, s)
	app := NewApp(s, s.deps, testLogger())
	model, _ := app.Update(pushMsg{screen: ds})
	a := model.(App)

	// The download action fires on the session below: nothing must
	// move (the session is not the top screen).
	before := len(dl.calls)
	nextA, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = nextA.(App)
	if len(dl.calls) != before {
		t.Fatalf("keys must not reach the session under the locked screen, %d calls", len(dl.calls))
	}
	top := a.stack[len(a.stack)-1]
	if top.ID() != downloadScreenID {
		t.Fatalf("the direct screen must stay on top, got %s", top.ID())
	}
}

// runDirectBatchCollect runs the batch machinery without a screen:
// the events drain into the returned settle (the direct screen's
// runner is the only batch executor since the PR174 mode redesign).
func runDirectBatchCollect(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, strict bool) directSettleMsg {
	labels := make([]string, len(tasks))
	for i, t := range tasks {
		labels[i] = t.EpisodeNum
	}
	events := make(chan tea.Msg, 64+len(tasks)*4)
	done := make(chan directSettleMsg, 1)
	go func() {
		for m := range events {
			if s, ok := m.(directSettleMsg); ok {
				done <- s
			}
		}
	}()
	runDirectBatch(ctx, deps, directBatch{tasks: tasks, preferred: preferred, strict: strict, episodes: labels}, events)
	return <-done
}
