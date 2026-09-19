package tui

// Download-flow machinery (PR64): the availability line of the
// download-range prompt and the per-episode dub resolution shared by
// the foreground and background range downloads (the PR63 watch-flow
// semantics — python resolve_dubs_smart).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// describeAvailableEpisodes renders the compact availability line of
// the download-range prompt (PR64 #2): the count plus the real
// available set — consecutive episodes collapsed into runs, gaps and
// non-numeric labels listed as-is, so the user never guesses what
// exists before typing a range. The caller's order is preserved —
// the session passes its merged (display) order.
func describeAvailableEpisodes(order []string) string {
	if len(order) == 0 {
		return "Доступных серий нет"
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
	return fmt.Sprintf("Доступно серий: %d (%s)", len(order), strings.Join(runs, ", "))
}

// downloadResolveTimeout bounds ONE dub liveness probe of the
// download resolution (the watch-flow lookup budget class).
const downloadResolveTimeout = lookupTimeout

// errNoViableDub types the verdict of a range episode whose every dub
// is dead (PR64 #3): the report names the episode instead of silently
// skipping it or aborting the whole range.
var errNoViableDub = fmt.Errorf("нет доступных озвучек с потоками")

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
// prompt use. Lazily-listing providers hydrate first (the same
// on-demand round the watch flow runs). Returns "" when no dub is
// viable — the caller types the failure instead of skipping silently.
//
// The machinery is the watch flow's own: liveness = a scoped
// resolveAllStreams success, hydration = hydrateEpisodeCmd.
func resolveDownloadDub(ctx context.Context, deps *Deps, ep contracts.Episode, preferred string) string {
	ep = hydrateForDownload(ctx, deps, ep)
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
		if entries, err := resolveAllStreams(ctx, deps.Episode, ep, cand); err == nil && len(entries) > 0 {
			return cand
		}
	}
	return ""
}

// hydrateForDownload runs the watch flow's hydration round over one
// range episode (the PR43 on-demand model — range episodes other than
// the opened one are usually unhydrated) and merges the
// provider-prefixed embeds into the episode copy. Providers already
// carrying real links are skipped by hydrateEpisodeCmd itself.
func hydrateForDownload(ctx context.Context, deps *Deps, ep contracts.Episode) contracts.Episode {
	msg := hydrateEpisodeCmd(deps, ep.Num, ep, 0, ctx)
	if len(msg.embeds) == 0 {
		return ep
	}
	if ep.RawEmbeds == nil {
		ep.RawEmbeds = map[string][]string{}
	}
	for dub, links := range msg.embeds {
		ep.RawEmbeds[dub] = links
	}
	return ep
}

// stampResolvedDub pins the per-episode resolution onto the task.
func stampResolvedDub(task *DownloadTask, dub string) {
	task.DubID = dub
	task.ProviderID = providerOfTrackKey(dub)
}

// runForegroundDownload resolves each range episode's dub (smart) and
// downloads the batch sequentially, typing every episode's verdict in
// the report. A dead episode never aborts its siblings.
func runForegroundDownload(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string) downloadSettledMsg {
	report := make([]downloadEpisodeReport, 0, len(tasks))
	ok, firstErr := 0, error(nil)
	for _, task := range tasks {
		rctx, cancel := context.WithTimeout(ctx, downloadResolveTimeout)
		dub := resolveDownloadDub(rctx, deps, task.Episode, preferred)
		cancel()
		if dub == "" {
			report = append(report, downloadEpisodeReport{Episode: task.EpisodeNum, Err: errNoViableDub})
			if firstErr == nil {
				firstErr = errNoViableDub
			}
			continue
		}
		stampResolvedDub(&task, dub)
		path, err := deps.Download.Download(ctx, task)
		report = append(report, downloadEpisodeReport{Episode: task.EpisodeNum, Dub: dub, Path: path, Err: err})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
	}
	return downloadSettledMsg{count: ok, total: len(tasks), err: firstErr, report: report}
}

// queueBackgroundDownloads resolves each range episode's dub (smart)
// BEFORE queueing, so the background manager never receives a task
// whose dub is dead on its episode. The settle types the verdicts.
func queueBackgroundDownloads(ctx context.Context, deps *Deps, tasks []DownloadTask, preferred string) backgroundQueuedMsg {
	report := make([]downloadEpisodeReport, 0, len(tasks))
	queued := 0
	for _, task := range tasks {
		rctx, cancel := context.WithTimeout(ctx, downloadResolveTimeout)
		dub := resolveDownloadDub(rctx, deps, task.Episode, preferred)
		cancel()
		if dub == "" {
			report = append(report, downloadEpisodeReport{Episode: task.EpisodeNum, Err: errNoViableDub})
			continue
		}
		stampResolvedDub(&task, dub)
		deps.Download.Submit(task)
		queued++
		report = append(report, downloadEpisodeReport{Episode: task.EpisodeNum, Dub: dub})
	}
	return backgroundQueuedMsg{queued: queued, total: len(tasks), report: report}
}

// renderDownloadSettle composes the foreground settle line: the
// headline (the vocabulary the earlier PRs pinned) plus the compact
// per-episode report.
func renderDownloadSettle(msg downloadSettledMsg) string {
	var b strings.Builder
	switch {
	case msg.err == nil:
		fmt.Fprintf(&b, "✓ Загружено серий: %d", msg.count)
	case msg.count > 0:
		fmt.Fprintf(&b, "⚠ Загружено серий: %d из %d", msg.count, msg.total)
	default:
		fmt.Fprintf(&b, "Ошибка загрузки: 0 из %d серий", msg.total)
	}
	for _, r := range msg.report {
		b.WriteString("\n")
		switch {
		case r.Err != nil && r.Dub != "":
			fmt.Fprintf(&b, "Серия %s — %s — ошибка: %v", r.Episode, r.Dub, r.Err)
		case r.Err != nil:
			fmt.Fprintf(&b, "Серия %s — ✗ нет доступных озвучек", r.Episode)
		case r.Path != "":
			fmt.Fprintf(&b, "Серия %s — %s — %s", r.Episode, r.Dub, r.Path)
		default:
			fmt.Fprintf(&b, "Серия %s — %s — готово", r.Episode, r.Dub)
		}
	}
	return b.String()
}

// renderBackgroundQueued composes the background settle line: the
// queued headline plus the per-episode dub typing.
func renderBackgroundQueued(msg backgroundQueuedMsg) string {
	var b strings.Builder
	if msg.queued == msg.total {
		fmt.Fprintf(&b, "Отправлено в фон: %d серий", msg.queued)
	} else {
		fmt.Fprintf(&b, "Отправлено в фон: %d из %d серий", msg.queued, msg.total)
	}
	for _, r := range msg.report {
		b.WriteString("\n")
		if r.Err != nil {
			fmt.Fprintf(&b, "Серия %s — ✗ нет доступных озвучек", r.Episode)
			continue
		}
		fmt.Fprintf(&b, "Серия %s — %s — фон", r.Episode, r.Dub)
	}
	return b.String()
}
