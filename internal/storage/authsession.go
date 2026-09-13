package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuthSession is the api_auth_session row: one API login session with a
// rotating refresh token and optional request-scoped Shikimori
// credentials (python alembic 20260404_000001; api_server.py login flow).
type AuthSession struct {
	ID                 int64
	SessionID          string
	UserLogin          string
	RefreshTokenHash   string
	ShikiAuthMode      *string
	ShikiUsername      *string
	ShikiCookieSession *string
	ShikiAccessToken   *string
	ExpiresAt          time.Time
	RevokedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AuthSessionRepo is the api_auth_session repository backing the token
// auth face (PR10): login creates, the bearer guard reads by session id,
// refresh rotates the token hash, logout revokes.
type AuthSessionRepo struct {
	db *sql.DB
}

const authSessionSelect = `SELECT id, session_id, user_login, refresh_token_hash,
	shiki_auth_mode, shiki_username, shiki_cookie_session, shiki_access_token,
	expires_at, revoked_at, created_at, updated_at
	FROM api_auth_session`

// Create inserts a new session row, stamping created_at/updated_at.
func (r *AuthSessionRepo) Create(ctx context.Context, s *AuthSession) error {
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
INSERT INTO api_auth_session
	(session_id, user_login, refresh_token_hash,
	 shiki_auth_mode, shiki_username, shiki_cookie_session, shiki_access_token,
	 expires_at, revoked_at, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		s.SessionID, s.UserLogin, s.RefreshTokenHash,
		nullString(s.ShikiAuthMode), nullString(s.ShikiUsername),
		nullString(s.ShikiCookieSession), nullString(s.ShikiAccessToken),
		fmtTime(s.ExpiresAt), nullTimePtr(s.RevokedAt), fmtTime(now), fmtTime(now))
	if err != nil {
		return fmt.Errorf("create api auth session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("create api auth session: row id: %w", err)
	}
	s.ID = id
	s.CreatedAt = now
	s.UpdatedAt = now
	return nil
}

// GetBySessionID loads one session by its public id.
func (r *AuthSessionRepo) GetBySessionID(ctx context.Context, sessionID string) (*AuthSession, error) {
	s, err := scanAuthSession(r.db.QueryRowContext(ctx,
		authSessionSelect+` WHERE session_id = ?`, sessionID))
	if err != nil {
		return nil, notFound(fmt.Errorf("get api auth session %q: %w", sessionID, err))
	}
	return s, nil
}

// GetByRefreshHash loads the session currently holding refresh_token_hash
// (rotation replaces the hash, so at most one row matches).
func (r *AuthSessionRepo) GetByRefreshHash(ctx context.Context, hash string) (*AuthSession, error) {
	s, err := scanAuthSession(r.db.QueryRowContext(ctx,
		authSessionSelect+` WHERE refresh_token_hash = ?`, hash))
	if err != nil {
		return nil, notFound(fmt.Errorf("get api auth session by refresh hash: %w", err))
	}
	return s, nil
}

// RotateRefresh swaps the refresh token hash and extends the expiry of
// one session, returning the refreshed row. A missing session fails with
// contracts.ErrNotFound (python rotate_api_auth_session_refresh).
func (r *AuthSessionRepo) RotateRefresh(ctx context.Context, sessionID, newHash string, expiresAt time.Time) (*AuthSession, error) {
	res, err := r.db.ExecContext(ctx, `
UPDATE api_auth_session
	SET refresh_token_hash = ?, expires_at = ?, updated_at = ?
	WHERE session_id = ?`,
		newHash, fmtTime(expiresAt), fmtTime(time.Now().UTC()), sessionID)
	if err != nil {
		return nil, fmt.Errorf("rotate api auth session %q: %w", sessionID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("rotate api auth session %q: %w", sessionID, ErrSessionNotFound)
	}
	return r.GetBySessionID(ctx, sessionID)
}

// Revoke marks one session revoked (logout). Revoking an already-revoked
// session is an idempotent success; a missing session fails loudly.
func (r *AuthSessionRepo) Revoke(ctx context.Context, sessionID string) error {
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `
UPDATE api_auth_session
	SET revoked_at = COALESCE(revoked_at, ?), updated_at = ?
	WHERE session_id = ?`,
		fmtTime(now), fmtTime(now), sessionID)
	if err != nil {
		return fmt.Errorf("revoke api auth session %q: %w", sessionID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("revoke api auth session %q: %w", sessionID, ErrSessionNotFound)
	}
	return nil
}

// ErrSessionNotFound reports a missing api_auth_session row distinctly
// from contracts.ErrNotFound so auth paths can map it to the
// unauthorized error contract instead of 404.
var ErrSessionNotFound = fmt.Errorf("api auth session not found")

func scanAuthSession(row rowScanner) (*AuthSession, error) {
	var (
		s                                  AuthSession
		mode, user, cookie, token, revoked sql.NullString
		expires, created, updated          string
	)
	err := row.Scan(&s.ID, &s.SessionID, &s.UserLogin, &s.RefreshTokenHash,
		&mode, &user, &cookie, &token,
		&expires, &revoked, &created, &updated)
	if err != nil {
		return nil, err
	}
	s.ShikiAuthMode = stringPtr(mode)
	s.ShikiUsername = stringPtr(user)
	s.ShikiCookieSession = stringPtr(cookie)
	s.ShikiAccessToken = stringPtr(token)
	if s.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if s.RevokedAt, err = timePtr(revoked); err != nil {
		return nil, err
	}
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &s, nil
}
