package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware chain of the API face: request id (trace_id), slog request
// logging, panic recovery and the bearer guard — the mandated subset of
// the python guardrails (semaphore/timeout/CORS wrappers are documented
// as out of scope for the Go port).

type ctxKey int

const (
	ctxTraceID ctxKey = iota
	ctxAuthSession
)

// publicPaths are reachable without a bearer token (python
// is_public_endpoint set).
var publicPaths = map[string]bool{
	"/api/v1/health":       true,
	"/api/v1/auth/login":   true,
	"/api/v1/auth/refresh": true,
}

// newTraceID mirrors python uuid4().hex (32 hex chars).
func newTraceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand never fails on supported platforms; fall back to
		// a time-derived id rather than panicking mid-request.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000"))) +
			hex.EncodeToString([]byte("anicli"))
	}
	return hex.EncodeToString(buf)
}

// traceIDOf returns the request's trace id (empty when unset).
func traceIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxTraceID).(string); ok {
		return v
	}
	return ""
}

// authSessionOf returns the authenticated session (nil when the guard
// did not run or the route is public).
func authSessionOf(r *http.Request) *authSession {
	if v, ok := r.Context().Value(ctxAuthSession).(*authSession); ok {
		return v
	}
	return nil
}

// withTraceID assigns the request id: caller-supplied X-Trace-Id or a
// fresh one; decorates responses with the shared no-store headers
// (python guardrail response headers).
func (a *App) withTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = newTraceID()
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Trace-Id", traceID)

		ctx := context.WithValue(r.Context(), ctxTraceID, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withLogging emits one slog line per request (method, path, status,
// duration, trace id).
func (a *App) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Info("api request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", traceIDOf(r),
		)
	})
}

// withRecover converts panics into the unified 500 error contract
// (python _unexpected_exception_handler).
func (a *App) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.log.Error("api panic", "panic", rec, "trace_id", traceIDOf(r), "stack", string(debug.Stack()))
				writeAPIError(w, r, errInternal("Internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withBearerGuard enforces the token auth on every /api/v1 route except
// the public set (python _guardrails auth_enabled branch, always on in
// the Go port — the off-switch collapsed into api.enabled).
func (a *App) withBearerGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !apiPath(r.URL.Path) || publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		token := parseBearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeAPIError(w, r, errUnauthorized("Missing bearer access token"))
			return
		}

		claims, err := DecodeAccessToken(a.secret, token, a.now())
		if err != nil {
			writeAPIError(w, r, errUnauthorized("Invalid access token format"))
			return
		}

		sess, err := a.store.AuthSessions.GetBySessionID(r.Context(), claims.SessionID)
		if err != nil {
			writeAPIError(w, r, errUnauthorized("Session not found"))
			return
		}
		now := a.now()
		if sess.RevokedAt != nil || !sess.ExpiresAt.After(now) {
			writeAPIError(w, r, errUnauthorized("Session expired or revoked"))
			return
		}
		if sess.UserLogin != claims.Subject {
			writeAPIError(w, r, errUnauthorized("Token subject mismatch"))
			return
		}

		ctx := context.WithValue(r.Context(), ctxAuthSession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiPath reports whether the path belongs to the versioned API face.
func apiPath(path string) bool {
	return len(path) >= len("/api/v1/") && path[:len("/api/v1/")] == "/api/v1/" ||
		path == "/api/v1"
}

// parseBearerToken extracts the bearer token from an Authorization
// header value (python _parse_bearer_token; case-insensitive scheme).
func parseBearerToken(headerValue string) string {
	if headerValue == "" {
		return ""
	}
	if len(headerValue) < 8 || !stringsEqualFold(headerValue[:7], "bearer ") {
		return ""
	}
	token := trimSpaces(headerValue[7:])
	if token == "" {
		return ""
	}
	return token
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// stringsEqualFold is a tiny ASCII case-insensitive compare (avoids
// pulling strings into the hot path for one comparison).
func stringsEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func trimSpaces(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
