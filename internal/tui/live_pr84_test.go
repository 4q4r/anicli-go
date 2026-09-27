//go:build live

package tui

// PR84 live probe: the ⏭ Next auto-launch on a real two-episode
// title. Captures the DISTINCT loading surface frame during the real
// scoped resolve (never the picker's title), lets the settle launch
// mpv, and pastes the frames.
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//	  go test -tags live -run TestLivePR84 -count=1 -v ./internal/tui/

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"time"
)

func TestLivePR84NextEpisodeLoadingSurface(t *testing.T) {
	_, deps := liveDeps(t)
	// PR84 rule: the harness controls ONLY the children it spawned.
	// The playback rides the PID-scoped wrapper — real mpv, retained
	// handle, bounded lifetime — never the process table.
	player := newPidsafePlayer()
	t.Cleanup(player.StopAll)
	deps.Playback = player

	// 1. Fan-out search; pick a source whose top result merges into a
	// 2+ episode order with one dub present on the first two episodes.
	bySource := liveSearch(t, deps, "frieren")
	if len(bySource) == 0 {
		t.Fatal("no provider answered the probe query")
	}

	// Probe preference: the sources that resolve reliably through the
	// proxy first (a CF-gated origin's window fluctuates; a closed window
	// would burn every attempt). Anything else remains a fallback.
	preferred := []string{"anilibria", "gogoanime", "anilib"}
	var primaries []contracts.SearchResult
	for _, want := range preferred {
		if p, ok := bySource[want]; ok {
			primaries = append(primaries, p)
		}
	}
	for _, p := range bySource {
		skip := false
		for _, want := range preferred {
			if p.SourceID == want {
				skip = true
				break
			}
		}
		if !skip {
			primaries = append(primaries, p)
		}
	}

	var session *sessionScreen
	var dub string
	for _, primary := range primaries {
		group := []contracts.SearchResult{primary}
		s := NewSessionScreen(deps, primary, group)
		s.loadEpisodesSync()
		if len(s.order) < 2 {
			t.Logf("  %s → %d episodes, need 2+ — trying next source", primary.SourceID, len(s.order))
			continue
		}
		dubs := map[string]int{}
		for idx := range []int{0, 1} {
			s.currentIdx = idx
			ep := s.currentEpisodeData()
			if ep == nil {
				break
			}
			if len(ep.RawEmbeds) == 0 {
				if cmd := s.hydrateEpisode(ep.Num); cmd != nil {
					if done, ok := cmd().(hydrateDoneMsg); ok {
						s.Update(done)
					}
				}
				ep = s.currentEpisodeData()
				if ep == nil {
					break
				}
			}
			for key, links := range ep.RawEmbeds {
				dubs[key] += len(links)
			}
		}
		// The dub with the most links across both episodes — a junk
		// «Unknown» key with one link loses to a real dub.
		for key, n := range dubs {
			if n >= 2 && (dub == "" || n > dubs[dub]) {
				dub = key
			}
		}
		if dub == "" {
			t.Logf("  %s → no dub on both first episodes — trying next source", primary.SourceID)
			continue
		}
		session = s
		break
	}
	if session == nil {
		t.Fatal("no source offered a 2-episode title with a shared dub")
	}

	// 2. Remember the pair (the PR63 shape) and jump to the second
	// episode — the ⏭ Next position — from the episode list.
	session.videoDub, session.audioDub = dub, dub
	for i, candidate := range session.order {
		if candidate == session.order[1] {
			session.currentIdx = i
			break
		}
	}
	session.setState(sessionStateEpisodeList)

	scr, cmd := session.autoWatchNext()
	ss, ok := scr.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T", scr)
	}
	session = ss

	// 3. ⏭ Next with the real command pump: hydration (when tier-1)
	// then the scoped resolve — the state can pass through several
	// steps inside one Update chain, so the loading frame is sampled
	// after EVERY step.
	sawLoading := false
	loadingFrame := ""
	sample := func() {
		frame := session.View().Content
		if session.state == sessionStateResolveLoading {
			sawLoading = true
			loadingFrame = frame
		}
	}
	sample()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		sample()
		if session.state == sessionStatePlaying || session.state == sessionStateBuffering {
			break
		}
		if cmd == nil {
			// A settle landed back on a menu/list state: continue the
			// ⏭ Next flow with a fresh autoWatchNext.
			var scr Screen
			scr, cmd = session.autoWatchNext()
			if s, ok := scr.(*sessionScreen); ok {
				session = s
			}
			sample()
		}
		if cmd == nil {
			break
		}
		msg := cmd()
		if msg == nil {
			break
		}
		next, follow := session.Update(msg)
		ns, ok := next.(*sessionScreen)
		if !ok {
			t.Fatalf("update scr = %T", next)
		}
		session = ns
		cmd = follow
	}
	sample()
	if !sawLoading {
		t.Fatalf("the loading surface never rendered (final state=%v)", session.state)
	}
	if strings.Contains(loadingFrame, "Pick a stream") || strings.Contains(loadingFrame, "Looking for streams…") {
		t.Errorf("the picker flashed during the resolve:\n%s", loadingFrame)
	}
	if !strings.Contains(loadingFrame, "Ep. "+session.resolveEp) || !strings.Contains(loadingFrame, session.resolveDub) {
		t.Errorf("the loading frame must carry the ep/dub context:\n%s", loadingFrame)
	}
	t.Logf("LOADING FRAME:\n%s", loadingFrame)

	// 4. The settle launches playback through the PID-scoped wrapper.
	// A resolve error (flaky exits/windows) retries the whole ⏭
	// След. flow — bounded attempts, total time bounded by deadline.
	// The playedMsg wait is NOT executed: mpv exiting is the owner
	// territory; the harness terminates only its own child
	// (player.StopAll — retained handles) at the end.
	for attempts := 1; ; attempts++ {
		if session.state != sessionStatePlaying {
			if attempts >= 3 {
				t.Fatalf("no launch after %d attempts (state=%v status=%q)",
					attempts, session.state, session.status)
			}
			scr, cmd = session.autoWatchNext()
			if s, ok := scr.(*sessionScreen); ok {
				session = s
			}
			sample()
			for cmd != nil && session.state != sessionStatePlaying && time.Now().Before(deadline) {
				msg := cmd()
				if msg == nil {
					break
				}
				next, follow := session.Update(msg)
				ns, ok := next.(*sessionScreen)
				if !ok {
					t.Fatalf("update scr = %T", next)
				}
				session = ns
				cmd = follow
				sample()
				if session.state == sessionStateResolveLoading {
					loadingFrame = session.View().Content
				}
				if session.state == sessionStatePlaying || session.state == sessionStateQuality {
					break
				}
			}
		}
		if cmd == nil {
			break
		}
		msg := cmd()
		if msg == nil {
			break
		}
		if settle, ok := msg.(streamResolvedMsg); ok && settle.err != nil {
			// Resolve failed (window/exit flake): retry.
			cmd = nil
			continue
		}
		next, play := session.Update(msg)
		ns, ok := next.(*sessionScreen)
		if !ok {
			t.Fatalf("settle scr = %T", next)
		}
		session = ns
		if session.state == sessionStatePlaying {
			break
		}
		cmd = play
		if cmd == nil {
			break
		}
	}
	t.Logf("PLAYING status: %q", session.status)
	// The launch is proven by the PID-scoped wrapper: the real mpv was
	// spawned as the harness's OWN child (handle retained) and the
	// bounded lifetime synthesized its exit — headless mpv does not
	// exit on its own, so the lifetime deadline is the expected shape.
	// The owner's processes are unreachable by construction.
	if player.Plays() != 1 {
		t.Errorf("plays = %d, want exactly one PID-tracked launch", player.Plays())
	}
	player.StopAll()
}
