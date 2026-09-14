package shikimori

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// oauthCfg is the full OAuth2 credential set of a bearer client.
func oauthCfg() config.Shikimori {
	return config.Shikimori{
		Enabled:        true,
		AccessToken:    "at-old",
		RefreshToken:   "rt-old",
		TokenExpiresAt: time.Now().Add(time.Hour).Unix(), // far from expiry
		ClientID:       "cid",
		ClientSecret:   "csec",
	}
}

// tokenEndpoint answers a successful /oauth/token POST for the given
// grant, recording the form it received.
func tokenEndpoint(t *testing.T, mu *sync.Mutex, forms *[]url.Values) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected token-endpoint request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token content-type = %q, want form-urlencoded", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != UserAgent {
			t.Errorf("token user-agent = %q, want %q", ua, UserAgent)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csec" {
			t.Errorf("token form missing app credentials: %v", r.Form)
		}
		mu.Lock()
		*forms = append(*forms, r.Form)
		mu.Unlock()
		writeJSON(w, map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-new",
			"token_type":    "Bearer",
			"expires_in":    86400,
			"scope":         "user_rates",
			"created_at":    time.Now().Unix(),
		})
	}
}

// TestAuthorizeURL pins the authorization-code URL contract: correct
// endpoint and the exact query set including the user_rates scope (PR25 D).
func TestAuthorizeURL(t *testing.T) {
	t.Parallel()

	got := AuthorizeURL("cid", "http://127.0.0.1:8931/callback")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if want := DefaultBaseURL + "/oauth/authorize"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Errorf("authorize base = %s, want %s", u.Scheme+"://"+u.Host+u.Path, want)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "cid",
		"redirect_uri":  "http://127.0.0.1:8931/callback",
		"response_type": "code",
		"scope":         "user_rates",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
}

// TestExchangeCode pins the authorization-code exchange: form fields,
// content type, User-Agent ruling and the parsed TokenSet.
func TestExchangeCode(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c, _ := newTestClient(t, config.Shikimori{Enabled: true}, func(w http.ResponseWriter, r *http.Request) {
		tokenEndpoint(t, &mu, &forms)(w, r)
	})

	set, err := c.ExchangeCode(context.Background(), "cid", "csec", "http://127.0.0.1:8931/callback", "code-42")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if set.AccessToken != "at-new" || set.RefreshToken != "rt-new" {
		t.Errorf("TokenSet = %+v", set)
	}
	if want := time.Now().Add(86400 * time.Second).Unix(); set.ExpiresAt < want-10 || set.ExpiresAt > want+10 {
		t.Errorf("ExpiresAt = %d, want ~%d (1 day)", set.ExpiresAt, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 1 {
		t.Fatalf("token requests = %d, want 1", len(forms))
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cid",
		"client_secret": "csec",
		"redirect_uri":  "http://127.0.0.1:8931/callback",
		"code":          "code-42",
	} {
		if got := forms[0].Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
}

// TestRefreshAccessToken pins the refresh grant form.
func TestRefreshAccessToken(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c, _ := newTestClient(t, config.Shikimori{Enabled: true}, func(w http.ResponseWriter, r *http.Request) {
		tokenEndpoint(t, &mu, &forms)(w, r)
	})

	set, err := c.RefreshAccessToken(context.Background(), "cid", "csec", "rt-old")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if set.AccessToken != "at-new" {
		t.Errorf("access token = %q, want at-new", set.AccessToken)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 1 || forms[0].Get("grant_type") != "refresh_token" || forms[0].Get("refresh_token") != "rt-old" {
		t.Errorf("refresh form = %+v", forms)
	}
}

// TestBearerGet401RefreshesAndRetries pins the reactive refresh chain on
// the GET path (PR25 C): 401 with the old token -> refresh -> replay
// with the new token -> success.
func TestBearerGet401RefreshesAndRetries(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c, log := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			tokenEndpoint(t, &mu, &forms)(w, r)
		case "/api/users/whoami":
			if r.Header.Get("Authorization") != "Bearer at-new" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			writeJSON(w, map[string]any{"id": 77})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	id, err := c.GetUserID(context.Background())
	if err != nil {
		t.Fatalf("GetUserID: %v", err)
	}
	if id != 77 {
		t.Errorf("id = %d, want 77", id)
	}
	if got := log.count("/oauth/token"); got != 1 {
		t.Errorf("token refreshes = %d, want 1", got)
	}
	if got := log.count("/api/users/whoami"); got != 2 {
		t.Errorf("whoami attempts = %d, want 2 (401 + replay)", got)
	}
}

// TestBearerMutate401RefreshesAndRetries pins the reactive refresh chain
// on the mutating path plus the bearer header contract on the replay
// (Authorization only — never Cookie, never X-CSRF-Token).
func TestBearerMutate401RefreshesAndRetries(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	var posts int
	c, log := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			tokenEndpoint(t, &mu, &forms)(w, r)
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			if r.Header.Get("Authorization") != "Bearer at-new" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 55})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"})
	if err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if rateID != 55 {
		t.Errorf("rateID = %d, want 55", rateID)
	}
	if posts != 2 {
		t.Errorf("POST attempts = %d, want 2 (403 + refreshed replay)", posts)
	}
	for _, rec := range log.snapshot() {
		if rec.Path == "/api/v2/user_rates" {
			if rec.Cookie != "" {
				t.Errorf("bearer replay carried Cookie %q, want none", rec.Cookie)
			}
			if rec.CSRF != "" {
				t.Errorf("bearer replay carried X-CSRF-Token %q, want none", rec.CSRF)
			}
		}
	}
}

// TestBearerRefreshFailureDegradesToPublicReads pins the failure branch
// (PR25 C): an unrecoverable refresh logs a warning, degrades the client
// to public reads and the authed operation fails loud with
// ErrAuthRequired; public reads keep working.
func TestBearerRefreshFailureDegradesToPublicReads(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		case "/api/users/whoami":
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/animes/5":
			writeJSON(w, map[string]any{"id": 5, "name": "Public"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if _, err := c.GetUserID(context.Background()); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("GetUserID err = %v, want ErrAuthRequired", err)
	}
	if c.Mode() != "none" {
		t.Errorf("mode after failed refresh = %q, want none (degraded to public reads)", c.Mode())
	}
	if c.Authenticated() {
		t.Error("client must report unauthenticated after degradation")
	}
	// Public reads survive.
	anime, err := c.GetAnime(context.Background(), 5)
	if err != nil {
		t.Fatalf("public read after degradation: %v", err)
	}
	if anime.Name != "Public" {
		t.Errorf("anime = %+v", anime)
	}
}

// TestProactiveRefreshNearExpiry pins the five-minute window (PR25 C):
// a token expiring inside the window is refreshed BEFORE the API
// request, which then carries the fresh token.
func TestProactiveRefreshNearExpiry(t *testing.T) {
	t.Parallel()

	cfg := oauthCfg()
	cfg.TokenExpiresAt = time.Now().Add(2 * time.Minute).Unix() // inside the window

	var mu sync.Mutex
	var forms []url.Values
	c, log := newTestClient(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			tokenEndpoint(t, &mu, &forms)(w, r)
		case "/api/animes/5":
			if r.Header.Get("Authorization") != "Bearer at-new" {
				t.Errorf("api call Authorization = %q, want the refreshed token", r.Header.Get("Authorization"))
			}
			writeJSON(w, map[string]any{"id": 5, "name": "X"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if _, err := c.GetAnime(context.Background(), 5); err != nil {
		t.Fatalf("GetAnime: %v", err)
	}
	if got := log.count("/oauth/token"); got != 1 {
		t.Errorf("proactive refreshes = %d, want 1", got)
	}
	paths := log.paths()
	tokenIdx, apiIdx := -1, -1
	for i, p := range paths {
		if p == "POST /oauth/token" {
			tokenIdx = i
		}
		if p == "GET /api/animes/5" {
			apiIdx = i
		}
	}
	if tokenIdx == -1 || apiIdx == -1 || tokenIdx > apiIdx {
		t.Errorf("request order = %v, want /oauth/token before the API call", paths)
	}
}

// TestNoProactiveRefreshWhenExpiryFar pins: a token well outside the
// five-minute window never triggers a token-endpoint round-trip.
func TestNoProactiveRefreshWhenExpiryFar(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/animes/5" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": 5, "name": "X"})
	})

	if _, err := c.GetAnime(context.Background(), 5); err != nil {
		t.Fatalf("GetAnime: %v", err)
	}
	if got := log.count("/oauth/token"); got != 0 {
		t.Errorf("token requests = %d, want 0 (expiry an hour away)", got)
	}
}

// TestRefreshPersistsTokens pins the persistence hook (PR25 C/E): a
// successful refresh reports the updated section to the persister with
// the new access token, rotated refresh token and moved expiry.
func TestRefreshPersistsTokens(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var persisted []config.Shikimori
	c, _ := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			writeJSON(w, map[string]any{
				"access_token":  "at-new",
				"refresh_token": "rt-new",
				"token_type":    "Bearer",
				"expires_in":    86400,
			})
		case "/api/users/whoami":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	c.persist = func(s config.Shikimori) error {
		mu.Lock()
		persisted = append(persisted, s)
		mu.Unlock()
		return nil
	}

	_, err := c.GetUserID(context.Background())
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("GetUserID err = %v, want ErrAuthRequired", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(persisted) != 1 {
		t.Fatalf("persister calls = %d, want 1", len(persisted))
	}
	got := persisted[0]
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-new" {
		t.Errorf("persisted tokens = %+v, want the refreshed pair", got)
	}
	if want := time.Now().Add(86400 * time.Second).Unix(); got.TokenExpiresAt < want-10 {
		t.Errorf("persisted expiry = %d, want ~%d", got.TokenExpiresAt, want)
	}
	if got.ClientID != "cid" || got.ClientSecret != "csec" || !got.Enabled {
		t.Errorf("persisted section lost app credentials: %+v", got)
	}
}

// TestWithTokenPersisterOption pins the constructor option wiring.
func TestWithTokenPersisterOption(t *testing.T) {
	t.Parallel()

	var called bool
	c := New(config.Shikimori{Enabled: true, AccessToken: "t"}, newTestNet(t),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithTokenPersister(func(config.Shikimori) error { called = true; return nil }))
	if c.persist == nil {
		t.Fatal("persister not installed")
	}
	if err := c.persist(config.Shikimori{}); err != nil || !called {
		t.Errorf("persister call err=%v called=%v", err, called)
	}
}

// TestExchangeFailureSurfacesError pins fail-loud token errors: a
// non-2xx token endpoint answers a typed error, never an empty TokenSet.
func TestExchangeFailureSurfacesError(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, config.Shikimori{Enabled: true}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
	})

	set, err := c.ExchangeCode(context.Background(), "cid", "csec", "http://127.0.0.1:1/callback", "stale")
	if err == nil {
		t.Fatalf("ExchangeCode must fail on 400, got %+v", set)
	}
	if !strings.Contains(err.Error(), "invalid_grant") && !strings.Contains(err.Error(), "400") {
		t.Errorf("error text = %v, want status/body context", err)
	}
}

// TestBearerCRUDUpdateDelete pins PR25 D: the remaining user_rates
// CRUD verbs (PATCH/DELETE) ride the same bearer contract —
// Authorization only, no cookie, no CSRF bootstrap, no page fetches.
func TestBearerCRUDUpdateDelete(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, oauthCfg(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/user_rates/31":
			switch r.Method {
			case http.MethodPatch:
				writeJSON(w, map[string]any{"id": 31, "episodes": 4})
			case http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		default:
			t.Errorf("unexpected request %s (bearer mode is JSON-API only)", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	id, err := c.UpdateRate(context.Background(), 31, RateInput{Status: "watching", Episodes: intPtr(4)})
	if err != nil {
		t.Fatalf("UpdateRate: %v", err)
	}
	if id != 31 {
		t.Errorf("UpdateRate id = %d, want 31", id)
	}
	if err := c.DeleteRate(context.Background(), 31); err != nil {
		t.Fatalf("DeleteRate: %v", err)
	}

	for _, rec := range log.snapshot() {
		if rec.Auth != "Bearer at-old" {
			t.Errorf("%s %s Authorization = %q, want bearer token", rec.Method, rec.Path, rec.Auth)
		}
		if rec.Cookie != "" || rec.CSRF != "" {
			t.Errorf("%s %s carried cookie/CSRF (%q/%q), want none", rec.Method, rec.Path, rec.Cookie, rec.CSRF)
		}
	}
	for _, page := range []string{"/users/sign_in", "/"} {
		if got := log.count(page); got != 0 {
			t.Errorf("bearer mode fetched %s %d times, want 0", page, got)
		}
	}
}
