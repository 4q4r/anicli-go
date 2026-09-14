package shikimori

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// reqRecord captures one incoming request for assertions.
type reqRecord struct {
	Method string
	Path   string
	Query  string
	UA     string
	Cookie string
	Auth   string
	CSRF   string
	Body   string
}

// requestLog is a concurrency-safe recorder shared by test handlers. It
// drains the request body for capture and restores a re-readable body for
// the wrapped handler.
type requestLog struct {
	mu   sync.Mutex
	reqs []reqRecord
}

func (l *requestLog) record(r *http.Request) reqRecord {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := reqRecord{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		UA:     r.Header.Get("User-Agent"),
		Cookie: r.Header.Get("Cookie"),
		Auth:   r.Header.Get("Authorization"),
		CSRF:   r.Header.Get("X-CSRF-Token"),
		Body:   string(body),
	}
	l.mu.Lock()
	l.reqs = append(l.reqs, rec)
	l.mu.Unlock()
	return rec
}

func (l *requestLog) snapshot() []reqRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]reqRecord(nil), l.reqs...)
}

func (l *requestLog) count(path string) int {
	n := 0
	for _, r := range l.snapshot() {
		if r.Path == path {
			n++
		}
	}
	return n
}

func (l *requestLog) paths() []string {
	var out []string
	for _, r := range l.snapshot() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// writeJSON writes a JSON response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestNet builds the shared netclient with short timeouts.
func newTestNet(t *testing.T) *netclient.Client {
	t.Helper()
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	cfg.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfg, netclient.WithProvider("shikimori"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	return net
}

// newTestClient builds a Client wired to an httptest server and a
// fake-clock limiter (no real sleeping, no rate stalls).
func newTestClient(t *testing.T, cfg config.Shikimori,
	handler http.HandlerFunc,
) (*Client, *requestLog) {
	t.Helper()

	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	c := New(cfg, newTestNet(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.baseURL = srv.URL
	c.lim, _ = newTestLimiter(t)
	return c, log
}

func cookieCfg(session string) config.Shikimori {
	return config.Shikimori{Enabled: true, Session: session}
}

func bearerCfg(token string) config.Shikimori {
	return config.Shikimori{Enabled: true, AccessToken: token}
}

// mustReadBody drains and returns the request body as a string.
func mustReadBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// intPtr is the payload builder helper for optional rate fields.
func intPtr(v int) *int { return &v }

// TestClientModeSelection pins the strict auth-mode split: access token
// wins, else cookie, else unauthenticated; disabled gates everything.
func TestClientModeSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Shikimori
		want authMode
	}{
		{name: "disabled", cfg: config.Shikimori{}, want: modeDisabled},
		{name: "cookie", cfg: cookieCfg("s"), want: modeCookie},
		{name: "bearer", cfg: bearerCfg("tok"), want: modeBearer},
		{
			name: "both configured -> bearer (no cross-mode)",
			cfg:  config.Shikimori{Enabled: true, Session: "s", AccessToken: "tok"},
			want: modeBearer,
		},
		{name: "enabled without credentials", cfg: config.Shikimori{Enabled: true}, want: modeNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := New(tt.cfg, newTestNet(t), nil)
			if c.mode != tt.want {
				t.Errorf("mode = %v, want %v", c.mode, tt.want)
			}
		})
	}
}

// TestClientDisabledFailsLoud pins: Enabled=false rejects every operation
// with ErrDisabled before any network activity.
func TestClientDisabledFailsLoud(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{}, func(http.ResponseWriter, *http.Request) {
		t.Error("disabled client must not touch the network")
	})
	if _, err := c.GetUserID(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("GetUserID err = %v, want ErrDisabled", err)
	}
	if _, err := c.GetAnime(context.Background(), 1); !errors.Is(err, ErrDisabled) {
		t.Errorf("GetAnime err = %v, want ErrDisabled", err)
	}
	if got := len(log.snapshot()); got != 0 {
		t.Errorf("disabled client made %d requests, want 0", got)
	}
}

// TestClientNoCredentialsFailsAuthOperations pins: enabled but
// unauthenticated — public reads stay allowed, authed operations fail
// loud with ErrAuthRequired without touching the network.
func TestClientNoCredentialsFailsAuthOperations(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{Enabled: true}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/animes/5" {
			writeJSON(w, map[string]any{"id": 5, "name": "Public"})
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
	})

	if _, err := c.GetUserID(context.Background()); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("GetUserID err = %v, want ErrAuthRequired", err)
	}
	if _, err := c.GetUserRates(context.Background()); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("GetUserRates err = %v, want ErrAuthRequired", err)
	}
	if _, err := c.CreateRate(context.Background(), 5, RateInput{Status: "watching"}); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("CreateRate err = %v, want ErrAuthRequired", err)
	}
	// Public read still works.
	anime, err := c.GetAnime(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetAnime: %v", err)
	}
	if anime.Name != "Public" {
		t.Errorf("anime name = %q, want Public", anime.Name)
	}
	if got := log.count("/api/animes/5"); got != 1 {
		t.Errorf("public read hits = %d, want 1", got)
	}
	if got := len(log.snapshot()); got != 1 {
		t.Errorf("total requests = %v, want only the public read", log.paths())
	}
}

// TestGetUserIDCookieMode pins whoami over the cookie session: correct
// URL, headers and caching, and — critically — that a GET never bootstraps
// CSRF (no /users/sign_in, no root page fetch).
func TestGetUserIDCookieMode(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("sess42"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/whoami":
			if r.Method != http.MethodGet {
				t.Errorf("whoami method = %s, want GET", r.Method)
			}
			writeJSON(w, map[string]any{"id": 42, "nickname": "user"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	id, err := c.GetUserID(context.Background())
	if err != nil {
		t.Fatalf("GetUserID: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}

	// Second call is served from the cache: no extra whoami.
	if _, err = c.GetUserID(context.Background()); err != nil {
		t.Fatalf("GetUserID cached: %v", err)
	}
	if got := log.count("/api/users/whoami"); got != 1 {
		t.Errorf("whoami calls = %d, want 1 (cached)", got)
	}

	reqs := log.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %v, want exactly whoami", log.paths())
	}
	r := reqs[0]
	if r.Cookie != "_kawai_session=sess42" {
		t.Errorf("cookie header = %q, want _kawai_session=sess42", r.Cookie)
	}
	if r.UA != UserAgent {
		t.Errorf("user-agent = %q, want %q", r.UA, UserAgent)
	}
	if r.CSRF != "" {
		t.Errorf("GET carried X-CSRF-Token %q, want none", r.CSRF)
	}
}

// TestGetUserIDBearerMode pins Bearer JSON-API mode: Authorization header,
// no cookie, and never any cookie-flow endpoint (page fetches).
func TestGetUserIDBearerMode(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, bearerCfg("tok777"), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/whoami" {
			t.Errorf("unexpected request %s (bearer mode must never leave the JSON API)", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": 7})
	})

	id, err := c.GetUserID(context.Background())
	if err != nil {
		t.Fatalf("GetUserID: %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}

	reqs := log.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %v, want exactly whoami", log.paths())
	}
	if got := reqs[0].Auth; got != "Bearer tok777" {
		t.Errorf("authorization = %q, want Bearer tok777", got)
	}
	if reqs[0].Cookie != "" {
		t.Errorf("bearer mode sent cookie %q, want none", reqs[0].Cookie)
	}
	if reqs[0].CSRF != "" {
		t.Errorf("bearer mode sent X-CSRF-Token %q, want none", reqs[0].CSRF)
	}
}

// TestGetUserRates pins the list fetch: v2 endpoint, query parameters and
// response decoding.
func TestGetUserRates(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 42})
		case "/api/v2/user_rates":
			for _, want := range []string{"user_id=42", "target_type=Anime", "limit=1000"} {
				if !strings.Contains(r.URL.RawQuery, want) {
					t.Errorf("query %q missing %q", r.URL.RawQuery, want)
				}
			}
			writeJSON(w, []map[string]any{
				{"id": 1, "user_id": 42, "target_id": 100, "target_type": "Anime",
					"score": 9, "status": "watching", "episodes": 3, "rewatches": 0},
				{"id": 2, "user_id": 42, "target_id": 200, "target_type": "Anime",
					"status": "plan_to_watch"},
			})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if _, err := c.GetUserID(context.Background()); err != nil {
		t.Fatalf("GetUserID: %v", err)
	}
	rates, err := c.GetUserRates(context.Background())
	if err != nil {
		t.Fatalf("GetUserRates: %v", err)
	}
	if len(rates) != 2 {
		t.Fatalf("rates = %d, want 2", len(rates))
	}
	if rates[0].ID != 1 || rates[0].TargetID != 100 || rates[0].Status != "watching" || rates[0].Episodes != 3 {
		t.Errorf("rate[0] = %+v", rates[0])
	}
	if rates[1].Status != "plan_to_watch" {
		t.Errorf("rate[1].Status = %q, want plan_to_watch", rates[1].Status)
	}
	if got := log.count("/api/v2/user_rates"); got != 1 {
		t.Errorf("user_rates calls = %d, want 1", got)
	}
}

// TestCreateRateCookieModeCSRFBootstrap pins the full cookie mutating flow:
// CSRF bootstrapped from /users/sign_in first, token + cookie + UA on the
// POST, payload wrapped as {"user_rate":{...}} with user/target fields,
// and the returned rate id.
func TestCreateRateCookieModeCSRFBootstrap(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("sess1"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok-signin"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 42})
		case "/api/v2/user_rates":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			var payload struct {
				UserRate map[string]any `json:"user_rate"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode payload: %v", err)
			}
			for _, want := range []string{"user_id", "target_id", "target_type"} {
				if _, ok := payload.UserRate[want]; !ok {
					t.Errorf("payload missing %q: %+v", want, payload.UserRate)
				}
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 777})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.CreateRate(context.Background(), 100, RateInput{Status: "watching", Episodes: intPtr(1)})
	if err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if rateID != 777 {
		t.Errorf("rateID = %d, want 777", rateID)
	}

	// Ordering: sign_in bootstrap strictly before the POST.
	paths := log.paths()
	postIdx, signInIdx := -1, -1
	for i, p := range paths {
		if p == "POST /api/v2/user_rates" {
			postIdx = i
		}
		if p == "GET /users/sign_in" {
			signInIdx = i
		}
	}
	if signInIdx == -1 || postIdx == -1 || signInIdx > postIdx {
		t.Errorf("request order = %v, want sign_in before POST", paths)
	}

	var post reqRecord
	for _, r := range log.snapshot() {
		if r.Path == "/api/v2/user_rates" {
			post = r
		}
	}
	if post.CSRF != "tok-signin" {
		t.Errorf("X-CSRF-Token = %q, want tok-signin", post.CSRF)
	}
	if post.Cookie != "_kawai_session=sess1" {
		t.Errorf("cookie = %q, want session cookie", post.Cookie)
	}
	if post.UA != UserAgent {
		t.Errorf("user-agent = %q, want %q", post.UA, UserAgent)
	}
	if !strings.Contains(post.Body, `"status":"watching"`) || !strings.Contains(post.Body, `"episodes":1`) {
		t.Errorf("payload = %q, want status+episodes", post.Body)
	}
	if strings.Contains(post.Body, `"score"`) || strings.Contains(post.Body, `"rewatches"`) {
		t.Errorf("payload = %q, zero-valued score/rewatches must be omitted", post.Body)
	}
}

// TestCSRFBootstrapRootFallback pins: when /users/sign_in carries no
// csrf meta tag, the bootstrap falls back to the root page.
func TestCSRFBootstrapRootFallback(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><title>no token here</title></head></html>`))
		case "/":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok-root"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 5})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if _, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"}); err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if got := log.count("/users/sign_in"); got != 1 {
		t.Errorf("sign_in fetches = %d, want 1", got)
	}
	if got := log.count("/"); got != 1 {
		t.Errorf("root fetches = %d, want 1 (fallback)", got)
	}
	var post reqRecord
	for _, r := range log.snapshot() {
		if r.Path == "/api/v2/user_rates" {
			post = r
		}
	}
	if post.CSRF != "tok-root" {
		t.Errorf("X-CSRF-Token = %q, want tok-root", post.CSRF)
	}
}

// TestMutating401RebootsCSRFAndRetriesOnce pins the 401/403-only CSRF
// retry ladder: re-bootstrap the token, replay the request once, then
// fail loud with ErrAuthRequired.
func TestMutating401RebootsCSRFAndRetriesOnce(t *testing.T) {
	t.Parallel()

	var posts int
	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			// Fresh token on every bootstrap.
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok-a"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	_, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("CreateRate err = %v, want ErrAuthRequired", err)
	}
	if posts != 2 {
		t.Errorf("POST attempts = %d, want 2 (original + one CSRF retry)", posts)
	}
	if got := log.count("/users/sign_in"); got != 2 {
		t.Errorf("sign_in bootstraps = %d, want 2 (initial + retry)", got)
	}
}

// TestMutating401RetrySucceeds pins the happy half of the ladder: 401,
// re-bootstrap, replay, success.
func TestMutating401RetrySucceeds(t *testing.T) {
	t.Parallel()

	var posts int
	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok-a"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			if posts == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 11})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"})
	if err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if rateID != 11 {
		t.Errorf("rateID = %d, want 11", rateID)
	}
	if posts != 2 {
		t.Errorf("POST attempts = %d, want 2", posts)
	}
}

// TestCreateRate422PlannedNormalization pins the 422 handler: a rate
// whose status is "planned" is retried exactly once with the canonical
// "plan_to_watch", and 422 never triggers a CSRF re-bootstrap.
func TestCreateRate422PlannedNormalization(t *testing.T) {
	t.Parallel()

	var posts int
	var bodies []string
	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			bodies = append(bodies, mustReadBody(t, r))
			if posts == 1 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"error":"Status is not included in the list"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 21})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.CreateRate(context.Background(), 9, RateInput{Status: "planned"})
	if err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if rateID != 21 {
		t.Errorf("rateID = %d, want 21", rateID)
	}
	if posts != 2 {
		t.Errorf("POST attempts = %d, want 2 (planned -> plan_to_watch retry)", posts)
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], `"planned"`) {
		t.Errorf("first body = %q, want status planned", bodies[0])
	}
	if !strings.Contains(bodies[1], `"plan_to_watch"`) {
		t.Errorf("retry body = %q, want status plan_to_watch", bodies[1])
	}
	// The ruling: CSRF retry is 401/403-only — 422 must not re-bootstrap.
	if got := log.count("/users/sign_in"); got != 1 {
		t.Errorf("sign_in bootstraps = %d, want 1 (422 never re-bootstraps)", got)
	}
}

// TestCreateRate422OtherFailsLoud pins: a 422 unrelated to the planned
// status maps to ErrUnprocessable with exactly one attempt.
func TestCreateRate422OtherFailsLoud(t *testing.T) {
	t.Parallel()

	var posts int
	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"Episodes must be <= 12"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	_, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching", Episodes: intPtr(99)})
	if !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("CreateRate err = %v, want ErrUnprocessable", err)
	}
	if posts != 1 {
		t.Errorf("POST attempts = %d, want 1 (no retry for non-planned 422)", posts)
	}
	if got := log.count("/users/sign_in"); got != 1 {
		t.Errorf("sign_in bootstraps = %d, want 1", got)
	}
}

// TestUpdateRatePatches pins the PATCH update path: URL carries the rate
// id, payload is user_rate-wrapped, response id returned.
func TestUpdateRatePatches(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/v2/user_rates/55":
			if r.Method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", r.Method)
			}
			writeJSON(w, map[string]any{"id": 55, "episodes": 4})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.UpdateRate(context.Background(), 55, RateInput{Episodes: intPtr(4), Status: "watching"})
	if err != nil {
		t.Fatalf("UpdateRate: %v", err)
	}
	if rateID != 55 {
		t.Errorf("rateID = %d, want 55", rateID)
	}
	if got := log.count("/api/v2/user_rates/55"); got != 1 {
		t.Errorf("PATCH calls = %d, want 1", got)
	}
}

// TestDeleteRate pins the DELETE path (204 No Content).
func TestDeleteRate(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/v2/user_rates/55":
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if err := c.DeleteRate(context.Background(), 55); err != nil {
		t.Fatalf("DeleteRate: %v", err)
	}
	if got := log.count("/api/v2/user_rates/55"); got != 1 {
		t.Errorf("DELETE calls = %d, want 1", got)
	}
}

// TestAddAnimeToListNormalizesPlanned pins the sugar API: status
// canonicalization happens before the wire (planned never leaves the
// client in AddAnimeToList).
func TestAddAnimeToListNormalizesPlanned(t *testing.T) {
	t.Parallel()

	var body string
	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 3})
		case "/api/v2/user_rates":
			body = mustReadBody(t, r)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 31})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	rateID, err := c.AddAnimeToList(context.Background(), 77, "planned")
	if err != nil {
		t.Fatalf("AddAnimeToList: %v", err)
	}
	if rateID != 31 {
		t.Errorf("rateID = %d, want 31", rateID)
	}
	if !strings.Contains(body, `"plan_to_watch"`) {
		t.Errorf("body = %q, want canonical plan_to_watch on first attempt", body)
	}
}

// TestBearerModeNeverTouchesCookieEndpoints pins the strict mode split
// end-to-end on the mutating path: bearer POST carries only the
// Authorization header, no CSRF bootstrap, no cookie.
func TestBearerModeNeverTouchesCookieEndpoints(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, bearerCfg("btok"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 8})
		case "/api/v2/user_rates":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 88})
		default:
			t.Errorf("unexpected request %s (bearer mode is JSON-API only)", r.URL.Path)
		}
	})

	rateID, err := c.CreateRate(context.Background(), 12, RateInput{Status: "watching"})
	if err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if rateID != 88 {
		t.Errorf("rateID = %d, want 88", rateID)
	}
	for _, forbidden := range []string{"/users/sign_in", "/"} {
		if got := log.count(forbidden); got != 0 {
			t.Errorf("bearer mode fetched %s %d times, want 0", forbidden, got)
		}
	}
	var post reqRecord
	for _, r := range log.snapshot() {
		if r.Path == "/api/v2/user_rates" {
			post = r
		}
	}
	if post.CSRF != "" {
		t.Errorf("bearer POST carried X-CSRF-Token %q, want none", post.CSRF)
	}
	if post.Cookie != "" {
		t.Errorf("bearer POST carried Cookie %q, want none", post.Cookie)
	}
	if post.Auth != "Bearer btok" {
		t.Errorf("bearer POST Authorization = %q, want Bearer btok", post.Auth)
	}
}

// TestCSRFBootstrapFailureWarnsOnce pins the single-warning ruling: with
// no token anywhere, mutating requests still go out (empty token), the
// warning is logged once, and the server's verdict stands.
func TestCSRFBootstrapFailureWarnsOnce(t *testing.T) {
	t.Parallel()

	var posts int
	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in", "/":
			_, _ = w.Write([]byte(`<html><head></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			posts++
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 99})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	if _, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"}); err != nil {
		t.Fatalf("CreateRate: %v", err)
	}
	if posts != 1 {
		t.Errorf("POST attempts = %d, want 1 (no 401, no retry)", posts)
	}
	if c.csrfWarnings != 1 {
		t.Errorf("csrf warnings = %d, want exactly 1", c.csrfWarnings)
	}
}

// TestRateLimitedResponsePenalizesLimiter pins: a surfaced 429 (after the
// netclient's internal retries are exhausted) wraps ErrRateLimited and
// penalizes the limiter so the next request backs off.
func TestRateLimitedResponsePenalizesLimiter(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/sign_in":
			_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="tok"></head></html>`))
		case "/api/users/whoami":
			writeJSON(w, map[string]any{"id": 1})
		case "/api/v2/user_rates":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})

	_, err := c.CreateRate(context.Background(), 9, RateInput{Status: "watching"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("CreateRate err = %v, want ErrRateLimited", err)
	}

	c.lim.mu.Lock()
	penalty := c.lim.penalty
	c.lim.mu.Unlock()
	if penalty.IsZero() {
		t.Error("limiter penalty not set after surfaced 429")
	}
}

// TestProviderErrorPassthrough pins 404 mapping onto the shared taxonomy.
func TestProviderErrorPassthrough(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, cookieCfg("s"), func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	_, err := c.GetAnime(context.Background(), 404)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("GetAnime err = %v, want contracts.ErrNotFound", err)
	}
}

// TestWhoAmIReturnsNickname pins the combined identity probe (PR26):
// one whoami round-trip resolves id + nickname, both cached and shared
// with GetUserID; an absent nickname decodes as "" (callers render the
// id fallback).
func TestWhoAmIReturnsNickname(t *testing.T) {
	t.Parallel()

	t.Run("nickname decoded and cached", func(t *testing.T) {
		t.Parallel()
		c, log := newTestClient(t, cookieCfg("sess42"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"id": 42, "nickname": "kawai-fan"})
		})

		id, nick, err := c.WhoAmI(context.Background())
		if err != nil {
			t.Fatalf("WhoAmI: %v", err)
		}
		if id != 42 || nick != "kawai-fan" {
			t.Errorf("WhoAmI = (%d, %q), want (42, kawai-fan)", id, nick)
		}

		// The cache is shared: GetUserID after WhoAmI performs no
		// second round-trip.
		if _, err = c.GetUserID(context.Background()); err != nil {
			t.Fatalf("GetUserID after WhoAmI: %v", err)
		}
		if got := log.count("/api/users/whoami"); got != 1 {
			t.Errorf("whoami calls = %d, want 1 (shared cache)", got)
		}
	})

	t.Run("empty nickname decodes as empty", func(t *testing.T) {
		t.Parallel()
		c, _ := newTestClient(t, bearerCfg("tok"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"id": 7})
		})
		id, nick, err := c.WhoAmI(context.Background())
		if err != nil {
			t.Fatalf("WhoAmI: %v", err)
		}
		if id != 7 || nick != "" {
			t.Errorf("WhoAmI = (%d, %q), want (7, \"\")", id, nick)
		}
	})
}
