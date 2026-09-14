package shikimori

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/storage"
)

// syncFullFixture builds a cookie-mode client + in-memory store for the
// PR27 startup two-way sync. Seeds are upserted via seedProgress.
func syncFullFixture(t *testing.T, handler http.HandlerFunc) (*Client, *Syncer, *storage.Store) {
	t.Helper()

	c, _ := newTestClient(t, cookieCfg("fullsync"), handler)
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return c, NewSyncer(c, st.Progress, nil), st
}

// seedProgress inserts one progress row and returns it.
func seedProgress(t *testing.T, st *storage.Store, p storage.AnimeProgress) storage.AnimeProgress {
	t.Helper()
	p.UpdatedAt = time.Now().UTC()
	if err := st.Progress.Upsert(context.Background(), &p); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	return p
}

// syncFullHandler serves the cookie-mode plumbing (sign_in, whoami) and
// delegates the API surface to the callback table.
func syncFullHandler(t *testing.T, rates func(w http.ResponseWriter), extra func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 50})
		case "/api/v2/user_rates":
			if r.Method == http.MethodGet {
				rates(w)
				return
			}
			extra(w, r)
		default:
			extra(w, r)
		}
	}
}

// rateJSON renders one user_rates row.
func rateJSON(id, target int64, status string, episodes, score, rewatches int) map[string]any {
	return map[string]any{
		"id": id, "user_id": 50, "target_id": target, "target_type": "Anime",
		"status": status, "episodes": episodes, "score": score, "rewatches": rewatches,
	}
}

// TestSyncFullUpdatesExisting pins the pull phase for an already-bound
// row (python shikimori_sync.py:49-61): remote status/rate id/score/
// rewatches overwrite local; the episode only moves FORWARD (remote >
// local), never backward.
func TestSyncFullUpdatesExisting(t *testing.T) {
	t.Parallel()

	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		writeJSON(w, []any{
			rateJSON(901, 500, "completed", 5, 8, 2), // ahead of local
			rateJSON(902, 501, "watching", 4, 0, 0),  // behind local
		})
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	_, syncer, st := syncFullFixture(t, handler)

	forward := seedProgress(t, st, storage.AnimeProgress{
		Title: "Forward", SourceID: "animego", SourceURL: "https://animego/f",
		CurrentEpisode: "3", ShikimoriID: int64Ptr(500), ShikimoriRateID: int64Ptr(900),
		ShikimoriStatus: "watching",
	})
	backward := seedProgress(t, st, storage.AnimeProgress{
		Title: "Backward", SourceID: "animego", SourceURL: "https://animego/b",
		CurrentEpisode: "7", ShikimoriID: int64Ptr(501), ShikimoriStatus: "watching",
	})

	result, err := syncer.SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull: %v", err)
	}
	if result.Updated != 2 || result.Created != 0 || result.Pushed != 0 || result.Conflicts != 0 {
		t.Fatalf("result = %+v, want Updated=2 only", result)
	}

	got := mustGetProgress(t, st, forward.ID)
	if got.ShikimoriStatus != "completed" || got.Score != 8 || got.Rewatches != 2 {
		t.Errorf("forward row not refreshed from remote: %+v", got)
	}
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 901 {
		t.Errorf("rate id = %v, want 901", got.ShikimoriRateID)
	}
	if got.CurrentEpisode != "5" {
		t.Errorf("forward episode = %q, want 5 (remote ahead)", got.CurrentEpisode)
	}

	back := mustGetProgress(t, st, backward.ID)
	if back.CurrentEpisode != "7" {
		t.Errorf("backward episode = %q, want 7 (remote behind local stays)", back.CurrentEpisode)
	}
	if back.ShikimoriRateID == nil || *back.ShikimoriRateID != 902 {
		t.Errorf("backward rate id = %v, want 902", back.ShikimoriRateID)
	}
}

// TestSyncFullCreatesNew pins the new-anime path (python
// shikimori_sync.py:63-101): remote-only ids are fetched via
// GetAnimesInfo and inserted as shikimori-sourced placeholder rows with
// needs_correction set.
func TestSyncFullCreatesNew(t *testing.T) {
	t.Parallel()

	var patchBase string
	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		writeJSON(w, []any{
			rateJSON(900, 500, "watching", 3, 7, 0),
			rateJSON(902, 700, "planned", 4, 0, 0),
		})
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/animes" {
			writeJSON(w, []map[string]any{{
				"id": 700, "name": "New Anime", "russian": "Новое аниме",
				"image":    map[string]any{"original": "/animes/original/700.jpg"},
				"episodes": 12,
			}})
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	c, syncer, st := syncFullFixture(t, handler)
	patchBase = c.baseURL
	_ = patchBase

	seedProgress(t, st, storage.AnimeProgress{
		Title: "Bound", SourceID: "animego", SourceURL: "https://animego/f",
		CurrentEpisode: "3", ShikimoriID: int64Ptr(500), ShikimoriStatus: "watching",
	})

	result, err := syncer.SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull: %v", err)
	}
	if result.Updated != 1 || result.Created != 1 || result.Pushed != 0 || result.Conflicts != 0 {
		t.Fatalf("result = %+v, want Updated=1 Created=1", result)
	}

	created, err := st.Progress.GetByShikimoriID(context.Background(), 700)
	if err != nil {
		t.Fatalf("new anime not created: %v", err)
	}
	want := map[string]string{
		"title": "Новое аниме", "source": "shikimori", "url": "700",
		"episode": "4", "status": "planned",
	}
	if created.Title != want["title"] || created.SourceID != want["source"] ||
		created.SourceURL != want["url"] || created.CurrentEpisode != want["episode"] ||
		created.ShikimoriStatus != want["status"] {
		t.Errorf("created row mismatch: %+v", *created)
	}
	if created.TotalEpisodes != 12 {
		t.Errorf("total episodes = %d, want 12", created.TotalEpisodes)
	}
	if created.ShikimoriRateID == nil || *created.ShikimoriRateID != 902 {
		t.Errorf("rate id = %v, want 902", created.ShikimoriRateID)
	}
	if created.Poster == nil || !strings.HasPrefix(*created.Poster, "http") ||
		!strings.HasSuffix(*created.Poster, "/animes/original/700.jpg") {
		t.Errorf("poster = %v, want absolute original url", created.Poster)
	}
	if !created.NeedsCorrection {
		t.Error("needs_correction not set on shikimori-sourced placeholder")
	}
	if created.Dirty {
		t.Error("fresh remote row must not be dirty")
	}
}

// TestSyncFullPushesDirty pins the deferred-push phase: a dirty row
// (offline edit) is replayed to Shikimori — PATCH via the stored rate
// id or POST a fresh rate — and the flag clears only on success.
func TestSyncFullPushesDirty(t *testing.T) {
	t.Parallel()

	var patchBody string
	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		writeJSON(w, []any{rateJSON(900, 500, "watching", 3, 0, 0)})
	}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v2/user_rates/900":
			patchBody = mustReadBody(t, r)
			writeJSON(w, map[string]any{"id": 900})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/user_rates":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 903})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	_, syncer, st := syncFullFixture(t, handler)

	patchMe := seedProgress(t, st, storage.AnimeProgress{
		Title: "PatchMe", SourceID: "animego", SourceURL: "https://animego/p",
		CurrentEpisode: "6", ShikimoriID: int64Ptr(500), ShikimoriRateID: int64Ptr(900),
		ShikimoriStatus: "watching", Dirty: true,
	})
	postMe := seedProgress(t, st, storage.AnimeProgress{
		Title: "PostMe", SourceID: "animego", SourceURL: "https://animego/q",
		CurrentEpisode: "2", ShikimoriID: int64Ptr(800), ShikimoriStatus: "completed",
		Dirty: true,
	})

	result, err := syncer.SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull: %v", err)
	}
	if result.Pushed != 2 {
		t.Fatalf("Pushed = %d, want 2", result.Pushed)
	}
	if !strings.Contains(patchBody, `"episodes":6`) {
		t.Errorf("PATCH body = %q, want local episode 6 replayed", patchBody)
	}

	patched := mustGetProgress(t, st, patchMe.ID)
	if patched.Dirty {
		t.Error("dirty survived a successful PATCH")
	}
	posted := mustGetProgress(t, st, postMe.ID)
	if posted.Dirty {
		t.Error("dirty survived a successful POST")
	}
	if posted.ShikimoriRateID == nil || *posted.ShikimoriRateID != 903 {
		t.Errorf("created rate id = %v, want 903 persisted", posted.ShikimoriRateID)
	}
}

// TestSyncFullConflictResolution pins the conflict verdict: a dirty row
// matched by a remote rate counts as both-sides-changed — the remote
// status wins locally, the LOCAL episode wins and is pushed up.
func TestSyncFullConflictResolution(t *testing.T) {
	t.Parallel()

	var patchBody string
	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		writeJSON(w, []any{rateJSON(900, 500, "on_hold", 9, 6, 1)})
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v2/user_rates/900" {
			patchBody = mustReadBody(t, r)
			writeJSON(w, map[string]any{"id": 900})
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	_, syncer, st := syncFullFixture(t, handler)

	dirty := seedProgress(t, st, storage.AnimeProgress{
		Title: "Conflict", SourceID: "animego", SourceURL: "https://animego/c",
		CurrentEpisode: "6", ShikimoriID: int64Ptr(500), ShikimoriRateID: int64Ptr(900),
		ShikimoriStatus: "watching", Dirty: true,
	})

	result, err := syncer.SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull: %v", err)
	}
	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.Pushed != 1 {
		t.Fatalf("Pushed = %d, want 1 (conflict row is still replayed)", result.Pushed)
	}
	if result.Updated != 0 {
		t.Errorf("Updated = %d, want 0 (conflict rows count separately)", result.Updated)
	}

	got := mustGetProgress(t, st, dirty.ID)
	if got.ShikimoriStatus != "on_hold" {
		t.Errorf("status = %q, want remote on_hold (remote wins status)", got.ShikimoriStatus)
	}
	if got.CurrentEpisode != "6" {
		t.Errorf("episode = %q, want local 6 (local wins episode progress)", got.CurrentEpisode)
	}
	if !strings.Contains(patchBody, `"episodes":6`) || !strings.Contains(patchBody, `"status":"on_hold"`) {
		t.Errorf("PATCH body = %q, want merged state (local episode + remote status)", patchBody)
	}
}

// TestSyncFullEmptyRatesIsNoop pins python `if not rates: return`: an
// empty remote list settles silently without further requests.
func TestSyncFullEmptyRatesIsNoop(t *testing.T) {
	t.Parallel()

	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		writeJSON(w, []any{})
	}, func(http.ResponseWriter, *http.Request) {
		t.Error("empty rates must not trigger further requests")
	})
	_, syncer, _ := syncFullFixture(t, handler)

	result, err := syncer.SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull: %v", err)
	}
	if result.Updated != 0 || result.Created != 0 || result.Pushed != 0 || result.Conflicts != 0 {
		t.Fatalf("result = %+v, want all zero", result)
	}
}

// TestSyncFullRatesErrorSurfaces pins the network-failure verdict: the
// rates fetch failing returns the empty result AND the error (the TUI
// renders its yellow warning from it).
func TestSyncFullRatesErrorSurfaces(t *testing.T) {
	t.Parallel()

	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	}, func(http.ResponseWriter, *http.Request) {
		t.Error("a failed rates fetch must not trigger further requests")
	})
	_, syncer, _ := syncFullFixture(t, handler)

	result, err := syncer.SyncFull(context.Background(), nil)
	if err == nil {
		t.Fatal("SyncFull err = nil, want the rates failure surfaced")
	}
	if result == nil || result.Updated != 0 || result.Created != 0 {
		t.Fatalf("result = %+v, want zeroed on fetch failure", result)
	}
}

// TestSyncFullDisabledIsNoop pins the enabled gate (python
// `if not cfg.enabled: return`): a disabled integration never touches
// the network and reports nothing.
func TestSyncFullDisabledIsNoop(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{}, func(http.ResponseWriter, *http.Request) {
		t.Error("disabled client must not touch the network")
	})
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	result, err := NewSyncer(c, st.Progress, nil).SyncFull(context.Background(), nil)
	if err != nil {
		t.Fatalf("SyncFull on disabled integration: %v", err)
	}
	if result.Updated != 0 || result.Created != 0 || result.Pushed != 0 || result.Conflicts != 0 {
		t.Fatalf("result = %+v, want all zero", result)
	}
	if n := len(log.snapshot()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// TestSyncFullPushFailureKeepsDirty pins the deferred handoff on a
// startup replay that fails: the row stays dirty for the next pass and
// the error surfaces with the partial result.
func TestSyncFullPushFailureKeepsDirty(t *testing.T) {
	t.Parallel()

	handler := syncFullHandler(t, func(w http.ResponseWriter) {
		// Non-empty rates keep the flow alive past the python early
		// return; the clean row below absorbs the pull phase.
		writeJSON(w, []any{rateJSON(901, 500, "watching", 1, 0, 0)})
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v2/user_rates/900" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	_, syncer, st := syncFullFixture(t, handler)

	seedProgress(t, st, storage.AnimeProgress{
		Title: "Clean", SourceID: "animego", SourceURL: "https://animego/f",
		CurrentEpisode: "1", ShikimoriID: int64Ptr(500), ShikimoriRateID: int64Ptr(901),
		ShikimoriStatus: "watching",
	})
	dirty := seedProgress(t, st, storage.AnimeProgress{
		Title: "Stuck", SourceID: "animego", SourceURL: "https://animego/s",
		CurrentEpisode: "6", ShikimoriID: int64Ptr(501), ShikimoriRateID: int64Ptr(900),
		ShikimoriStatus: "watching", Dirty: true,
	})

	result, err := syncer.SyncFull(context.Background(), nil)
	if err == nil {
		t.Fatal("SyncFull err = nil, want push failure surfaced")
	}
	if result == nil || result.Pushed != 0 {
		t.Fatalf("result = %+v, want zero Pushed with partial result", result)
	}
	if err != nil && !errors.Is(err, ErrAuthRequired) {
		t.Errorf("err = %v, want ErrAuthRequired from rejected replay", err)
	}
	if got := mustGetProgress(t, st, dirty.ID); !got.Dirty {
		t.Error("dirty cleared on failed replay, want kept for next sync")
	}
}
