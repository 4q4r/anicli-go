package tui

// PR174 S5 (owner spec 2026-10-22): the DIRECT download is a
// dedicated screen with per-episode progress rows — a full progress
// dashboard, not a scrolling status line. Navigation is LOCKED while
// the batch runs: the only early exit is an explicit confirmed
// cancel (ffmpeg killed by context, the .part removed), which
// structurally eliminates the S4 double-dispatch class at the UX
// layer together with the single-flight guard. The batch continues
// on per-episode errors; the settle summary reuses the established
// report renderer.

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// downloadScreenID is the identity of the direct download screen.
const downloadScreenID = "__download__"

// directRowState is one episode row's lifecycle on the direct screen.
type directRowState string

const (
	directRowPending   directRowState = "pending"
	directRowResolving directRowState = "resolving"
	directRowActive    directRowState = "active"
	directRowDone      directRowState = "done"
	directRowFailed    directRowState = "failed"
	directRowSkipped   directRowState = "skipped"
	directRowStopped   directRowState = "stopped"
)

// directRow is one episode of the batch as the screen renders it.
type directRow struct {
	episode string
	dub     string
	state   directRowState
	// pct/hasPct drive the live bar of the active row (indeterminate
	// rows render the honest elapsed output time instead).
	pct    float64
	hasPct bool
	// timeSec is ffmpeg's out_time (the honest elapsed even without
	// a measurable total).
	timeSec float64
	err     error
}

// directBatch carries the settled batch parameters from the session
// into the screen (the sync-ask already decided; the tasks carry
// AudioSync/AudioSet/Quality).
type directBatch struct {
	tasks     []DownloadTask
	preferred string
	strict    bool
	episodes  []string
}

// The screen's message protocol (the events channel feeds a re-arming
// pump exactly like the buffered-watch channel).
type (
	// directResolveMsg reports one episode's resolve verdict.
	directResolveMsg struct {
		done, total int
		idx         int
		episode     string
		verdict     string
		skip        bool
	}
	// directTickMsg is one progress sample of the running episode.
	directTickMsg struct {
		idx     int
		pct     float64
		hasPct  bool
		timeSec float64
	}
	// directRowMsg settles one episode's download (or skip/failure).
	directRowMsg struct {
		idx    int
		row    directRow
		report downloadEpisodeReport
	}
	// directSettleMsg closes the batch with the summary report.
	directSettleMsg struct {
		count, total int
		firstErr     error
		report       []downloadEpisodeReport
		cancelled    bool
	}
	// directPumpEnd closes the event pump (the channel drained).
	directPumpEnd struct{}
)

// emitDirect is the non-blocking publish of the batch goroutine: a
// saturated buffer drops ticks (the next sample is seconds away);
// row/settle messages never drop — the buffer is sized for the whole
// batch's worth of them.
func emitDirect(ch chan<- tea.Msg, msg tea.Msg, drop bool) {
	if ch == nil {
		return
	}
	if drop {
		select {
		case ch <- msg:
		default:
		}
		return
	}
	ch <- msg
}

// directDownloadScreen is the PR174 direct download surface.
type directDownloadScreen struct {
	deps   *Deps
	batch  directBatch
	rows   []directRow
	ctx    context.Context
	cancel context.CancelFunc
	events chan tea.Msg
	// resolve bookkeeping for the stage line.
	resolveDone, resolveTotal int
	stageResolving            bool
	// done/cancelled/stopping track the batch lifecycle.
	done      bool
	cancelled bool
	stopping  bool
	summary   string
	// confirm is the cancel-confirmation menu (nil = not asking).
	confirm *PinList
}

// newDirectDownloadScreen builds the locked progress screen for one
// settled batch.
func newDirectDownloadScreen(deps *Deps, batch directBatch) *directDownloadScreen {
	rows := make([]directRow, len(batch.episodes))
	for i, ep := range batch.episodes {
		rows[i] = directRow{episode: ep, state: directRowPending}
	}
	return &directDownloadScreen{
		deps:   deps,
		batch:  batch,
		rows:   rows,
		events: make(chan tea.Msg, 64+len(batch.episodes)*4),
	}
}

// ID implements Screen.
func (d *directDownloadScreen) ID() string { return downloadScreenID }

// Init implements Screen: the cancellable batch + the event pump.
func (d *directDownloadScreen) Init() tea.Cmd {
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.stageResolving = true
	d.resolveTotal = len(d.batch.tasks)
	deps, batch, events := d.deps, d.batch, d.events
	run := safeCmd(downloadScreenID, func() tea.Msg {
		runDirectBatch(d.ctx, deps, batch, events)
		return nil
	})
	return tea.Batch(run, waitDirectProgress(events))
}

// Update implements Screen.
func (d *directDownloadScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case directResolveMsg:
		d.resolveDone = m.done
		if d.resolveDone >= d.resolveTotal {
			d.stageResolving = false
		}
		if m.idx >= 0 && m.idx < len(d.rows) {
			if m.skip {
				d.rows[m.idx].state = directRowSkipped
				d.rows[m.idx].dub = m.verdict
			} else {
				d.rows[m.idx].state = directRowActive
				d.rows[m.idx].dub = m.verdict
			}
		}
		return d, waitDirectProgress(d.events)
	case directTickMsg:
		if m.idx >= 0 && m.idx < len(d.rows) {
			d.rows[m.idx].pct = m.pct
			d.rows[m.idx].hasPct = m.hasPct
			d.rows[m.idx].timeSec = m.timeSec
		}
		return d, waitDirectProgress(d.events)
	case directRowMsg:
		if m.idx >= 0 && m.idx < len(d.rows) {
			d.rows[m.idx] = m.row
		}
		return d, waitDirectProgress(d.events)
	case directSettleMsg:
		d.done = true
		d.cancelled = m.cancelled
		d.summary = renderDownloadSettle(downloadSettledMsg{
			count: m.count, total: m.total, err: m.firstErr, report: m.report,
		})
		if m.cancelled {
			for i := range d.rows {
				switch d.rows[i].state {
				case directRowPending, directRowResolving, directRowActive:
					d.rows[i].state = directRowStopped
				}
			}
		}
		return d, waitDirectProgress(d.events)
	case directPumpEnd:
		return d, nil
	case tea.KeyPressMsg:
		return d.handleKey(m)
	default:
		return d, nil
	}
}

// handleKey: the locked surface. While the batch runs every key is a
// no-op except the cancel pair (Esc/c) which opens the confirmation;
// after the settle any key pops back to the session.
func (d *directDownloadScreen) handleKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if d.confirm != nil {
		return d.handleConfirmKey(key)
	}
	if d.done {
		return d, pop()
	}
	if key.Code == tea.KeyEscape || key.Code == 'c' {
		d.confirm = NewPinList(NewMenu(i18n.T("download.cancel_title"), "", []Choice{
			{ID: "stop", Label: i18n.T("download.cancel_stop"), Value: "stop"},
			{ID: "continue", Label: i18n.T("download.cancel_continue"), Value: "continue"},
		}...), defaultListHeight)
		return d, nil
	}
	return d, nil
}

// handleConfirmKey drives the cancel confirmation: «Остановить»
// cancels the batch context (ffmpeg dies, the download package's
// error path removes the .part) and waits for the settle on this
// screen; anything else closes the prompt and keeps downloading.
func (d *directDownloadScreen) handleConfirmKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if d.confirm.HandleKey(key) {
		return d, nil
	}
	resolved := ResolveKey(d.confirm.Menu(), d.confirm.Cursor(), key)
	if resolved == nil {
		return d, nil
	}
	answer, _ := resolved.(string)
	d.confirm = nil
	if answer != "stop" {
		return d, nil
	}
	if d.cancel != nil && !d.stopping {
		d.stopping = true
		d.cancel()
	}
	// The settle arrives through the pump; the screen stays until it
	// lands (the batch goroutine exits on the cancelled context).
	return d, nil
}

// formatClock renders seconds as m:ss (the progress bar's tail).
func formatDirectClock(sec float64) string {
	total := int(sec)
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// renderBar draws the active row's progress: a filled bar with the
// percentage when measurable, the honest elapsed output time
// otherwise (never a fabricated fraction).
func renderBar(r directRow) string {
	const width = 20
	filled := int(r.pct * float64(width))
	if filled > width {
		filled = width
	}
	if r.hasPct {
		return fmt.Sprintf("[%s%s] %3.0f%%",
			strings.Repeat("█", filled), strings.Repeat("░", width-filled), r.pct*100)
	}
	return fmt.Sprintf("[%s] %s", strings.Repeat("░", width), formatDirectClock(r.timeSec))
}

// View implements Screen: the per-episode progress dashboard.
func (d *directDownloadScreen) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render(i18n.T("download.screen_title")) + "\n\n")
	if d.confirm != nil {
		b.WriteString(themedList(d.confirm))
		return tea.NewView(b.String())
	}
	if d.stageResolving {
		b.WriteString(theme.StatusLine.Render(i18n.T("download.stage_resolving")) + "\n\n")
	} else if !d.done {
		b.WriteString(theme.StatusLine.Render(i18n.T("download.stage_downloading")) + "\n\n")
	}
	for i, r := range d.rows {
		switch r.state {
		case directRowPending:
			b.WriteString(theme.Dim.Render(i18n.T("download.screen_row_pending", i18n.Vals{"ep": r.episode})) + "\n")
		case directRowResolving:
			b.WriteString(theme.Dim.Render(i18n.T("download.screen_row_pending", i18n.Vals{"ep": r.episode})) + "\n")
		case directRowActive:
			line := i18n.T("download.screen_row_active", i18n.Vals{"ep": r.episode, "dub": stripProviderTag(r.dub)})
			b.WriteString(theme.StatusLine.Render(line) + " " + renderBar(d.rows[i]) + "\n")
		case directRowDone:
			b.WriteString(theme.Success.Render(i18n.T("download.screen_row_done", i18n.Vals{"ep": r.episode, "dub": stripProviderTag(r.dub)})) + "\n")
		case directRowFailed:
			b.WriteString(theme.Error.Render(i18n.T("download.screen_row_failed", i18n.Vals{
				"ep": r.episode, "dub": stripProviderTag(r.dub), "err": fmt.Sprint(r.err)})) + "\n")
		case directRowSkipped:
			b.WriteString(theme.Dim.Render(i18n.T("download.screen_row_skipped", i18n.Vals{
				"ep": r.episode, "reason": r.dub})) + "\n")
		case directRowStopped:
			b.WriteString(theme.Dim.Render(i18n.T("download.cancelled_line")) + "\n")
		}
	}
	b.WriteString("\n")
	switch {
	case d.done:
		b.WriteString(theme.Success.Render(d.summary) + "\n\n")
		b.WriteString(theme.StatusLine.Render(i18n.T("download.screen_done_hint")))
	default:
		b.WriteString(theme.StatusLine.Render(i18n.T("download.screen_lock_hint")))
	}
	return tea.NewView(b.String())
}

// waitDirectProgress is the re-arming pump of the screen's event
// channel (the buffered-watch pattern).
func waitDirectProgress(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return directPumpEnd{}
		}
		return msg
	}
}

// runDirectBatch executes the settled batch on the screen's
// cancellable context: the bounded-parallel resolve phase first
// (every settled episode flips its row), then the sequential download
// leg in episode order — a dead episode never aborts its siblings.
// The download package's cancellation paths remove the .part files;
// the settle lands on the screen with the established report.
func runDirectBatch(ctx context.Context, deps *Deps, batch directBatch, events chan<- tea.Msg) {
	defer close(events)
	stubs := resolveDownloadDubs(ctx, deps, batch.tasks, batch.preferred, batch.strict, func(done, total int, idx int, episode, verdict string, skip bool) {
		emitDirect(events, directResolveMsg{done: done, total: total, idx: idx, episode: episode, verdict: verdict, skip: skip}, false)
	})
	report := make([]downloadEpisodeReport, len(batch.tasks))
	ok, firstErr := 0, error(nil)
	for i, task := range batch.tasks {
		stub := stubs[i]
		report[i] = downloadEpisodeReport{
			Episode: stub.Episode, Dub: stub.Dub, Err: stub.Err,
			SkipDub: stub.SkipDub, DroppedAudio: stub.DroppedAudio,
			SkipAudioSet: stub.SkipAudioSet,
		}
		switch {
		case stub.SkipDub != "" || stub.SkipAudioSet:
			directSettleRow(events, i, batch.episodes[i], directRow{
				episode: batch.episodes[i], state: directRowSkipped, dub: i18n.T("download.skip_verdict"),
			}, report[i])
			continue
		case stub.Err != nil:
			if firstErr == nil {
				firstErr = stub.Err
			}
			directSettleRow(events, i, batch.episodes[i], directRow{
				episode: batch.episodes[i], state: directRowFailed, dub: stub.Dub, err: stub.Err,
			}, report[i])
			continue
		}
		if ctx.Err() != nil {
			// The confirmed cancel: stop the whole batch (never walk
			// the rest into guaranteed cancellation failures).
			emitDirect(events, directSettleMsg{
				count: ok, total: len(batch.tasks), firstErr: firstErr,
				report: report, cancelled: true,
			}, false)
			return
		}
		stamped := task
		stampResolvedDub(&stamped, stub.Dub)
		path, dlDropped, dlSync, err := deps.Download.DownloadDetailed(ctx, stamped, func(p DownloadProgress) {
			emitDirect(events, directTickMsg{idx: i, pct: p.Fraction, hasPct: p.Known, timeSec: p.TimeSec}, true)
		})
		if err != nil {
			report[i].Err = err
			report[i].SyncNotes = dlSync
			if firstErr == nil {
				firstErr = err
			}
			directSettleRow(events, i, batch.episodes[i], directRow{
				episode: batch.episodes[i], state: directRowFailed, dub: stub.Dub, err: err,
			}, report[i])
			continue
		}
		report[i].Path = path
		report[i].DroppedAudio = mergeDroppedAudio(report[i].DroppedAudio, dlDropped)
		report[i].SyncNotes = dlSync
		ok++
		directSettleRow(events, i, batch.episodes[i], directRow{
			episode: batch.episodes[i], state: directRowDone, dub: stub.Dub,
		}, report[i])
	}
	cancelled := ctx.Err() != nil
	emitDirect(events, directSettleMsg{
		count: ok, total: len(batch.tasks), firstErr: firstErr,
		report: report, cancelled: cancelled,
	}, false)
}

// directSettleRow publishes one episode's settled row.
func directSettleRow(events chan<- tea.Msg, idx int, episode string, row directRow, report downloadEpisodeReport) {
	row.episode = episode
	emitDirect(events, directRowMsg{idx: idx, row: row, report: report}, false)
}
