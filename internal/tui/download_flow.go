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

// SyncNote is one dub's PR165b sync verdict for the report row.
type SyncNote struct {
	// Dub is the bare dub name (the provider tag stripped).
	Dub string
	// Seconds is the applied offset (3-decimal rendering, signed).
	Seconds float64
	// Kind is "applied", "uncertain" or "unavailable".
	Kind string
}

// Sync note kinds (the plan §Task 10 taxonomy + the FIX-ROUND anchor).
const (
	SyncApplied     = "applied"
	SyncUncertain   = "uncertain"
	SyncUnavailable = "unavailable"
	// SyncAnchor marks the anchor donor of a reference-less batch: it
	// ships UNSHIFTED (its timeline is the only reference) and is
	// never silently dropped.
	SyncAnchor = "anchor"
)

// availabilityMaxRuns caps the rendered run list: a pathological
// many-gap merge (1100+ gappy episodes, dozens of junk labels) must
// not render a multi-KB status line (review fix 5).
const availabilityMaxRuns = 8

// episodeRuns collapses an ordered label list: consecutive numeric
// labels into runs («2–5»), non-numeric labels as-is, the caller's
// order preserved. Shared by the availability line and the #164 skip
// attribution.
func episodeRuns(order []string) []string {
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
	return runs
}

// truncateRuns caps a rendered run list: a pathological many-gap
// merge (1100+ gappy episodes, dozens of junk labels) must not render
// a multi-KB line (review fix 5) — the first runs plus «… +N ещё».
func truncateRuns(runs []string) []string {
	if len(runs) > availabilityMaxRuns {
		hidden := len(runs) - availabilityMaxRuns
		runs = append(runs[:availabilityMaxRuns], i18n.T("download.more_runs", i18n.Vals{"count": strconv.Itoa(hidden)}))
	}
	return runs
}

// describeAvailableEpisodes renders the compact availability line of
// the download-range prompt (PR64 #2): the count plus the real
// available set — consecutive episodes collapsed into runs, gaps and
// non-numeric labels listed as-is, so the user never guesses what
// exists before typing a range. The caller's order is preserved —
// the session passes its merged (display) order.
func describeAvailableEpisodes(order []string) string {
	if len(order) == 0 {
		return i18n.T("download.no_episodes")
	}
	runs := truncateRuns(episodeRuns(order))
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
	// SkipDub marks a strict #164 batch episode that does not carry
	// the chosen dub: skipped, never substituted. The renderer folds
	// these rows into the one compact «пропущены» attribution line.
	SkipDub string
	// DroppedAudio names the PR165 marked dubs this episode does not
	// carry: the episode still downloads with video + the dubs it
	// has, and the report row names the drop loudly. Merges the
	// resolve-phase drops with the dubs that died between the resolve
	// and the download (M2).
	DroppedAudio []string
	// SkipAudioSet marks an episode carrying NONE of the marked dubs:
	// skipped entirely (never a silent video-only file), folded into
	// the compact audio-set attribution line.
	SkipAudioSet bool
	// SyncNotes carries the PR165b per-dub sync verdicts of the
	// episode (applied/uncertain/unavailable) — rendered as one row
	// suffix.
	SyncNotes []SyncNote
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
// The strict #164 mode (the ask-first menu pick) forbids the
// rotation: the picked dub alone drives the episode. A dub the fresh
// hydration proves absent skips the episode without a probe; a dub
// present but dead probes alone and types the failure — an episode
// never falls back to a dub the user did not choose.
//
// The machinery is the watch flow's own: liveness = a scoped
// resolveAllStreams success, hydration = hydrateEpisodeFresh — with
// the watch flow's budgeting too: hydration and each probe are
// independently bounded (review fix 1).
func resolveDownloadDub(ctx context.Context, deps *Deps, ep contracts.Episode, preferred string, strict bool) string {
	hctx, hcancel := context.WithTimeout(ctx, downloadHydrateBudget)
	ep = hydrateEpisodeFresh(hctx, deps, ep)
	hcancel()
	return resolveDownloadDubHydrated(ctx, deps, ep, preferred, strict)
}

// resolveDownloadDubHydrated is the candidate walk over an ALREADY
// hydrated episode (PR165: the batch hydrates once per episode and
// reuses the embeds for the marked-audio probes). Semantics identical
// to resolveDownloadDub's post-hydration half.
func resolveDownloadDubHydrated(ctx context.Context, deps *Deps, ep contracts.Episode, preferred string, strict bool) string {
	candidates := make([]string, 0, len(ep.RawEmbeds)+1)
	if strict {
		// #164: the picked dub or nothing — never a substitute.
		if len(ep.RawEmbeds[preferred]) == 0 {
			return ""
		}
		candidates = append(candidates, preferred)
	} else {
		if preferred != "" {
			candidates = append(candidates, preferred)
		}
		for _, k := range sortedEmbedKeys(ep.RawEmbeds) {
			if k != preferred && len(ep.RawEmbeds[k]) > 0 {
				candidates = append(candidates, k)
			}
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

// resolveMarkedAudioSet probes one episode's PR165 marked audio set
// on the hydrated embeds: every key is probed independently (fresh
// streams, the same per-probe budget) and a key the episode does not
// carry drops. Returns the carried keys and the dropped names — an
// empty carried set means the episode muxes nothing of the user's
// selection (the caller skips it, never a silent video-only file).
func resolveMarkedAudioSet(ctx context.Context, deps *Deps, ep contracts.Episode, keys []string) (carried, dropped []string) {
	for _, key := range keys {
		if len(ep.RawEmbeds[key]) == 0 {
			dropped = append(dropped, key)
			continue
		}
		pctx, pcancel := context.WithTimeout(ctx, downloadProbeBudget)
		entries, _, err := resolveAllStreams(pctx, deps.Episode, ep, key, deps.Log)
		pcancel()
		if err != nil || len(entries) == 0 {
			dropped = append(dropped, key)
			continue
		}
		carried = append(carried, key)
	}
	return carried, dropped
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
// resolve tick (nil channel = silent). The strict #164 mode types a
// not-carrying episode as a skip (SkipDub) instead of the
// no-viable-dub error — the batch summary attributes the skips
// compactly, nothing substitutes.
func resolveDownloadDubs(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, strict bool, progCh chan<- string) []downloadEpisodeReport {
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
		// PR165: ONE fresh hydration per episode feeds both the dub
		// walk and the marked-audio probes.
		hctx, hcancel := context.WithTimeout(ctx, downloadHydrateBudget)
		ep := hydrateEpisodeFresh(hctx, deps, it.task.Episode)
		hcancel()
		dub := resolveDownloadDubHydrated(ctx, deps, ep, preferred, strict)
		mu.Lock()
		defer mu.Unlock()
		done++
		if dub == "" {
			if strict {
				stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, SkipDub: preferred}
				emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, i18n.T("download.skip_verdict")))
				return nil
			}
			stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, Err: errNoViableDub}
			emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, i18n.T("download.no_dubs_verdict")))
			return nil
		}
		if len(it.task.AudioSet) > 0 {
			carried, dropped := resolveMarkedAudioSet(ctx, deps, ep, it.task.AudioSet)
			if len(carried) == 0 {
				// None of the marked dubs is on this episode: the
				// episode skips with the compact attribution — never
				// a silent video-only file (mandate #7).
				stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, Dub: dub, SkipAudioSet: true}
				emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, i18n.T("download.audio_skip_verdict")))
				return nil
			}
			stubs[it.idx] = downloadEpisodeReport{Episode: it.task.EpisodeNum, Dub: dub, DroppedAudio: dropped}
			emitProgress(progCh, downloadResolveLine(done, len(tasks), it.task.EpisodeNum, dub))
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
func runForegroundDownload(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, strict bool, progCh chan<- string) downloadSettledMsg {
	stubs := resolveDownloadDubs(ctx, deps, tasks, preferred, strict, progCh)
	report := make([]downloadEpisodeReport, len(tasks))
	ok, firstErr := 0, error(nil)
	for i, task := range tasks {
		stub := stubs[i]
		report[i] = downloadEpisodeReport{
			Episode: stub.Episode, Dub: stub.Dub, Err: stub.Err,
			SkipDub: stub.SkipDub, DroppedAudio: stub.DroppedAudio,
			SkipAudioSet: stub.SkipAudioSet,
		}
		switch {
		case stub.SkipDub != "":
			continue // #164: the chosen dub is absent — skipped, never substituted
		case stub.SkipAudioSet:
			continue // #165: none of the marked dubs is on this episode — skipped
		case stub.Err != nil:
			if firstErr == nil {
				firstErr = stub.Err
			}
			continue
		}
		stamped := task
		stampResolvedDub(&stamped, stub.Dub)
		emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, stub.Dub+"…"))
		path, dlDropped, dlSync, err := deps.Download.Download(ctx, stamped)
		if err != nil {
			report[i].Err = err
			report[i].SyncNotes = dlSync
			emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, i18n.T("download.verdict_error", i18n.Vals{"err": err.Error()})))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		report[i].Path = path
		// M2: the dubs that died between the resolve and the download
		// land on the same row, in the same established suffix.
		report[i].DroppedAudio = mergeDroppedAudio(report[i].DroppedAudio, dlDropped)
		report[i].SyncNotes = dlSync
		ok++
		emitProgress(progCh, downloadProgressLine(i+1, len(tasks), stub.Episode, i18n.T("download.verdict_done")))
	}
	return downloadSettledMsg{count: ok, total: len(tasks), err: firstErr, report: report}
}

// queueBackgroundDownloads resolves the range's dubs bounded-parallel
// BEFORE queueing, so the background manager never receives a task
// whose dub is dead on its episode. The settle types the verdicts in
// episode order. The strict #164 mode leaves the not-carrying
// episodes unqueued with the skip attribution on their report rows.
func queueBackgroundDownloads(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string, strict bool) backgroundQueuedMsg {
	stubs := resolveDownloadDubs(ctx, deps, tasks, preferred, strict, nil)
	report := make([]downloadEpisodeReport, len(tasks))
	queued := 0
	for i, task := range tasks {
		stub := stubs[i]
		report[i] = downloadEpisodeReport{
			Episode: stub.Episode, Dub: stub.Dub, Err: stub.Err,
			SkipDub: stub.SkipDub, DroppedAudio: stub.DroppedAudio,
			SkipAudioSet: stub.SkipAudioSet,
		}
		if stub.Err != nil || stub.SkipDub != "" || stub.SkipAudioSet {
			continue
		}
		stamped := task
		stampResolvedDub(&stamped, stub.Dub)
		deps.Download.Submit(stamped)
		queued++
	}
	return backgroundQueuedMsg{queued: queued, total: len(tasks), report: report}
}

// renderSkippedDubs composes the #164 compact skip attribution: one
// line, the run-compressed episode labels, the dub they lack — the
// #161 «пропущены:» shape (compact, nothing path-shaped, no
// per-episode flood). "" when the report has no skips.
//
// The dub label reads the LAST skip row safely: a strict batch has a
// single chosen dub — resolveDownloadDubs stamps the same SkipDub
// (the menu pick) on every skip it types — so one label names the
// whole run (invariant, review round 2).
func renderSkippedDubs(report []downloadEpisodeReport) string {
	skipped := make([]string, 0, len(report))
	dub := ""
	for _, r := range report {
		if r.SkipDub == "" {
			continue
		}
		skipped = append(skipped, r.Episode)
		dub = r.SkipDub
	}
	if len(skipped) == 0 {
		return ""
	}
	runs := truncateRuns(episodeRuns(skipped))
	return i18n.T("download.skipped_dubs", i18n.Vals{
		"eps": strings.Join(runs, ", "), "dub": dub})
}

// renderSkippedAudioSet composes the #165 compact audio-set
// attribution: one line, the run-compressed episodes carrying NONE of
// the marked dubs — the #161 «пропущены:» shape. "" when the report
// has no audio-set skips.
func renderSkippedAudioSet(report []downloadEpisodeReport) string {
	skipped := make([]string, 0, len(report))
	for _, r := range report {
		if !r.SkipAudioSet {
			continue
		}
		skipped = append(skipped, r.Episode)
	}
	if len(skipped) == 0 {
		return ""
	}
	runs := truncateRuns(episodeRuns(skipped))
	return i18n.T("download.skipped_audio_set", i18n.Vals{
		"eps": strings.Join(runs, ", ")})
}

// renderSyncSuffix renders the PR165b sync verdicts as one row
// suffix: «синхронизация: DUB +17.042с, DUB2: нет уверенности».
// Applied notes render the signed 3-decimal offset; uncertain and
// unavailable render their typed forms. "" when there are no notes.
func renderSyncSuffix(notes []SyncNote) string {
	if len(notes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(notes))
	for _, n := range notes {
		switch n.Kind {
		case SyncApplied:
			parts = append(parts, i18n.T("download.sync_note_applied", i18n.Vals{
				"dub": n.Dub, "offset": fmt.Sprintf("%+.3f", n.Seconds)}))
		case SyncUncertain:
			parts = append(parts, i18n.T("download.sync_note_uncertain", i18n.Vals{"dub": n.Dub}))
		case SyncUnavailable:
			parts = append(parts, i18n.T("download.sync_note_unavailable", i18n.Vals{"dub": n.Dub}))
		case SyncAnchor:
			parts = append(parts, i18n.T("download.sync_note_anchor", i18n.Vals{"dub": n.Dub}))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return i18n.T("download.row_sync", i18n.Vals{"notes": strings.Join(parts, ", ")})
}

// renderDroppedSuffix renders the loud per-episode degradation note:
// the marked dubs the episode downloaded WITHOUT (compact bare dub
// names, the #161 attribution style). "" when nothing dropped.
func renderDroppedSuffix(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	names := make([]string, 0, len(dropped))
	for _, key := range dropped {
		names = append(names, stripProviderTag(key))
	}
	return i18n.T("download.row_dropped", i18n.Vals{"dubs": strings.Join(names, ", ")})
}

// mergeDroppedAudio unions the resolve-phase and download-time (M2)
// drop lists in order without duplicates: a dub dead at BOTH phases
// (the download re-resolves the whole marked set) must not render
// twice on the row suffix.
func mergeDroppedAudio(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, key := range base {
		if !seen[key] {
			seen[key] = true
			merged = append(merged, key)
		}
	}
	for _, key := range extra {
		if !seen[key] {
			seen[key] = true
			merged = append(merged, key)
		}
	}
	return merged
}

// renderDownloadSettle composes the foreground settle line: the
// headline plus the compact per-episode report. The all-failed
// headline carries the first error verbatim — the pre-PR64
// «Ошибка загрузки: …» contract (review fix 6); the typed no-dub
// verdict stays on its per-episode line. Strict #164 skips render as
// the one compact «пропущены» line and fold the headline into the
// partial form — a batch that only skipped is not an error. #165
// audio-set skips fold into their own compact line the same way.
func renderDownloadSettle(msg downloadSettledMsg) string {
	var b strings.Builder
	skips := 0
	for _, r := range msg.report {
		if r.SkipDub != "" || r.SkipAudioSet {
			skips++
		}
	}
	switch {
	case msg.err == nil && skips > 0:
		fmt.Fprintf(&b, "%s", i18n.T("download.settle_partial", i18n.Vals{
			"count": strconv.Itoa(msg.count), "total": strconv.Itoa(msg.total)}))
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
	if skip := renderSkippedDubs(msg.report); skip != "" {
		fmt.Fprintf(&b, "\n%s", skip)
	}
	if skip := renderSkippedAudioSet(msg.report); skip != "" {
		fmt.Fprintf(&b, "\n%s", skip)
	}
	for _, r := range msg.report {
		if r.SkipDub != "" || r.SkipAudioSet {
			continue // the compact skip lines already carry them
		}
		dropped := renderDroppedSuffix(r.DroppedAudio)
		synced := renderSyncSuffix(r.SyncNotes)
		b.WriteString("\n")
		switch {
		case r.Err != nil && r.Dub != "":
			fmt.Fprintf(&b, "%s", i18n.T("download.row_error", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub, "err": fmt.Sprint(r.Err)}))
			if synced != "" {
				// A failed row still renders what the analysis measured
				// (CodeRabbit #6: failed rows must not hide applied
				// offsets).
				fmt.Fprintf(&b, " (%s)", synced)
			}
		case r.Err != nil:
			fmt.Fprintf(&b, "%s", i18n.T("download.row_no_dubs", i18n.Vals{"ep": r.Episode}))
		case r.Path != "":
			fmt.Fprintf(&b, "%s", i18n.T("download.row_path", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub, "path": r.Path}))
			if dropped != "" {
				fmt.Fprintf(&b, " (%s)", dropped)
			}
			if synced != "" {
				fmt.Fprintf(&b, " (%s)", synced)
			}
		default:
			fmt.Fprintf(&b, "%s", i18n.T("download.row_done", i18n.Vals{
				"ep": r.Episode, "dub": r.Dub}))
			if dropped != "" {
				fmt.Fprintf(&b, " (%s)", dropped)
			}
			if synced != "" {
				fmt.Fprintf(&b, " (%s)", synced)
			}
		}
	}
	return b.String()
}

// renderBackgroundQueued composes the background settle line: the
// queued headline plus the per-episode dub typing, with the #164
// skips on the compact attribution line.
func renderBackgroundQueued(msg backgroundQueuedMsg) string {
	var b strings.Builder
	if msg.queued == msg.total {
		fmt.Fprintf(&b, "%s", i18n.T("download.bg_queued_all", i18n.Vals{"count": strconv.Itoa(msg.queued)}))
	} else {
		fmt.Fprintf(&b, "%s", i18n.T("download.bg_queued_partial", i18n.Vals{
			"count": strconv.Itoa(msg.queued), "total": strconv.Itoa(msg.total)}))
	}
	if skip := renderSkippedDubs(msg.report); skip != "" {
		fmt.Fprintf(&b, "\n%s", skip)
	}
	if skip := renderSkippedAudioSet(msg.report); skip != "" {
		fmt.Fprintf(&b, "\n%s", skip)
	}
	for _, r := range msg.report {
		if r.SkipDub != "" || r.SkipAudioSet {
			continue // the compact skip lines already carry them
		}
		b.WriteString("\n")
		if r.Err != nil {
			fmt.Fprintf(&b, "%s", i18n.T("download.row_no_dubs", i18n.Vals{"ep": r.Episode}))
			continue
		}
		fmt.Fprintf(&b, "%s", i18n.T("download.row_background", i18n.Vals{
			"ep": r.Episode, "dub": r.Dub}))
		if dropped := renderDroppedSuffix(r.DroppedAudio); dropped != "" {
			fmt.Fprintf(&b, " (%s)", dropped)
		}
	}
	return b.String()
}
