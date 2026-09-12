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

// newSyncFixture builds a cookie-mode client + in-memory store with one
// progress row bound to shikimori anime 500 (rate 900 when withRate).
func newSyncFixture(t *testing.T, handler http.HandlerFunc, withRate bool) (*Client, *Syncer, *storage.Store, storage.AnimeProgress) {
	t.Helper()

	c, _ := newTestClient(t, cookieCfg("syncsess"), handler)
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	progress := storage.AnimeProgress{
		Title:           "Sync Me",
		SourceID:        "animego",
		SourceURL:       "https://animego.org/anime/sync-me",
		CurrentEpisode:  "3",
		ShikimoriID:     int64Ptr(500),
		ShikimoriStatus: "watching",
		UpdatedAt:       time.Now().UTC(),
	}
	if withRate {
		progress.ShikimoriRateID = int64Ptr(900)
	}
	if err := st.Progress.Upsert(context.Background(), &progress); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	return c, NewSyncer(c, st.Progress, nil), st, progress
}

// syncHandler serves the full happy-path cookie flow.
func syncHandler(t *testing.T, method, path string, respond func(w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 50})
		case path:
			if r.Method != method {
				t.Errorf("%s method = %s, want %s", path, r.Method, method)
			}
			respond(w)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }

func mustGetProgress(t *testing.T, st *storage.Store, id int64) storage.AnimeProgress {
	t.Helper()
	p, err := st.Progress.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	return *p
}

// TestSyncPushSuccessClearsDirty pins the immediate-push happy path with
// an existing rate: PATCH episodes+watching, local episode updated,
// dirty cleared.
func TestSyncPushSuccessClearsDirty(t *testing.T) {
	t.Parallel()

	var body string
	handler := syncHandler(t, http.MethodPatch, "/api/v2/user_rates/900", func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"id": 900})
	})
	// Capture the PATCH body.
	_, syncer, st, progress := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/user_rates/900" {
			body = mustReadBody(t, r)
		}
		handler(w, r)
	}, true)

	if err := syncer.SyncEpisodeProgress(context.Background(), progress, 4); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}

	got := mustGetProgress(t, st, progress.ID)
	if got.Dirty {
		t.Error("dirty set after successful push, want cleared")
	}
	if got.CurrentEpisode != "4" {
		t.Errorf("episode = %q, want 4 (local update first)", got.CurrentEpisode)
	}
	if !strings.Contains(body, `"episodes":4`) || !strings.Contains(body, `"status":"watching"`) {
		t.Errorf("push payload = %q, want episodes=4 + watching", body)
	}
}

// TestSyncPushCreatePersistsRateID pins the new-title fix: when no rate
// exists, the created rate id is persisted (duplicate-rate bug ruling).
func TestSyncPushCreatePersistsRateID(t *testing.T) {
	t.Parallel()

	c, syncer, st, progress := newSyncFixture(t, syncHandler(t,
		http.MethodPost, "/api/v2/user_rates", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 901})
		}), false)
	_ = c

	if err := syncer.SyncEpisodeProgress(context.Background(), progress, 5); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}

	got := mustGetProgress(t, st, progress.ID)
	if got.Dirty {
		t.Error("dirty set after successful create, want cleared")
	}
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 901 {
		t.Errorf("rate id = %v, want persisted 901", got.ShikimoriRateID)
	}
}

// TestSyncAuthFailureSetsDirty pins: a rejected push marks the row dirty
// (deferred to the next sync) and surfaces ErrAuthRequired.
func TestSyncAuthFailureSetsDirty(t *testing.T) {
	t.Parallel()

	c, syncer, st, progress := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 50})
		case "/api/v2/user_rates/900":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}, true)
	_ = c

	err := syncer.SyncEpisodeProgress(context.Background(), progress, 4)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("SyncEpisodeProgress err = %v, want ErrAuthRequired", err)
	}

	got := mustGetProgress(t, st, progress.ID)
	if !got.Dirty {
		t.Error("dirty not set after auth failure, want set")
	}
}

// TestSyncNetworkFailureThenRetryClears pins the full deferred loop:
// network failure marks dirty; a later successful push clears it.
func TestSyncNetworkFailureThenRetryClears(t *testing.T) {
	t.Parallel()

	var signIns int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, syncer, st, progress := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			signIns++
			if signIns == 1 {
				// Simulate the network dying mid-flight: cancel the
				// caller's context, then hang until the client gives up.
				cancel()
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 50})
		case "/api/v2/user_rates/900":
			writeJSON(w, map[string]any{"id": 900})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}, true)

	// First attempt: the push dies with the canceled context AFTER the
	// local update has landed.
	if err := syncer.SyncEpisodeProgress(ctx, progress, 4); err == nil {
		t.Fatal("SyncEpisodeProgress with dying network returned nil, want error")
	}
	got := mustGetProgress(t, st, progress.ID)
	if !got.Dirty {
		t.Fatal("dirty not set after network failure")
	}
	if got.CurrentEpisode != "4" {
		t.Errorf("episode = %q, want 4 (local update survives push failure)", got.CurrentEpisode)
	}

	// Retry later: healthy push clears the flag.
	if err := syncer.SyncEpisodeProgress(context.Background(), progress, 4); err != nil {
		t.Fatalf("retry SyncEpisodeProgress: %v", err)
	}
	if got := mustGetProgress(t, st, progress.ID); got.Dirty {
		t.Error("dirty still set after successful retry, want cleared")
	}
}

// TestSyncNoBindingIsNoop pins: without a shikimori binding there is
// nothing to push — local episode updates, dirty untouched, no network.
func TestSyncNoBindingIsNoop(t *testing.T) {
	t.Parallel()

	c, syncer, st, _ := newSyncFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("unbound progress must not touch the network")
	}, true)
	_ = c

	progress := storage.AnimeProgress{
		Title: "Unbound", SourceID: "x", SourceURL: "https://x/1",
		CurrentEpisode: "1", UpdatedAt: time.Now().UTC(),
	}
	if err := st.Progress.Upsert(context.Background(), &progress); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := syncer.SyncEpisodeProgress(context.Background(), progress, 2); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}
	got := mustGetProgress(t, st, progress.ID)
	if got.Dirty {
		t.Error("dirty set on unbound progress, want untouched")
	}
	if got.CurrentEpisode != "2" {
		t.Errorf("episode = %q, want 2", got.CurrentEpisode)
	}
}

// TestSyncDisabledSetsDirty pins the download.py else-branch: shikimori
// disabled -> local status update with dirty set.
func TestSyncDisabledSetsDirty(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{}, func(http.ResponseWriter, *http.Request) {
		t.Error("disabled client must not touch the network")
	})
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	progress := storage.AnimeProgress{
		Title: "Off", SourceID: "x", SourceURL: "https://x/2",
		CurrentEpisode: "1", ShikimoriID: int64Ptr(7), UpdatedAt: time.Now().UTC(),
	}
	if err := st.Progress.Upsert(context.Background(), &progress); err != nil {
		t.Fatalf("seed: %v", err)
	}

	syncer := NewSyncer(c, st.Progress, nil)
	if err := syncer.SyncEpisodeProgress(context.Background(), progress, 2); !errors.Is(err, ErrDisabled) {
		t.Fatalf("SyncEpisodeProgress err = %v, want ErrDisabled", err)
	}
	got := mustGetProgress(t, st, progress.ID)
	if !got.Dirty {
		t.Error("dirty not set with disabled shikimori, want set")
	}
	if got.CurrentEpisode != "2" {
		t.Errorf("episode = %q, want 2 (local update still happens)", got.CurrentEpisode)
	}
	if n := len(log.snapshot()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}
