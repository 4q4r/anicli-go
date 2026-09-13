package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/an0nx/anicli-go/internal/storage"
)

// authSession mirrors the stored session for request handling (the
// storage row plus convenience accessors).
type authSession = storage.AuthSession

// loginRequest is the /api/v1/auth/login body (python AuthLoginRequest).
type loginRequest struct {
	Login    string        `json:"login"`
	Password string        `json:"password"`
	Shiki    *shikiPayload `json:"shiki"`
}

// shikiPayload carries per-user Shikimori credentials bound to the
// session (python ShikiAuthPayload).
type shikiPayload struct {
	AuthMode      string  `json:"auth_mode"`
	Username      *string `json:"username"`
	CookieSession *string `json:"cookie_session"`
	AccessToken   *string `json:"access_token"`
}

// refreshRequest is the /api/v1/auth/refresh body.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// maxLoginBody bounds the JSON bodies accepted by auth endpoints
// (python pydantic caps the fields; the body cap prevents abuse).
const maxLoginBody = 64 << 10

// handleAuthLogin verifies credentials and issues the token pair
// (python auth_login).
func (a *App) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}
	if req.Login == "" || len(req.Login) > 128 {
		writeAPIError(w, r, fieldError("login", "required, 1-128 chars"))
		return
	}
	if req.Password == "" || len(req.Password) > 1024 {
		writeAPIError(w, r, fieldError("password", "required, 1-1024 chars"))
		return
	}
	if req.Shiki != nil && req.Shiki.AuthMode != "cookie" && req.Shiki.AuthMode != "oauth" {
		writeAPIError(w, r, errValidation("Validation failed", map[string]any{
			"errors": []map[string]string{{"field": "shiki.auth_mode", "message": "must be cookie or oauth"}},
		}))
		return
	}

	hash, ok := a.cfg.Web.Users[req.Login]
	if !ok || !VerifyPasswordHash(req.Password, hash.PasswordHash) {
		writeAPIError(w, r, errUnauthorized("Invalid credentials"))
		return
	}

	sid, err := newSessionID()
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to start session"))
		return
	}
	refresh, err := NewRefreshToken()
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to start session"))
		return
	}

	now := a.now()
	sess := &storage.AuthSession{
		SessionID:        sid,
		UserLogin:        req.Login,
		RefreshTokenHash: HashRefreshToken(refresh),
		ExpiresAt:        now.Add(a.cfg.API.RefreshTokenTTL),
	}
	if req.Shiki != nil {
		mode := req.Shiki.AuthMode
		sess.ShikiAuthMode = &mode
		sess.ShikiUsername = req.Shiki.Username
		sess.ShikiCookieSession = req.Shiki.CookieSession
		sess.ShikiAccessToken = req.Shiki.AccessToken
	}
	if err := a.store.AuthSessions.Create(r.Context(), sess); err != nil {
		writeAPIError(w, r, errInternal("Failed to persist session"))
		return
	}

	a.writeTokenPair(w, req.Login, sid, refresh)
}

// handleAuthRefresh exchanges a refresh token for a rotated pair
// (python auth_refresh): the old token dies with the hash swap.
func (a *App) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if e := decodeJSON(r, &req); e != nil {
		writeAPIError(w, r, e)
		return
	}
	if len(req.RefreshToken) < 16 || len(req.RefreshToken) > 4096 {
		writeAPIError(w, r, fieldError("refresh_token", "required, 16-4096 chars"))
		return
	}

	hash := HashRefreshToken(req.RefreshToken)
	sess, err := a.store.AuthSessions.GetByRefreshHash(r.Context(), hash)
	if err != nil {
		writeAPIError(w, r, errUnauthorized("Invalid refresh token"))
		return
	}
	now := a.now()
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(now) {
		writeAPIError(w, r, errUnauthorized("Refresh token expired or revoked"))
		return
	}

	rotated, err := NewRefreshToken()
	if err != nil {
		writeAPIError(w, r, errInternal("Failed to rotate session"))
		return
	}
	updated, err := a.store.AuthSessions.RotateRefresh(r.Context(),
		sess.SessionID, HashRefreshToken(rotated), now.Add(a.cfg.API.RefreshTokenTTL))
	if err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			writeAPIError(w, r, errUnauthorized("Session not found"))
			return
		}
		writeAPIError(w, r, errInternal("Failed to rotate session"))
		return
	}

	a.writeTokenPair(w, updated.UserLogin, updated.SessionID, rotated)
}

// handleAuthLogout revokes the calling session (python auth_logout).
func (a *App) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	sess := authSessionOf(r)
	if sess == nil {
		writeAPIError(w, r, errUnauthorized("Unauthorized"))
		return
	}
	if err := a.store.AuthSessions.Revoke(r.Context(), sess.SessionID); err != nil {
		writeAPIError(w, r, errInternal("Failed to revoke session"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAuthMe reports the authenticated principal (python auth_me).
func (a *App) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	sess := authSessionOf(r)
	if sess == nil {
		writeAPIError(w, r, errUnauthorized("Unauthorized"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"user": map[string]any{
			"login":      sess.UserLogin,
			"session_id": sess.SessionID,
		},
	})
}

// writeTokenPair renders the login/refresh success payload.
func (a *App) writeTokenPair(w http.ResponseWriter, login, sid, refresh string) {
	access := IssueAccessToken(a.secret, login, sid, a.cfg.API.TokenTTL, a.now())
	writeJSON(w, 200, map[string]any{
		"token_type":         "Bearer",
		"access_token":       access,
		"expires_in":         int64(a.cfg.API.TokenTTL / time.Second),
		"refresh_token":      refresh,
		"refresh_expires_in": int64(a.cfg.API.RefreshTokenTTL / time.Second),
		"user":               map[string]any{"login": login},
	})
}

// decodeJSON decodes a JSON body; failures become the 422
// validation_error contract (python RequestValidationError handler).
// Unknown fields are rejected, mirroring pydantic's strictness.
func decodeJSON(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody))
	if err != nil || len(body) == 0 {
		return errValidation("Validation failed", map[string]any{
			"errors": []map[string]string{{"field": "body", "message": "body is required"}},
		})
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errValidation("Validation failed", map[string]any{
			"errors": []map[string]string{{"field": "body", "message": "malformed JSON"}},
		})
	}
	return nil
}

// fieldError builds a 422 for one invalid field.
func fieldError(field, message string) *apiError {
	return errValidation("Validation failed", map[string]any{
		"errors": []map[string]string{{"field": field, "message": message}},
	})
}

// newSessionID mints a 32-hex-char session id (python uuid4().hex).
func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
