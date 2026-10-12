package tui

// #166 (owner report, live): «Сменить озвучку» re-resolves and
// RESTARTS THE EPISODE FROM ZERO — the requirement is that changing
// the dub continues from the current playback position («требовалось
// просто озвучку поменять»). The pick captures the final position the
// player tracked over IPC for the dub being replaced and the relaunch
// carries it as StartPosition (→ mpv --start); without a captured
// position (redub before any playback, IPC degraded) or with a
// position belonging to another episode, the relaunch starts clean.

import (
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// redubResumeSession is the redubSession fixture plus a playback
// history: the remembered dub [animego] Дубль 1 is currently playing
// (its 1080 URL), so the redub pick has a position to resume from.
func redubResumeSession(t *testing.T, pb *fakePlayback) *sessionScreen {
	t.Helper()
	s := redubSession(t)
	s.deps.Playback = pb
	s.pickedVideo = contracts.VideoSource{URL: "v1080"}
	return s
}

// driveRedubPick walks the PR95 flow: «Сменить озвучку» → fresh
// resolve settle → the AniLib pick → the auto-launch settle. Returns
// the screen past the launch and the playedMsg command.
func driveRedubPick(t *testing.T, s *sessionScreen) (*sessionScreen, tea.Cmd) {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "redub"))
	ss := settleRedubResolve(t, s)
	for i, c := range ss.redubList.Menu().Items {
		if c.ID == "[anilib] AniLib" {
			ss.redubList.Jump(i)
			break
		}
	}
	next, resolve := ss.Update(enter())
	ss2 := next.(*sessionScreen)
	if resolve == nil {
		t.Fatal("the pick must schedule a fresh scoped resolve")
	}
	next3, play := ss2.Update(resolve())
	ss3 := next3.(*sessionScreen)
	if ss3.state != sessionStatePlaying {
		t.Fatalf("state = %v, want sessionStatePlaying", ss3.state)
	}
	return ss3, play
}

// TestRedubPickPassesCapturedPosition (#166): the redub relaunch must
// carry the captured playback position of the dub being replaced.
func TestRedubPickPassesCapturedPosition(t *testing.T) {
	pb := &fakePlayback{lastURL: "v1080", lastPos: 187.5, lastOK: true}
	s := redubResumeSession(t, pb)

	_, play := driveRedubPick(t, s)
	if pm, ok := play().(playedMsg); !ok || pm.err != nil {
		t.Fatalf("playback must succeed, got %T %v", play(), pm.err)
	}
	got := s.deps.Playback.(*fakePlayback).played
	if len(got) != 1 {
		t.Fatalf("played = %d requests, want 1", len(got))
	}
	if got[0].URL != "a1080" {
		t.Fatalf("played URL = %q, want the picked dub's stream a1080", got[0].URL)
	}
	if got[0].StartPosition != 187.5 {
		t.Fatalf("redub relaunch StartPosition = %v, want 187.5 (the captured position)", got[0].StartPosition)
	}
}

// TestRedubWithoutPositionLaunchesFromZero (#166): redub before any
// playback (or with the IPC tracking degraded) must launch WITHOUT a
// start position — the redub itself must never fail or block.
func TestRedubWithoutPositionLaunchesFromZero(t *testing.T) {
	pb := &fakePlayback{}
	s := redubResumeSession(t, pb)

	_, play := driveRedubPick(t, s)
	if pm, ok := play().(playedMsg); !ok || pm.err != nil {
		t.Fatalf("playback must succeed, got %T %v", play(), pm.err)
	}
	got := s.deps.Playback.(*fakePlayback).played
	if len(got) != 1 || got[0].URL != "a1080" {
		t.Fatalf("played = %+v, want the picked dub's stream", got)
	}
	if got[0].StartPosition != 0 {
		t.Fatalf("no captured position must mean StartPosition 0, got %v", got[0].StartPosition)
	}
}

// TestRedubForeignPositionIgnored (#166): a tracked position that
// belongs to another episode's launch (the user navigated without a
// successful playback in between) must not resume onto the current
// episode — only the CURRENT dub's position counts.
func TestRedubForeignPositionIgnored(t *testing.T) {
	pb := &fakePlayback{lastURL: "u-some-other-episode", lastPos: 999, lastOK: true}
	s := redubResumeSession(t, pb)

	_, play := driveRedubPick(t, s)
	if pm, ok := play().(playedMsg); !ok || pm.err != nil {
		t.Fatalf("playback must succeed, got %T %v", play(), pm.err)
	}
	got := s.deps.Playback.(*fakePlayback).played
	if len(got) != 1 || got[0].StartPosition != 0 {
		t.Fatalf("a foreign position must not resume: %+v", got)
	}
}
