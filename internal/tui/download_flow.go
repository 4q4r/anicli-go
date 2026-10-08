package tui

// Download-flow machinery (PR64): the availability line of the
// download-range prompt and the per-episode dub resolution shared by
// the foreground and background range downloads (the PR63 watch-flow
// semantics — python resolve_dubs_smart).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// availabilityMaxRuns caps the rendered run list: a pathological
// many-gap merge (1100+ gappy episodes, dozens of junk labels) must
// not render a multi-KB status line (review fix 5).
const availabilityMaxRuns = 8

// describeAvailableEpisodes renders the compact availability line of
// the download-range prompt (PR64 #2): the count plus the real
// available set — consecutive episodes collapsed into runs, gaps and
// non-numeric labels listed as-is, so the user never guesses what
// exists before typing a range. The caller's order is preserved —
// the session passes its merged (display) order. More than
// availabilityMaxRuns runs truncate to the first ones plus «… +N ещё».
func describeAvailableEpisodes(order []string) string {
	if len(order) == 0 {
		return i18n.T("download.no_episodes")
	}
	runs := make([]string, 0, len(order))
	for i := 0; i < len(order); {
		n, err := strconv.Atoi(order[i])
		if err != nil {
			runs = append(runs, order[i])
			i++
			continue
		}
		j := i + 1
		for j < len(order) {
			m, err := strconv.Atoi(order[j])
			if err != nil || m != n+(j-i) {
				break
			}
			j++
		}
		if j-i == 1 {
			runs = append(runs, order[i])
		} else {
			runs = append(runs, order[i]+"–"+order[j-1])
		}
		i = j
	}
	if len(runs) > availabilityMaxRuns {
		hidden := len(runs) - availabilityMaxRuns
		runs = append(runs[:availabilityMaxRuns], i18n.T("download.more_runs", i18n.Vals{"count": strconv.Itoa(hidden)}))
	}
	return i18n.T("download.available", i18n.Vals{
		"count": strconv.Itoa(len(order)), "runs": strings.Join(runs, ", ")})
}

// The download-resolution budgets are the watch-flow lookup budget
// class (lookupTimeout), applied PER PHASE (review fix 1): hydration
// and every candidate probe each get their own fresh budget, so one
// slow phase can never starve the remaining probes into a false
// «нет доступных озвучек». Vars so the budget-scoping tests can
// shrink them.
var (
	downloadHydrateBudget = lookupTimeout
	downloadProbeBudget   = lookupTimeout
)

// errNoViableDub types the verdict of a range episode whose every dub
// is dead (PR64 #3): the report names the episode instead of silently
// skipping it or aborting the whole range.
var errNoViableDub = fmt.Errorf("no viable dubs with streams")

// downloadEpisodeReport is one line of the per-episode download
// report (PR64 #3): the episode, the dub actually used, and the
// verdict — the written file path or the failure reason.
type downloadEpisodeReport struct {
	Episode string
	// Dub is the resolved dub key; "" marks a no-viable-dub episode.
	Dub string
	// Path is the written file (foreground successes; the background
	// mode queues and reports later).
	Path string
	// Err is the failure (resolution or download); nil on success.
	Err error
}

// resolveDownloadDub resolves ONE range episode's dub with the PR63
// watch-flow semantics (python resolve_dubs_smart): the remembered
// dub first; if its mirror is dead on this episode (the per-episode
// rotation), fall back to the other dubs in the established order —
// the same sortedEmbedKeys order the merged picker and the audio
// prompt use. The hydration is FRESH (PR111: the ResolveStream cache
// is banned — the ladder must probe the provider's current links, not
// the episode's cached ones). Returns "" when no dub is viable — the
// caller types the failure instead of skipping silently.
//
// The machinery is the watch flow's own: liveness = a scoped
// resolveAllStreams success, hydration = hydrateEpisodeFresh — with
// the watch flow's budgeting too: hydration and each probe are
// independently bounded (review fix 1).
func resolveDownloadDub(ctx context.Context, deps *Deps, ep contracts.Episode, preferred string) string {
	hctx, hcancel := context.WithTimeout(ctx, downloadHydrateBudget)
	ep = hydrateEpisodeFresh(hctx, deps, ep)
	hcancel()
	candidates := make([]string, 0, len(ep.RawEmbeds)+1)
	if preferred != "" {
		candidates = append(candidates, preferred)
	}
	for _, k := range sortedEmbedKeys(ep.RawEmbeds) {
		if k != preferred && len(ep.RawEmbeds[k]) > 0 {
			candidates = append(candidates, k)
		}
	}
	for _, cand := range candidates {
		pctx, pcancel := context.WithTimeout(ctx, downloadProbeBudget)
		// PR94: the probe needs only the viability verdict — the
		// per-provider skip records ride the merged resolve but the
		// ladder's own preferred→rest walk already degrades per dub.
		entries, _, err := resolveAllStreams(pctx, deps.Episode, ep, cand, deps.Log)
		pcancel()
		if err == nil && len(entries) > 0 {
			return cand
		}
	}
	return ""
}

// hydrateForDownload was the watch flow's gated hydration round; the
// PR111 always-fresh resolve replaced it with hydrateEpisodeFresh
// everywhere (the ResolveStream cache is banned).

// stampResolvedDub pins the per-episode resolution onto the task.
func stampResolvedDub(task *DownloadTask, dub string) {
	task.DubID = dub
	task.ProviderID = providerOfTrackKey(dub)
}

// downloadResolveFanout bounds the concurrent per-episode dub
// resolutions of one range batch (review fix 2) — the same order as
// the provider fan-out consts.
const downloadResolveFanout = 8

// emitProgress publishes one progress line without ever blocking the
// batch: a nil channel (background mode) or a saturated buffer just
// drops the sample (the throttle keeps the next ones coming).
func emitProgress(ch chan<- string, line string) {
	if ch == nil {
		return
	}
	select {
	case ch <- line:
	default:
	}
}

// downloadResolveLine / downloadProgressLine render the per-episode
// progress ticks of a running range batch.
func downloadResolveLine(done, total int, episode, verdict string) string {
	return i18n.T("download.resolve_line", i18n.Vals{
		"done": strconv.Itoa(done), "total": strconv.Itoa(total),
		"ep": episode, "verdict": verdict})
}

func downloadProgressLine(done, total int, episode, verdict string) string {
	return i18n.T("download.progress_line", i18n.Vals{
		"done": strconv.Itoa(done), "total": strconv.Itoa(total),
		"ep": episode, "verdict": verdict})
}

// resolveDownloadDubs resolves EVERY range episode's dub through the
// repo's bounded pool (≤ downloadResolveFanout; a probe fns never
// fails, so one episode's error cannot cancel its siblings — the
// watch-flow fan-out rule). The returned report stubs are in EPISODE
// order regardless of completion order; each settled episode emits a
// resolve tick (nil channel = silent).
func resolveDownloadDubs(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, progCh chan<- string) []downloadEpisodeReport {
	type indexedTask struct {
		idx  int
		task DownloadTask
	}
	items := make([]indexedTask, len(tasks))
	for i, task := range tasks {
		items[i] = indexedTask{idx: i, task: task}
	}
	stubs := make([]downloadEpisodeReport, len(tasks))
	var (
		mu   sync.Mutex
		done int
	)
	_ = netclient.Parallel(ctx, items, downloadResolveFanout, func(ctx context.Context, it indexedTask) error {
		dub := resolveDownloadDub(ctx, deps, it.task.Episode, preferred)
		mu.Lock()
		defer mu.Unlock()
		done++
		if dub == "" {
			stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, Err: errNoViableDub}
			emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, i18n.T("download.no_dubs_verdict")))
			return nil
		}
		stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, Dub: dub}
		emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, dub))
		return nil
	})
	return stubs
}

// runForegroundDownload resolves the range's dubs bounded-parallel
// (the slow leg), then downloads sequentially in episode order (the
// heavy leg), typing every episode's verdict in the report. A dead
// episode never aborts its siblings; every settled episode ticks the
// progress channel.
func runForegroundDownload(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, progCh chan<- string) downloadSettledMsg {
	stubs := resolveDownloadDubs(ctx, deps, tasks, preferred, progCh)
	report := make([]downloadEpisodeReport, len(tasks))
	ok, firstErr := 0, error(nil)
	for i, task := range tasks {
		stub := stubs[i]
		report[i] = downloadEpisodeReport{Episode: stub.Episode, Dub: stub.Dub, Err: stub.Err}
		if stub.Err != nil {
			if firstErr == nil {
				firstErr = stub.Err
			}
			continue
		}
		stamped := task
		stampResolvedDub(&stamped, stub.Dub)
		emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, stub.Dub+"…"))
		path, err := deps.Download.Download(ctx, stamped)
		if err != nil {
			report[i].Err = err
			emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, i18n.T("download.verdict_error", i18n.Vals{"err": err.Error()})))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		report[i].Path = path
		ok++
		emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, i18n.T("download.verdict_done")))
	}
	return downloadSettledMsg{count: ok, total: len(tasks), err: firstErr, report: report}
}

// queueBackgroundDownloads resolves the range's dubs bounded-parallel
// BEFORE queueing, so the background manager never receives a task
// whose dub is dead on its episode. The settle types the verdicts in
// episode order.
func queueBackgroundDownloads(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string) backgroundQueuedMsg {
	stubs := resolveDownloadDubs(ctx, deps, tasks, preferred, nil)
	report := make([]downloadEpisodeReport, len(tasks))
	queued := 0
	for i, task := range tasks {
		stub := stubs[i]
		report[i] = downloadEpisodeReport{Episode: stub.Episode, Dub: stub.Dub, Err: stub.Err}
		if stub.Err != nil {
			continue
		}
		stamped := task
		stampResolvedDub(&stamped, stub.Dub)
		deps.Download.Submit(stamped)
		queued++
	}
	return backgroundQueuedMsg{queued: queued, total: len(tasks), report: report}
}

// renderDownloadSettle composes the foreground settle line: the
// headline plus the compact per-episode report. The all-failed
// headline carries the first error verbatim — the pre-PR64
// «Ошибка загрузки: …» contract (review fix 6); the typed no-dub
// verdict stays on its per-episode line.
func renderDownloadSettle(msg downloadSettledMsg) string {
	var b strings.Builder
	switch {
	case msg.err == nil:
		fmt.Fprintf(&b, "%s", i18n.T("download.settle_ok", i18n.Vals{"count": strconv.Itoa(msg.count)}))
	case msg.count > 0:
		fmt.Fprintf(&b, "%s", i18n.T("download.settle_partial", i18n.Vals{
			"count": strconv.Itoa(msg.count), "total": strconv.Itoa(msg.total)}))
	default:
		fmt.Fprintf(&b, "%s", i18n.T("download.settle_failed", i18n.Vals{"total": strconv.Itoa(msg.total)}))
		if msg.err != nil && !errors.Is(msg.err, errNoViableDub) {
			fmt.Fprintf(&b, " — %v", msg.err)
		}
	}
	for _, r := range msg.report {
		b.WriteString("\n")
		switch {
		case r.Err != nil && r.Dub != "":
			fmt.Fprintf(&b, "%s", i18n.T("download.row_error", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub, "err": fmt.Sprint(r.Err)}))
		case r.Err != nil:
			fmt.Fprintf(&b, "%s", i18n.T("download.row_no_dubs", i18n.Vals{"ep": r.Episode}))
		case r.Path != "":
			fmt.Fprintf(&b, "%s", i18n.T("download.row_path", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub, "path": r.Path}))
		default:
			fmt.Fprintf(&b, "%s", i18n.T("download.row_done", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub}))
		}
	}
	return b.String()
}

// renderBackgroundQueued composes the background settle line: the
// queued headline plus the per-episode dub typing.
func renderBackgroundQueued(msg backgroundQueuedMsg) string {
	var b strings.Builder
	if msg.queued == msg.total {
		fmt.Fprintf(&b, "%s", i18n.T("download.bg_queued_all", i18n.Vals{"count": strconv.Itoa(msg.queued)}))
	} else {
		fmt.Fprintf(&b, "%s", i18n.T("download.bg_queued_partial", i18n.Vals{
			"count": strconv.Itoa(msg.queued), "total": strconv.Itoa(msg.total)}))
	}
	for _, r := range msg.report {
		b.WriteString("\n")
		if r.Err != nil {
			fmt.Fprintf(&b, "%s", i18n.T("download.row_no_dubs", i18n.Vals{"ep": r.Episode}))
			continue
		}
		fmt.Fprintf(&b, "%s", i18n.T("download.row_background", i18n.Vals{
			"ep": r.Episode, "dub": r.Dub}))
	}
	return b.String()
}
