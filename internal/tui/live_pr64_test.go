//go:build live

package tui

// LIVE probes for PR64 (download UX: status scoping, prompt
// availability line, per-episode hybrid dub resolution). Excluded
// from the hermetic default suite by the `live` build tag. Run
// manually:
//
//	go test -tags live -run TestLivePR64 -count=1 -v ./internal/tui/
//
// Real providers, real episode lists, real stream resolution, REAL
// on-demand hydration of the range episodes. The ffmpeg/download step
// itself is replaced by a recording wrapper — the probe proves the
// RESOLUTION half of PR64 #3 against live data (which dub each
// episode of the range actually resolves to) without writing
// hundreds of megabytes into the owner's download directory; the
// download half is unchanged code covered by the hermetic suite.
// The mixed-availability rotation is forced by mutating the in-memory
// aggregate (a live provider cannot be forced to drop a dub) — the
// same honesty rule as the PR63 changed-dubs probe.

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// recordingDownload stands in for the real DownloadService during the
// live probe: it records the RESOLVED tasks (the probe's subject) and
// never runs ffmpeg.
type recordingDownload struct {
	tasks []DownloadTask
}

func (r *recordingDownload) Download(_ context.Context, task DownloadTask) (string, error) {
	r.tasks = append(r.tasks, task)
	return "/live-proof/EP_" + task.EpisodeNum + "_" + task.DubID + ".mp4", nil
}

func (r *recordingDownload) Submit(task DownloadTask) {
	r.tasks = append(r.tasks, task)
}

func (r *recordingDownload) ActiveBanner() string { return "" }

var _ DownloadService = (*recordingDownload)(nil)

// TestLivePR64DownloadPromptAndRangeResolution: on a real title the
// download prompt carries the availability line, and a range download
// resolves the dub PER EPISODE — the remembered dub where alive, the
// fallback dub on the rotated episode, every verdict typed in the
// report.
func TestLivePR64DownloadPromptAndRangeResolution(t *testing.T) {
	_, deps := liveDeps(t)
	rec := &recordingDownload{}
	deps.Download = rec

	group := liveSearch(t, deps, "черная лагуна")
	if len(group) < 1 {
		t.Fatalf("no live results")
	}
	g := make([]contracts.SearchResult, 0, len(group))
	for _, r := range group {
		g = append(g, r)
	}
	g = stableGroup(g)
	primary := g[0]
	t.Logf("primary source: %s/%s (%q)", primary.SourceID, primary.URL, primary.Title)

	deps.Download = rec
	s := NewSessionScreen(deps, primary, g)
	s.loadEpisodesSync()
	t.Logf("merged episodes: %v", s.order)
	if len(s.order) < 2 {
		t.Fatalf("need a 2+ episode title, got %v", s.order)
	}

	// --- Proof 2: the prompt text on a real title.
	s.list.Jump(sessionActionIndex(s, "download"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateDownloadRange {
		t.Fatalf("download must open the range prompt, got %v", ss.state)
	}
	prompt := ss.View().Content
	t.Logf("PROMPT VIEW:\n%s", prompt)
	if !contains(prompt, "Доступно серий: ") {
		t.Fatalf("the prompt must carry the availability line:\n%s", prompt)
	}

	// Force the mixed-availability case on live data: the remembered
	// dub disappears from ONE episode of the range (in-memory
	// mutation; a live provider cannot be forced to rotate).
	s.videoDub = firstAliveDub(t, s, s.order[0])
	if s.videoDub == "" {
		t.Fatalf("no alive dub on ep %q to remember", s.order[0])
	}
	t.Logf("remembered dub: %q", s.videoDub)
	rotated := s.order[1]
	ep := s.episodes[rotated]
	delete(ep.RawEmbeds, s.videoDub)
	s.episodes[rotated] = ep

	// --- Proof 3: the per-episode resolution over the live providers.
	ss.rangeInput.typeText(s.order[0] + "-" + rotated)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateDownloadMode {
		t.Fatalf("after the range the mode menu opens, got %v", ss.state)
	}
	next, cmd := ss.Update(enter()) // «Передний план»
	if cmd == nil {
		t.Fatalf("the foreground pick must dispatch")
	}
	// The batch arms the progress pump: collect the per-episode
	// ticks while the worker runs (the worker closes the channel).
	var lines []string
	var lineDone = make(chan struct{})
	go func() {
		defer close(lineDone)
		for line := range ss.downloadProgCh {
			lines = append(lines, line)
		}
	}()
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	<-lineDone
	next, _ = ss.Update(settled)
	ss = next.(*sessionScreen)
	for _, line := range lines {
		t.Logf("PROGRESS: %s", line)
	}
	t.Logf("SETTLE REPORT:\n%s", ss.status)

	if settled.count != 2 || settled.total != 2 {
		t.Fatalf("both range episodes must resolve and download, got %d/%d, report:\n%s",
			settled.count, settled.total, ss.status)
	}
	if len(lines) < 4 {
		t.Fatalf("per-episode progress lines expected (2 resolve + 2 download), got %v", lines)
	}
	if len(rec.tasks) != 2 {
		t.Fatalf("2 recorded tasks expected, got %d", len(rec.tasks))
	}
	// The rotated episode must NOT carry the remembered dub (it was
	// deleted from the aggregate before the range ran) — the report
	// must type the dub that ACTUALLY resolved for it.
	for _, task := range rec.tasks {
		if task.EpisodeNum != rotated {
			continue
		}
		if task.DubID == s.videoDub {
			t.Fatalf("the rotated episode must fall back, still using the remembered %q", s.videoDub)
		}
		if task.ProviderID != providerOfTrackKey(task.DubID) || task.ProviderID == "" {
			t.Fatalf("ep %s provider must match its dub, got %q for %q",
				task.EpisodeNum, task.ProviderID, task.DubID)
		}
		t.Logf("LIVE RESOLUTION: ep %s -> %s [%s] (remembered %q was rotated away)",
			task.EpisodeNum, task.DubID, task.ProviderID, s.videoDub)
	}
	if !contains(ss.status, "Серия "+rotated+" —") {
		t.Fatalf("the report must type the rotated episode's verdict:\n%s", ss.status)
	}
}

// firstAliveDub returns the first dub key of the episode that carries
// links (the established order).
func firstAliveDub(t *testing.T, s *sessionScreen, num string) string {
	t.Helper()
	ep, ok := s.episodes[num]
	if !ok {
		return ""
	}
	for _, k := range sortedEmbedKeys(ep.RawEmbeds) {
		if len(ep.RawEmbeds[k]) > 0 {
			return k
		}
	}
	return ""
}
