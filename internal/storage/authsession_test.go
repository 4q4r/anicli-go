package storage

import (
	"context"
	"testing"
	"time"
)

// newTestStore opens a fresh in-memory store for one test.
func newAuthTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAuthSessionLifecycle(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	sess := &AuthSession{
		SessionID:         "sid-1",
		UserLogin:         "alice",
		RefreshTokenHash:  "hash-1",
		ShikiAuthMode:     strPtr("cookie"),
		ShikiUsername:     strPtr("alice-shiki"),
		ShikiCookieSession: strPtr("kawai"),
		ExpiresAt:         time.Now().Add(time.Hour).UTC(),
	}
	if err := st.AuthSessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.ID == 0 {
		t.Fatal("create did not write back row id")
	}
	if sess.CreatedAt.IsZero() || sess.UpdatedAt.IsZero() {
		t.Fatal("create must stamp created_at/updated_at")
	}

	got, err := st.AuthSessions.GetBySessionID(ctx, "sid-1")
	if err != nil {
		t.Fatalf("get by session id: %v", err)
	}
	if got.UserLogin != "alice" || got.RefreshTokenHash != "hash-1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.ShikiAuthMode == nil || *got.ShikiAuthMode != "cookie" {
		t.Fatalf("shiki auth mode not roundtripped: %+v", got.ShikiAuthMode)
	}
	if got.ShikiCookieSession == nil || *got.ShikiCookieSession != "kawai" {
		t.Fatalf("shiki cookie not roundtripped")
	}
	if got.RevokedAt != nil {
		t.Fatalf("fresh session must not be revoked, got %v", *got.RevokedAt)
	}

	byHash, err := st.AuthSessions.GetByRefreshHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("get by refresh hash: %v", err)
	}
	if byHash.SessionID != "sid-1" {
		t.Fatalf("hash lookup returned %q", byHash.SessionID)
	}
}

func TestAuthSessionGetMissing(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	if _, err := st.AuthSessions.GetBySessionID(ctx, "nope"); err == nil {
		t.Fatal("missing session id must error")
	}
	if _, err := st.AuthSessions.GetByRefreshHash(ctx, "nope"); err == nil {
		t.Fatal("missing refresh hash must error")
	}
}

func TestAuthSessionRotateRefresh(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	sess := &AuthSession{
		SessionID:        "sid-2",
		UserLogin:        "bob",
		RefreshTokenHash: "hash-old",
		ExpiresAt:        time.Now().Add(time.Hour).UTC(),
	}
	if err := st.AuthSessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	newExp := time.Now().Add(2 * time.Hour).UTC()
	rotated, err := st.AuthSessions.RotateRefresh(ctx, "sid-2", "hash-new", newExp)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.RefreshTokenHash != "hash-new" {
		t.Fatalf("rotate did not swap hash: %q", rotated.RefreshTokenHash)
	}

	// Old hash must no longer resolve the session.
	if _, err := st.AuthSessions.GetByRefreshHash(ctx, "hash-old"); err == nil {
		t.Fatal("old refresh hash must be gone after rotation")
	}
	if _, err := st.AuthSessions.GetByRefreshHash(ctx, "hash-new"); err != nil {
		t.Fatalf("new refresh hash must resolve: %v", err)
	}

	// Rotating a missing session must fail loudly.
	if _, err := st.AuthSessions.RotateRefresh(ctx, "ghost", "x", newExp); err == nil {
		t.Fatal("rotate on missing session must error")
	}
}

func TestAuthSessionRevoke(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	sess := &AuthSession{
		SessionID:        "sid-3",
		UserLogin:        "carol",
		RefreshTokenHash: "hash-3",
		ExpiresAt:        time.Now().Add(time.Hour).UTC(),
	}
	if err := st.AuthSessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := st.AuthSessions.Revoke(ctx, "sid-3"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err := st.AuthSessions.GetBySessionID(ctx, "sid-3")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatal("revoked_at must be set after revoke")
	}

	// Second revoke on the same session is idempotent success (Python
	// semantics: logout sets revoked_at; a second call is a no-op), but a
	// missing session must fail.
	if err := st.AuthSessions.Revoke(ctx, "ghost"); err == nil {
		t.Fatal("revoke on missing session must error")
	}
}

func TestProgressUpdateLocalStatus(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	row := &AnimeProgress{
		Title: "Test Anime", SourceID: "animego", SourceURL: "https://x/y",
		CurrentEpisode: "1", ShikimoriStatus: "watching",
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.Progress.Upsert(ctx, row); err != nil {
		t.Fatalf("seed: %v", err)
	}

	status := "completed"
	score := 9
	rewatches := 2
	episode := "12"
	if err := st.Progress.UpdateLocalStatus(ctx, row.ID, &status, &score, &rewatches, &episode); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := st.Progress.GetByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.ShikimoriStatus != "completed" || got.Score != 9 || got.Rewatches != 2 || got.CurrentEpisode != "12" {
		t.Fatalf("fields not updated: %+v", got)
	}

	// Nil pointers must leave fields untouched.
	if err := st.Progress.UpdateLocalStatus(ctx, row.ID, nil, nil, nil, nil); err != nil {
		t.Fatalf("noop update: %v", err)
	}
	got, err = st.Progress.GetByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("reload noop: %v", err)
	}
	if got.ShikimoriStatus != "completed" || got.Score != 9 {
		t.Fatalf("noop update changed fields: %+v", got)
	}

	// Missing row fails loudly.
	if err := st.Progress.UpdateLocalStatus(ctx, 999999, &status, nil, nil, nil); err == nil {
		t.Fatal("update on missing row must error")
	}
}

func TestEpisodeProgressGetByEpisodeAndLatest(t *testing.T) {
	st := newAuthTestStore(t)
	ctx := context.Background()

	anime := &AnimeProgress{
		Title: "E", SourceID: "s", SourceURL: "u", CurrentEpisode: "1",
		ShikimoriStatus: "watching", UpdatedAt: time.Now().UTC(),
	}
	if err := st.Progress.Upsert(ctx, anime); err != nil {
		t.Fatalf("seed anime: %v", err)
	}

	mk := func(episode string, position int64) *EpisodeProgress {
		return &EpisodeProgress{
			AnimeID: anime.ID, Episode: episode,
			PositionSec: position, DurationSec: 1440,
			UpdatedAt: time.Now().UTC().Add(time.Duration(position) * time.Second),
		}
	}
	for _, e := range []*EpisodeProgress{mk("1", 10), mk("2", 200), mk("3", 100)} {
		if err := st.Episodes.Upsert(ctx, e); err != nil {
			t.Fatalf("seed episode %s: %v", e.Episode, err)
		}
	}

	got, err := st.Episodes.GetByEpisode(ctx, anime.ID, "2")
	if err != nil {
		t.Fatalf("get by episode: %v", err)
	}
	if got.PositionSec != 200 {
		t.Fatalf("wrong row: %+v", got)
	}

	if _, err := st.Episodes.GetByEpisode(ctx, anime.ID, "99"); err == nil {
		t.Fatal("missing episode must error")
	}

	latest, err := st.Episodes.LatestByAnime(ctx, anime.ID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Episode != "2" {
		t.Fatalf("latest must be newest updated_at row, got %q", latest.Episode)
	}
}
