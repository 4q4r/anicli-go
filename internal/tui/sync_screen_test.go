package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// syncDeps builds deps with a wired SyncFull seam.
func syncDeps(result *shikimori.SyncResult, err error) *Deps {
	return &Deps{
		ShikiCfg: config.Shikimori{Enabled: true, Session: "s"},
		SyncFull: func(context.Context, func(shikimori.SyncProgress)) (*shikimori.SyncResult, error) {
			return result, err
		},
	}
}

// TestSyncScreenShowsProgressWhileRunning pins the running surface:
// the spinner label renders until the sync settles.
func TestSyncScreenShowsProgressWhileRunning(t *testing.T) {
	s := NewSyncScreen(syncDeps(nil, nil))
	view := s.View().Content
	if !contains(view, "Синхронизация с Shikimori") {
		t.Fatalf("running view must carry the sync label, got:\n%s", view)
	}
	if contains(view, "Синхронизировано") {
		t.Fatalf("running view must not show the summary yet, got:\n%s", view)
	}
}

// TestSyncScreenRunCommandSettles pins the sync command: it calls the
// injected seam once and settles a syncDoneMsg with its verdict.
func TestSyncScreenRunCommandSettles(t *testing.T) {
	want := &shikimori.SyncResult{Updated: 1, Created: 2, Pushed: 3}
	s := NewSyncScreen(syncDeps(want, nil))

	msg := safeCmd(syncScreenID, s.runCmd())()
	settled, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("runCmd settled %#v, want syncDoneMsg", msg)
	}
	if settled.err != nil {
		t.Fatalf("runCmd err = %v, want nil", settled.err)
	}
	if settled.result == nil || settled.result.Created != 2 {
		t.Fatalf("runCmd result = %+v, want the seam verdict", settled.result)
	}
}

// TestSyncScreenCompletionShowsSummary pins the settled surface: the
// Russian summary line with the three counters.
func TestSyncScreenCompletionShowsSummary(t *testing.T) {
	s := NewSyncScreen(syncDeps(nil, nil))
	next, _ := s.Update(syncDoneMsg{result: &shikimori.SyncResult{
		Updated: 4, Created: 5, Pushed: 6,
	}})
	view := next.View().Content
	if !contains(view, "Синхронизировано: 4 обновлено, 5 добавлено, 6 отправлено") {
		t.Fatalf("summary line missing, got:\n%s", view)
	}
	if !contains(view, "завершена") {
		t.Fatalf("success line missing, got:\n%s", view)
	}
}

// TestSyncScreenDoneTransitionsToRoot pins both exits from the settled
// summary: any key press, and the auto-advance after the hold — both
// replace the sync screen with the root menu.
func TestSyncScreenDoneTransitionsToRoot(t *testing.T) {
	transitionIsReplace := func(s Screen, msg tea.Msg) Screen {
		next, cmd := s.Update(msg)
		if cmd == nil {
			t.Fatalf("%T must schedule the root transition", msg)
		}
		rm, ok := cmd().(replaceMsg)
		if !ok {
			t.Fatalf("%T transition = %#v, want replaceMsg", msg, cmd())
		}
		if rm.screen.ID() != rootScreenID {
			t.Fatalf("replace target = %q, want root", rm.screen.ID())
		}
		return next
	}

	t.Run("key press", func(t *testing.T) {
		var s Screen = NewSyncScreen(syncDeps(nil, nil))
		s, _ = s.Update(syncDoneMsg{result: &shikimori.SyncResult{}})
		transitionIsReplace(s, enter())
	})

	t.Run("auto advance", func(t *testing.T) {
		var s Screen = NewSyncScreen(syncDeps(nil, nil))
		s, _ = s.Update(syncDoneMsg{result: &shikimori.SyncResult{}})
		transitionIsReplace(s, syncAdvanceMsg{})
	})
}

// TestSyncScreenFailureShowsWarning pins the degraded surface: a
// network failure renders the yellow warning, waits for a key press
// (no auto-advance) and then continues to the root menu.
func TestSyncScreenFailureShowsWarning(t *testing.T) {
	var s Screen = NewSyncScreen(syncDeps(nil, errors.New("сеть недоступна")))
	s, cmd := s.Update(syncDoneMsg{err: errors.New("сеть недоступна")})
	if cmd != nil {
		t.Fatal("failure must not schedule the auto-advance")
	}
	view := s.View().Content
	if !contains(view, "Синхронизация не удалась") {
		t.Fatalf("warning line missing, got:\n%s", view)
	}

	// The auto-advance must be a no-op in the failed phase.
	next2, cmd2 := s.Update(syncAdvanceMsg{})
	if cmd2 != nil {
		t.Fatal("auto-advance fired in the failed phase, want key-press-only exit")
	}
	_ = next2

	_, cmd3 := s.Update(enter())
	if cmd3 == nil {
		t.Fatal("key press after failure must schedule the root transition")
	}
	if rm, ok := cmd3().(replaceMsg); !ok || rm.screen.ID() != rootScreenID {
		t.Fatalf("key press after failure = %#v, want replace with root", cmd3())
	}
}

// TestSyncScreenNilSeamFailsLoudly pins the loud-degradation rule: a
// sync screen without the seam reports an error instead of pretending
// success.
func TestSyncScreenNilSeamFailsLoudly(t *testing.T) {
	s := NewSyncScreen(&Deps{})
	msg := safeCmd(syncScreenID, s.runCmd())()
	settled, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("runCmd settled %#v, want syncDoneMsg", msg)
	}
	if settled.err == nil {
		t.Fatal("nil seam must settle an error, not a fake success")
	}
}

// TestInitialStackSyncOrdering pins the PR27 opening order: auth setup
// still gates everything, then the startup sync runs before the root
// menu — but only when the sync seam is wired.
func TestInitialStackSyncOrdering(t *testing.T) {
	wired := func() *Deps { return syncDeps(nil, nil) }

	t.Run("needs setup — auth gate wins over sync", func(t *testing.T) {
		deps := wired()
		deps.ShikiCfg = config.Shikimori{Enabled: true}
		ids := screenIDs(initialStack(deps))
		if len(ids) != 1 || ids[0] != shikiSetupID {
			t.Fatalf("want [shikimori_setup], got %v", ids)
		}
	})

	t.Run("authenticated + wired — sync before root", func(t *testing.T) {
		ids := screenIDs(initialStack(wired()))
		if len(ids) != 1 || ids[0] != syncScreenID {
			t.Fatalf("want [shikimori_sync], got %v", ids)
		}
	})

	t.Run("authenticated + no seam — root directly", func(t *testing.T) {
		deps := &Deps{ShikiCfg: config.Shikimori{Enabled: true, Session: "s"}}
		ids := screenIDs(initialStack(deps))
		if len(ids) != 1 || ids[0] != rootScreenID {
			t.Fatalf("want [root], got %v", ids)
		}
	})

	t.Run("sync requires credentials, not just the seam", func(t *testing.T) {
		deps := wired()
		deps.ShikiCfg = config.Shikimori{Enabled: true} // no session/token
		ids := screenIDs(initialStack(deps))
		if len(ids) != 1 || ids[0] != shikiSetupID {
			t.Fatalf("want [shikimori_setup] (auth is mandatory first), got %v", ids)
		}
	})
}

// TestAfterAuthScreenTransition pins the post-auth opening (PR27): with
// the seam wired, completing the first-run auth leads through the sync
// screen; without it, straight to the root menu.
func TestAfterAuthScreenTransition(t *testing.T) {
	if got := afterAuthScreen(syncDeps(nil, nil)).ID(); got != syncScreenID {
		t.Fatalf("wired after-auth screen = %q, want %q", got, syncScreenID)
	}
	plain := &Deps{ShikiCfg: config.Shikimori{Enabled: true}}
	if got := afterAuthScreen(plain).ID(); got != rootScreenID {
		t.Fatalf("unwired after-auth screen = %q, want %q", got, rootScreenID)
	}
}

// TestCookieDoneLeadsToSyncWhenWired pins the wired end of the cookie
// setup: the done phase replaces itself with the sync screen, not the
// root menu.
func TestCookieDoneLeadsToSyncWhenWired(t *testing.T) {
	deps := syncDeps(nil, nil)
	s := newShikiCookieScreen(deps)
	s.phase = shikiPhaseDone

	_, cmd := s.Update(enter())
	if cmd == nil {
		t.Fatal("enter on done must schedule a transition")
	}
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("transition = %#v, want replaceMsg", cmd())
	}
	if rm.screen.ID() != syncScreenID {
		t.Fatalf("cookie done target = %q, want %q", rm.screen.ID(), syncScreenID)
	}
}
