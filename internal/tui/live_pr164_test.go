//go:build live

package tui

// LIVE probe for PR164 (the owner-reported download bug): «скачать
// все серии» without a chosen dub used to silently download an
// arbitrary dub — the first lexicographic one with live streams. The
// ask-first doctrine now gates the batch: the dub menu opens BEFORE
// anything downloads, the pick drives every episode, cancel aborts.
//
// Run manually:
//
//	go test -tags live -run TestLivePR164DownloadAskFirst -count=1 -v ./internal/tui/
//
// Real providers, real episode lists, real stream resolution. The
// ffmpeg/download step itself is replaced by a recording wrapper (the
// same honesty rule as the PR64 probe): the probe proves the FLOW
// (menu → pick → batch) and the RESOLUTION (every recorded task
// carries exactly the dub picked in the menu) without writing
// hundreds of megabytes into the owner's download directory.

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestLivePR164DownloadAskFirst: the owner's scenario on a real
// title. A fresh session (no remembered dub, no playback) enters the
// download flow: the mode pick must NOT start a batch — the dub menu
// opens; Esc aborts with nothing recorded; a pick dispatches a batch
// whose every task carries exactly the picked dub.
func TestLivePR164DownloadAskFirst(t *testing.T) {
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

	s := NewSessionScreen(deps, primary, g)
	s.loadEpisodesSync()
	t.Logf("merged episodes: %v", s.order)
	if len(s.order) < 2 {
		t.Fatalf("need a 2+ episode title, got %v", s.order)
	}
	if s.videoDub != "" {
		t.Fatalf("precondition: a fresh session must have no remembered dub, got %q", s.videoDub)
	}

	// Force the variance case on live data BEFORE the flow opens: the
	// dub we will pick disappears from ep2's listing embeds (an
	// in-memory mutation — the same honesty rule as the PR64 probe).
	// The strict resolve then either skips ep2 (fresh hydration keeps
	// the deletion) or proves the dub carried and downloads it — with
	// the picked dub either way, never a substitute.
	picked := firstAliveDub(t, s, s.order[0])
	if picked == "" {
		t.Fatalf("no alive dub on ep %q", s.order[0])
	}
	ep2 := s.episodes[s.order[1]]
	delete(ep2.RawEmbeds, picked)
	s.episodes[s.order[1]] = ep2
	t.Logf("will pick %q; removed it from ep %q's listing embeds", picked, s.order[1])

	openFlow := func() *sessionScreen {
		t.Helper()
		s.list.Jump(sessionActionIndex(s, "download"))
		next, _ := s.Update(enter())
		ss := next.(*sessionScreen)
		if ss.state != sessionStateDownloadRange {
			t.Fatalf("download must open the range prompt, got %v", ss.state)
		}
		ss.rangeInput.typeText(s.order[0] + "-" + s.order[1])
		next, _ = ss.Update(enter())
		ss = next.(*sessionScreen)
		if ss.state != sessionStateDownloadContent {
			t.Fatalf("after the range the content-mode menu opens, got %v", ss.state)
		}
		next, _ = ss.Update(enter()) // видео+аудио, the default first row
		ss = next.(*sessionScreen)
		if ss.state != sessionStateDownloadMode {
			t.Fatalf("the content pick must open the mode menu, got %v", ss.state)
		}
		next, _ = ss.Update(enter()) // «Передний план»
		return next.(*sessionScreen)
	}

	// --- Proof 1: no dub chosen → the menu opens, nothing downloads.
	ss := openFlow()
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("no dub chosen must open the ask-first dub menu, got %v", ss.state)
	}
	if len(rec.tasks) != 0 {
		t.Fatalf("nothing may download before the pick, got %+v", rec.tasks)
	}
	view := ss.View().Content
	t.Logf("ASK-FIRST MENU VIEW:\n%s", view)
	if !contains(view, "Dub to download:") {
		t.Fatalf("the ask-first dub menu must render:\n%s", view)
	}

	// --- Proof 2: Esc (cancel) aborts — nothing recorded.
	next, _ := ss.Update(esc())
	ss = next.(*sessionScreen)
	if ss.state != sessionStateMenu {
		t.Fatalf("cancel must return to the session menu, got %v", ss.state)
	}
	if len(rec.tasks) != 0 {
		t.Fatalf("cancel must record nothing, got %+v", rec.tasks)
	}
	t.Logf("CANCEL PROOF: back at the menu, %d tasks recorded", len(rec.tasks))

	// --- Proof 3: the pick drives the whole batch.
	ss = openFlow()
	if ss.state != sessionStateDownloadDub {
		t.Fatalf("the ask-first menu must reopen for the second pass, got %v", ss.state)
	}
	idx := -1
	for i, item := range ss.redubList.Menu().Items {
		if item.ID == picked {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("the menu must list the picked dub %q:\n%s", picked, ss.View().Content)
	}
	ss.redubList.Jump(idx)
	next, cmd := ss.Update(enter())
	// PR165b: the default «ask» sync gate turns the dub pick into the
	// sync prompt first; «Нет» dispatches the batch unsynchronized —
	// the pre-gate semantics this probe pins.
	if cmd == nil {
		ask, ok := next.(*sessionScreen)
		if !ok || ask.state != sessionStateSyncAsk {
			t.Fatalf("the dub pick must dispatch the batch or open the sync ask, got %T", next)
		}
		ask.syncAskList.Jump(1) // «Нет» — the batch runs unsynchronized
		next, cmd = ask.Update(enter())
	}
	if cmd == nil {
		t.Fatal("the dub pick must dispatch the batch")
	}
	var settled downloadSettledMsg
	for _, m := range runLaunchBatch(t, cmd) {
		if d, ok := m.(downloadSettledMsg); ok {
			settled = d
		}
	}
	next, _ = ss.Update(settled)
	ss = next.(*sessionScreen)
	t.Logf("SETTLE REPORT:\n%s", ss.status)

	if len(rec.tasks) == 0 {
		t.Fatalf("the pick must download the carrying episodes, report:\n%s", ss.status)
	}
	if settled.count != len(rec.tasks) {
		t.Fatalf("settled count %d must match the recorded tasks %d", settled.count, len(rec.tasks))
	}
	for _, task := range rec.tasks {
		if task.DubID != picked {
			t.Fatalf("ep %s must carry the PICKED dub %q, got %q — a substitute",
				task.EpisodeNum, picked, task.DubID)
		}
		if task.ProviderID != providerOfTrackKey(picked) {
			t.Fatalf("ep %s provider must match the picked dub, got %q", task.EpisodeNum, task.ProviderID)
		}
		t.Logf("LIVE RESOLUTION: ep %s -> %s [%s] (the dub picked in the menu)",
			task.EpisodeNum, task.DubID, task.ProviderID)
	}
	t.Logf("ASK-FIRST PROOF: menu → pick %q → %d episode(s) downloaded, 0 substitutes", picked, len(rec.tasks))
}
