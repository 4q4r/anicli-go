package tui

// PR96 defensive hardening of the redub paths: a nil redubList, an
// empty settle carrier and a dub key with no resolved streams must
// degrade to clean no-ops with status notes (or the unscoped resolve
// fallback) — never a panic. The owner hit the recovered
// «invalid memory address … source: session» on this screen family.

import (
	"strings"
	"testing"
)

// TestPR96RedubNilListKeyRebuilds: state Redub with a nil redubList
// (a future clear-then-build interleaving) must rebuild the menu from
// the session entries instead of nil-dereferencing PinList.
func TestPR96RedubNilListKeyRebuilds(t *testing.T) {
	s := redubSession(t)
	s.redubList = nil
	s.setState(sessionStateRedub)

	next, _ := s.Update(enter())
	ss, ok := next.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T", next)
	}
	if ss.redubList == nil {
		t.Fatal("handleRedubKey must rebuild a nil redubList")
	}
	if ss.state != sessionStateRedub {
		t.Fatalf("state = %v, want sessionStateRedub", ss.state)
	}
}

// TestPR96RedubViewNilListNoPanic: rendering the Redub surface with a
// nil redubList stays a legal no-op (themedList nil-guard pin).
func TestPR96RedubViewNilListNoPanic(t *testing.T) {
	s := redubSession(t)
	s.redubList = nil
	s.setState(sessionStateRedub)
	_ = s.View().Content // must not panic
}

// TestPR96RedubSettleEmptyEntriesNoPanic: the redubPending settle with
// a nil/empty carrier opens the I3 empty dub menu (Back alone + the
// empty-state message) and stamps an honest status note — a clean
// no-op, never a panic and never an auto-launch.
func TestPR96RedubSettleEmptyEntriesNoPanic(t *testing.T) {
	s := redubSession(t)
	s.streamEntries = nil
	s.redubPending = true
	gen := s.resolveGen

	next, _ := s.Update(streamResolvedMsg{gen: gen, scope: "", entries: nil})
	ss, ok := next.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T", next)
	}
	if ss.state != sessionStateRedub {
		t.Fatalf("state = %v, want sessionStateRedub (clean no-op menu)", ss.state)
	}
	if got := len(ss.redubList.Menu().Items); got != 1 {
		t.Fatalf("empty settle must build the Back-only menu, got %d rows", got)
	}
	if !ss.statusVisible() || !strings.Contains(ss.status, "No dubs found") {
		t.Fatalf("empty settle must stamp the status note, got %q (visible=%v)",
			ss.status, ss.statusVisible())
	}
}

// TestPR96ScopedSettleEmptyEntriesFallsBackToUnscoped: the
// remembered-dub fast path (PR94 spirit) with an EMPTY verdict must
// not auto-launch a zero-URL play — it falls through to the full
// merged resolve of the rest, exactly like the scoped-error case.
func TestPR96ScopedSettleEmptyEntriesFallsBackToUnscoped(t *testing.T) {
	s := redubSession(t)
	gen := s.resolveGen

	next, _ := s.Update(streamResolvedMsg{gen: gen, scope: "[animego] Дубль 1", entries: nil})
	ss, ok := next.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T", next)
	}
	if ss.state != sessionStateQuality {
		t.Fatalf("state = %v, want the unscoped picker surface (sessionStateQuality)", ss.state)
	}
	if ss.resolveGen != gen+1 {
		t.Fatalf("resolveGen = %d, want a fresh round (%d)", ss.resolveGen, gen+1)
	}
	if v := ss.View().Content; !strings.Contains(v, "Looking for streams") {
		t.Fatalf("unscoped fallback must render the resolving surface:\n%s", v)
	}
}

// TestPR96PickRedubUnknownDubFallsBackToResolve: a dub key mapping to
// no resolved entries keeps the PR95 fallback — the scoped resolve —
// without launching anything (existing behavior pin, PR96 audit).
func TestPR96PickRedubUnknownDubFallsBackToResolve(t *testing.T) {
	s := redubSession(t)
	next, _ := s.pickRedub("[kodik] Неизвестный дубль")
	ss, ok := next.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T", next)
	}
	if ss.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want the scoped loading surface", ss.state)
	}
	if ss.videoDub != "[kodik] Неизвестный дубль" {
		t.Fatalf("dubs must retarget to the pick, got %q", ss.videoDub)
	}
}
