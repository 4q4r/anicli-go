package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"
)

const (
	testLogin = "alice"
	testPass  = "correct horse battery staple"
	testHash  = pyVector1Hash
	testShiki = "pbkdf2-irrelevant"
)

// fakeProvider is a registry member for tests (episodes/resolve flows
// live in their own test files).
type fakeProvider struct {
	contracts.Provider
	id string
}

func (f fakeProvider) ID() string                       { return f.id }
func (f fakeProvider) Name() string                     { return f.id }
func (f fakeProvider) BaseURL() string                  { return "https://" + f.id + ".example" }
func (f fakeProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

// newTestApp builds an App wired to an in-memory store, a registry with
// one fake provider and no shikimori.
func newTestApp(t *testing.T) *App {
	t.Helper()

	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := providers.NewEmptyRegistry()
	if err := reg.Register(fakeProvider{id: "fake"}); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}

	cfg := config.Default()
	cfg.API.Enabled = true
	cfg.API.TokenTTL = 15 * time.Minute
	cfg.API.RefreshTokenTTL = 24 * time.Hour
	cfg.API.AuthSecret = "test-secret"
	cfg.Web.Users = map[string]config.WebUser{
		testLogin: {PasswordHash: testHash},
	}

	app, err := NewApp(Config{
		Settings: cfg,
		Store:    store,
		Registry: reg,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return app
}

func doJSON(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var payload map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s %s: response is not JSON (%d): %q", method, target, rec.Code, rec.Body.String())
		}
	}
	return rec, payload
}

func bear(hdr map[string]string, token string) map[string]string {
	out := map[string]string{}
	for k, v := range hdr {
		out[k] = v
	}
	out["Authorization"] = "Bearer " + token
	return out
}

func loginBody(password, shikiMode string) string {
	body := fmt.Sprintf(`{"login": %q, "password": %q`, testLogin, password)
	if shikiMode != "" {
		body += fmt.Sprintf(`, "shiki": {"auth_mode": %q, "username": "alice-shiki", "cookie_session": "kawai-cookie"}`, shikiMode)
	}
	return body + "}"
}

func TestAuthLoginWrongPassword(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", loginBody("wrong", ""), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("error payload missing: %v", payload)
	}
	if errObj["code"] != "unauthorized" {
		t.Fatalf("code = %v, want unauthorized", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); msg != "Invalid credentials" {
		t.Fatalf("message = %v", msg)
	}
	traceID, _ := errObj["trace_id"].(string)
	if traceID == "" {
		t.Fatal("trace_id must be present in the error contract")
	}
	if rec.Header().Get("X-Trace-Id") == "" {
		t.Fatal("X-Trace-Id response header must be set")
	}
}

func TestAuthFullLifecycle(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	// 1. Login with the correct password.
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", loginBody(testPass, "cookie"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d: %v", rec.Code, payload)
	}
	if payload["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v", payload["token_type"])
	}
	access1, _ := payload["access_token"].(string)
	refresh1, _ := payload["refresh_token"].(string)
	if access1 == "" || refresh1 == "" {
		t.Fatalf("tokens missing: %v", payload)
	}
	if expIn, _ := payload["expires_in"].(float64); expIn != 900 { // 15m
		t.Fatalf("expires_in = %v, want 900", expIn)
	}
	if rexp, _ := payload["refresh_expires_in"].(float64); rexp != 86400 { // 24h
		t.Fatalf("refresh_expires_in = %v, want 86400", rexp)
	}
	if user, _ := payload["user"].(map[string]any); user["login"] != testLogin {
		t.Fatalf("user = %v", payload["user"])
	}

	// 2. Guard rejects missing/malformed bearer tokens.
	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without token: %d", rec.Code)
	}
	rec, payload = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", "", map[string]string{"Authorization": "Bearer garbage"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me with garbage token: %d", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["message"] != "Invalid access token format" && errObj["message"] != "Invalid access token signature" {
		t.Fatalf("unexpected guard message: %v", errObj["message"])
	}

	// 3. me with the fresh access token.
	rec, payload = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", "", bear(nil, access1))
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d: %v", rec.Code, payload)
	}
	user, _ := payload["user"].(map[string]any)
	if user["login"] != testLogin {
		t.Fatalf("me user = %v", user)
	}
	if sid, _ := user["session_id"].(string); sid == "" {
		t.Fatal("me must expose session_id")
	}

	// 4. Refresh rotates: old refresh dies, new pair works.
	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token": %q}`, refresh1), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d: %v", rec.Code, payload)
	}
	access2, _ := payload["access_token"].(string)
	refresh2, _ := payload["refresh_token"].(string)
	if access2 == "" || refresh2 == "" || refresh2 == refresh1 {
		t.Fatalf("rotation broken: %v", payload)
	}

	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token": %q}`, refresh1), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old refresh must be invalid after rotation, got %d", rec.Code)
	}

	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token": %q}`, refresh2), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotated refresh must work, got %d: %v", rec.Code, payload)
	}
	access3, _ := payload["access_token"].(string)

	// 5. Logout revokes the session.
	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/logout", "", bear(nil, access3))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d: %v", rec.Code, payload)
	}
	if ok, _ := payload["ok"].(bool); !ok {
		t.Fatalf("logout body = %v", payload)
	}

	rec, payload = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", "", bear(nil, access3))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("access token of revoked session must fail, got %d", rec.Code)
	}
	errObj, _ = payload["error"].(map[string]any)
	if errObj["message"] != "Session expired or revoked" {
		t.Fatalf("revocation message = %v", errObj["message"])
	}

	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token": %q}`, refresh2), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh of revoked session must fail, got %d", rec.Code)
	}
}

func TestAuthPublicRoutesWithoutToken(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	if ok, _ := payload["ok"].(bool); !ok {
		t.Fatalf("health body = %v", payload)
	}
	if payload["database_backend"] != "sqlite" {
		t.Fatalf("database_backend = %v", payload["database_backend"])
	}
	if n, _ := payload["provider_count"].(float64); n != 1 {
		t.Fatalf("provider_count = %v, want 1", payload["provider_count"])
	}

	// refresh is public (verifies its own token instead of a bearer).
	// pydantic Field(min_length=16): short tokens fail validation 422.
	rec, _ = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token": "short"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refresh with short token = %d, want 422 (pydantic min_length)", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		`{"refresh_token": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh with unknown token = %d, want 401", rec.Code)
	}

	// Every other /api/v1 route requires the bearer.
	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/providers", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("providers without token = %d, want 401", rec.Code)
	}
}

func TestAuthLoginValidationErrors(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", `{`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed body = %d, want 422", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "validation_error" {
		t.Fatalf("code = %v", errObj["code"])
	}

	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", `{"password": "x"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing login = %d, want 422", rec.Code)
	}

	// Unknown user is unauthorized (not 404) — no user enumeration.
	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", `{"login":"eve","password":"x"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d, want 401", rec.Code)
	}
}

func TestAuthPerUserShikimoriScoping(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	// Session created WITH shiki credentials must expose them to the
	// request-scoped client builder.
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", loginBody(testPass, "cookie"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	access, _ := payload["access_token"].(string)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.URL.RawQuery = "q=test"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("search with shiki-scoped session = %d: %s", rec2.Code, rec2.Body.String())
	}

	// The scoped session must carry the shiki fields through storage.
	claims := decodeForTest(t, access)
	sess, err := app.store.AuthSessions.GetBySessionID(context.Background(), claims.SessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if sess.ShikiCookieSession == nil || *sess.ShikiCookieSession != "kawai-cookie" {
		t.Fatalf("shiki cookie not persisted: %+v", sess)
	}
	if sess.ShikiUsername == nil || *sess.ShikiUsername != "alice-shiki" {
		t.Fatalf("shiki username not persisted: %+v", sess)
	}
	if sess.ShikiAuthMode == nil || *sess.ShikiAuthMode != "cookie" {
		t.Fatalf("shiki mode not persisted: %+v", sess)
	}
}

func decodeForTest(t *testing.T, token string) *accessClaims {
	t.Helper()
	claims, err := DecodeAccessToken([]byte("test-secret"), token, time.Now())
	if err != nil {
		t.Fatalf("decode access token: %v", err)
	}
	return claims
}

// guard against accidental test-wide imports
var _ = bytes.MinRead
