package shikimori

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestSyncCancelWindowStillPersistsRateID pins F39: when the caller's
// context is cancelled in the window after the push reached the server
// but before the local DB writes, SetRateID/ClearDirty still complete
// (context.WithoutCancel) — the rate id lands, dirty stays clear and
// the next sync PATCHes instead of creating a duplicate rate.
func TestSyncCancelWindowStillPersistsRateID(t *testing.T) {
	t.Parallel()

	var posts, patches atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, syncer, st, progress := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 50})
		case "/api/v2/user_rates":
			// Server records the create, THEN the caller's context dies.
			// Explicit Content-Length so the body is complete at flush
			// (chunked responses stay incomplete until handler return).
			body := `{"id":901}`
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(body))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Generous margin for the loopback client to consume the
			// body; the cancel then lands inside the local-write window.
			time.Sleep(150 * time.Millisecond)
			posts.Add(1)
			cancel()
		case "/api/v2/user_rates/901":
			patches.Add(1)
			writeJSON(w, map[string]any{"id": 901})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}, false)

	// First sync: push succeeds server-side, ctx cancels right after.
	if err := syncer.SyncEpisodeProgress(ctx, progress, 4); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v (rate-id writes must survive caller cancellation)", err)
	}

	got := mustGetProgress(t, st, progress.ID)
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 901 {
		t.Fatalf("rate id = %v, want persisted 901 despite cancel window", got.ShikimoriRateID)
	}
	if got.Dirty {
		t.Fatal("dirty set despite successful push — next sync would duplicate the rate")
	}
	if got.CurrentEpisode != "4" {
		t.Errorf("episode = %q, want 4", got.CurrentEpisode)
	}

	// Second sync with a healthy context must PATCH the existing rate,
	// never POST a duplicate.
	if err := syncer.SyncEpisodeProgress(context.Background(), got, 5); err != nil {
		t.Fatalf("second SyncEpisodeProgress: %v", err)
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("POST count = %d, want exactly 1 (no duplicate rate on re-sync)", n)
	}
	if n := patches.Load(); n != 1 {
		t.Errorf("PATCH count = %d, want 1 (update path via persisted rate id)", n)
	}
}

// TestSyncPersistSuccessSurvivesCanceledContext pins F39 at the
// deterministically controllable seam: the post-push persistence must
// complete with a caller context that is ALREADY dead — the server has
// recorded the push, so losing the local rate id now would make the
// next sync create a duplicate rate.
func TestSyncPersistSuccessSurvivesCanceledContext(t *testing.T) {
	t.Parallel()

	_, syncer, st, progress := newSyncFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("persistSuccess must not touch the network")
	}, true)

	// Simulate the push having succeeded server-side, then the caller
	// cancelling exactly before the local writes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := syncer.persistSuccess(ctx, progress, 902); err != nil {
		t.Fatalf("persistSuccess with dead context: %v", err)
	}

	got := mustGetProgress(t, st, progress.ID)
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 902 {
		t.Fatalf("rate id = %v, want persisted 902 despite dead ctx", got.ShikimoriRateID)
	}
	if got.Dirty {
		t.Error("dirty set despite successful push verdict")
	}
}
