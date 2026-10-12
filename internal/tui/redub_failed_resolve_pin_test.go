package tui

// Review M2 characterization pin (#166): a redub pick captured a
// position (100s, ep1) and the redub RESOLVE failed after the pick
// (the scoped dub's stream resolve dies with a transport-class error
// → the PR94 merged fallback → the quality picker, no launch). The
// pick's capture is a one-shot snapshot on the session, so the next
// launch OF THE SAME EPISODE (a plain quality pick from the picker)
// carries it as StartPosition. Reviewed M2: acceptable, self-healing —
// the snapshot is consumed by ANY launch, the episode guard keeps it
// from ever reaching another episode, and mpv's own watch-later makes
// the value redundant-but-consistent. The pin freezes exactly this
// behavior so a future change is a conscious decision, not drift.
//
// The one-shot half is pinned too: the launch consumed the snapshot,
// so the session is clean afterwards.

import (
	"errors"
	"testing"
)

func TestRedubFailedResolveSelfHealsOnNextLaunch(t *testing.T) {
	pb := &fakePlayback{lastURL: "v1080", lastPos: 100, lastOK: true}
	s := redubResumeSession(t, pb)

	// The redub menu opens from the healthy fresh round (both dubs
	// alive); the dub's stream resolve dies only AFTER the pick —
	// the transport-class failure the fallback path exists for.
	s.list.Jump(sessionActionIndex(s, "redub"))
	ss := settleRedubResolve(t, s)
	s.deps.Episode.(*fakeEpisode).streamErr = map[string]error{
		"[anilib] AniLib": errors.New("dead"),
	}

	var anilibIdx = -1
	for i, c := range ss.redubList.Menu().Items {
		if c.ID == "[anilib] AniLib" {
			anilibIdx = i
			break
		}
	}
	if anilibIdx < 0 {
		t.Fatalf("the anilib dub is missing from the menu: %v", ss.redubList.Menu().Items)
	}
	ss.redubList.Jump(anilibIdx)

	// The pick captures (100s, ep1) and schedules the scoped resolve;
	// its error verdict falls to the PR94 merged fallback.
	next, resolve := ss.Update(enter())
	ss2 := next.(*sessionScreen)
	if resolve == nil {
		t.Fatal("the pick must schedule the scoped resolve")
	}
	next2, fallback := ss2.Update(resolve())
	ss3 := next2.(*sessionScreen)
	if fallback == nil {
		t.Fatalf("the failed scoped resolve must schedule the merged fallback, got state=%v status=%q",
			ss3.state, ss3.status)
	}
	// The merged verdict (animego alive, anilib skipped fail-soft)
	// builds the quality picker — no launch happened yet.
	next3, _ := ss3.Update(fallback())
	ss4 := next3.(*sessionScreen)
	if ss4.state != sessionStateQuality {
		t.Fatalf("state after the fallback settle = %v (status %q), want sessionStateQuality",
			ss4.state, ss4.status)
	}
	if len(pb.played) != 0 {
		t.Fatalf("a failed redub resolve must not launch, played %+v", pb.played)
	}

	// A NORMAL launch from here (plain quality pick, no redub in the
	// flow) still carries the captured position — same episode. The
	// recovery models the real fallback: the user returns to the
	// healthy dub pair they had before the failed redub (the audio
	// dub rides along; every picker row is the surviving animego
	// dub, so the pair is uniform again).
	ss4.audioDub = "[animego] Дубль 1"
	next4, play := ss4.Update(enter())
	ss5 := next4.(*sessionScreen)
	if ss5.state != sessionStatePlaying {
		t.Fatalf("state = %v, want sessionStatePlaying", ss5.state)
	}
	if pm, ok := play().(playedMsg); !ok || pm.err != nil {
		t.Fatalf("playback must succeed, got %T %v", play(), pm.err)
	}
	got := pb.played
	if len(got) != 1 {
		t.Fatalf("played = %d requests, want 1", len(got))
	}
	if got[0].StartPosition != 100 {
		t.Fatalf("self-healing launch StartPosition = %v, want 100 (the reviewed M2 snapshot)", got[0].StartPosition)
	}

	// One-shot: the launch consumed the snapshot.
	if ss5.redubStart != 0 || ss5.redubStartEp != "" {
		t.Fatalf("the snapshot must be consumed: redubStart=%v redubStartEp=%q", ss5.redubStart, ss5.redubStartEp)
	}
}
