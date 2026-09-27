package mal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// bearerCfg is the full OAuth2 credential set of a bearer client.
func bearerCfg() config.MAL {
	return config.MAL{
		Enabled:        true,
		AccessToken:    "at-old",
		RefreshToken:   "rt-old",
		TokenExpiresAt: time.Now().Add(time.Hour).Unix(), // far from expiry
		ClientID:       "cid",
		ClientSecret:   "csec",
	}
}

// writeJSON is the httptest helper for API replies.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// tokenEndpoint answers a successful /v1/oauth2/token POST, recording
// every form it received.
func tokenEndpoint(t *testing.T, mu *sync.Mutex, forms *[]url.Values) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth2/token" {
			t.Errorf("unexpected token-endpoint request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token content-type = %q, want form-urlencoded", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		mu.Lock()
		*forms = append(*forms, r.Form)
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"token_type":    "Bearer",
			"expires_in":    2415600,
			"access_token":  "at-new",
			"refresh_token": "rt-new",
		})
	}
}

// newTestClient builds a client pointed at the httptest servers: api is
// the API-v2 base, auth the myanimelist.net OAuth base (nil reuses api).
func newTestClient(t *testing.T, cfg config.MAL, api, auth http.Handler) *Client {
	t.Helper()
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	oauthBase := apiSrv.URL
	if auth != nil {
		authSrv := httptest.NewServer(auth)
		t.Cleanup(authSrv.Close)
		oauthBase = authSrv.URL
	}
	net, err := netclient.New(config.Default().Network, netclient.WithProvider("mal"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	return New(cfg, apiSrv.URL, oauthBase, net, WithLogger(slog.New(slog.DiscardHandler)))
}

// apiRecorder captures every API request's bearer header + form.
type apiRecorder struct {
	mu      sync.Mutex
	bearers []string
	forms   []url.Values
}

func (rec *apiRecorder) record(r *http.Request) {
	_ = r.ParseForm()
	rec.mu.Lock()
	rec.bearers = append(rec.bearers, r.Header.Get("Authorization"))
	rec.forms = append(rec.forms, r.Form)
	rec.mu.Unlock()
}

func TestNewCodeVerifier(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 32 {
		v, err := NewCodeVerifier()
		if err != nil {
			t.Fatalf("NewCodeVerifier: %v", err)
		}
		// The official constraint: 43-128 characters (plain method).
		if l := len(v); l < 43 || l > 128 {
			t.Fatalf("verifier length = %d, want within [43,128]", l)
		}
		if seen[v] {
			t.Fatal("verifier repeated across calls; must be unique per flow")
		}
		seen[v] = true
	}
}

func TestOAuthStateRoundTrip(t *testing.T) {
	t.Parallel()

	state, err := NewOAuthState()
	if err != nil {
		t.Fatalf("NewOAuthState: %v", err)
	}
	if state == "" {
		t.Fatal("state is empty")
	}
	if err := VerifyOAuthState(state, state); err != nil {
		t.Errorf("VerifyOAuthState(match): %v", err)
	}
	if err := VerifyOAuthState(state, state+"x"); err == nil {
		t.Error("VerifyOAuthState(mismatch): expected an error")
	}
	if err := VerifyOAuthState(state, ""); err == nil {
		t.Error("VerifyOAuthState(empty): expected an error")
	}
}

func TestAuthorizeURL(t *testing.T) {
	t.Parallel()

	raw := AuthorizeURL("cid-1", "http://127.0.0.1:9009/callback", "st-1", "challenge-1")
	const wantBase = OAuthBaseURL + "/v1/oauth2/authorize?"
	if !strings.HasPrefix(raw, wantBase) {
		t.Fatalf("authorize url = %q, want prefix %q", raw, wantBase)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(raw, wantBase))
	if err != nil {
		t.Fatalf("parse authorize query: %v", err)
	}
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "cid-1",
		"redirect_uri":          "http://127.0.0.1:9009/callback",
		"state":                 "st-1",
		"code_challenge":        "challenge-1",
		"code_challenge_method": "plain", // the only method MAL supports
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestClientModeDispatch(t *testing.T) {
	t.Parallel()

	disabled := New(config.MAL{}, APIBaseURL, OAuthBaseURL, nil)
	if disabled.Mode() != "disabled" || disabled.Authenticated() {
		t.Errorf("empty cfg: mode=%s authed=%v, want disabled/unauthenticated", disabled.Mode(), disabled.Authenticated())
	}

	public := New(config.MAL{Enabled: true}, APIBaseURL, OAuthBaseURL, nil)
	if public.Mode() != "none" || public.Authenticated() {
		t.Errorf("enabled-only cfg: mode=%s authed=%v, want none/unauthenticated", public.Mode(), public.Authenticated())
	}

	bearer := New(bearerCfg(), APIBaseURL, OAuthBaseURL, nil)
	if bearer.Mode() != "bearer" || !bearer.Authenticated() {
		t.Errorf("token cfg: mode=%s authed=%v, want bearer/authenticated", bearer.Mode(), bearer.Authenticated())
	}
}

func TestExchangeCode(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c := newTestClient(t, config.MAL{Enabled: true, ClientID: "cid", ClientSecret: "csec"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("API server must not be hit by the token exchange")
			w.WriteHeader(http.StatusTeapot)
		}), http.HandlerFunc(tokenEndpoint(t, &mu, &forms)))

	set, err := c.ExchangeCode(context.Background(),
		"http://127.0.0.1:9009/callback", "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if set.AccessToken != "at-new" || set.RefreshToken != "rt-new" {
		t.Errorf("tokens = %q/%q, want at-new/rt-new", set.AccessToken, set.RefreshToken)
	}
	if set.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt = %d, want in the future (from expires_in)", set.ExpiresAt)
	}
	if len(forms) != 1 {
		t.Fatalf("token endpoint forms = %d, want 1", len(forms))
	}
	f := forms[0]
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cid",
		"client_secret": "csec",
		"code":          "the-code",
		"redirect_uri":  "http://127.0.0.1:9009/callback",
		"code_verifier": "the-verifier",
	} {
		if f.Get(k) != want {
			t.Errorf("form %s = %q, want %q", k, f.Get(k), want)
		}
	}
}

func TestExchangeCodeSkipsEmptySecret(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c := newTestClient(t, config.MAL{Enabled: true, ClientID: "cid"},
		nil, http.HandlerFunc(tokenEndpoint(t, &mu, &forms)))

	if _, err := c.ExchangeCode(context.Background(), "http://127.0.0.1:9009/callback", "c", "v"); err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if forms[0].Has("client_secret") {
		t.Error("client_secret must be omitted when the app has none (official Scheme-2 rule)")
	}
}

func TestRefreshAccessToken(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forms []url.Values
	c := newTestClient(t, config.MAL{
		Enabled: true, ClientID: "cid", ClientSecret: "csec", RefreshToken: "rt-old",
	}, nil, http.HandlerFunc(tokenEndpoint(t, &mu, &forms)))

	set, err := c.RefreshAccessToken(context.Background())
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if set.AccessToken != "at-new" {
		t.Errorf("access token = %q, want at-new", set.AccessToken)
	}
	if len(forms) != 1 {
		t.Fatalf("token endpoint forms = %d, want 1", len(forms))
	}
	f := forms[0]
	if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "rt-old" {
		t.Errorf("refresh form = %v", f)
	}
	if f.Get("client_id") != "cid" || f.Get("client_secret") != "csec" {
		t.Errorf("refresh form missing app credentials: %v", f)
	}
	// The refreshed pair lands in the live config (the persister reads
	// it from there).
	if got := c.Config(); got.AccessToken != "at-new" || got.RefreshToken != "rt-new" {
		t.Errorf("live config after refresh = %q/%q, want at-new/rt-new", got.AccessToken, got.RefreshToken)
	}
}

// TestUpdateMyListStatusRefreshesOn401 pins the reactive refresh ladder
// (PR25 C parity): an expired-token 401 refreshes the pair and replays
// the update exactly once with the fresh bearer.
func TestUpdateMyListStatusRefreshesOn401(t *testing.T) {
	t.Parallel()

	rec := &apiRecorder{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path != "/v2/anime/123/my_list_status" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.Header.Get("Authorization") == "Bearer at-old" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer at-new" {
			t.Errorf("replay bearer = %q, want the refreshed token", r.Header.Get("Authorization"))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":               "watching",
			"num_episodes_watched": 5,
		})
	})

	var mu sync.Mutex
	var forms []url.Values
	c := newTestClient(t, bearerCfg(), api, http.HandlerFunc(tokenEndpoint(t, &mu, &forms)))

	err := c.UpdateMyListStatus(context.Background(), 123, ListInput{
		Status: "watching", NumWatchedEpisodes: 5,
	})
	if err != nil {
		t.Fatalf("UpdateMyListStatus: %v", err)
	}
	if len(rec.forms) != 2 {
		t.Fatalf("API forms = %d, want 2 (initial + one replay)", len(rec.forms))
	}
	f := rec.forms[1]
	if f.Get("status") != "watching" || f.Get("num_watched_episodes") != "5" {
		t.Errorf("replay form = %v", f)
	}
}

// TestUpdateMyListStatusOmitsZeroFields pins the conditional-omission
// ruling: an unset episode counter and empty status never ride the form.
func TestUpdateMyListStatusOmitsZeroFields(t *testing.T) {
	t.Parallel()

	rec := &apiRecorder{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		writeJSON(w, http.StatusOK, map[string]any{"status": "watching"})
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	if err := c.UpdateMyListStatus(context.Background(), 7, ListInput{}); err != nil {
		t.Fatalf("UpdateMyListStatus: %v", err)
	}
	f := rec.forms[0]
	if len(f) != 0 {
		t.Errorf("form = %v, want empty (nothing specified)", f)
	}

	// The rewatching flag rides only when set.
	if err := c.UpdateMyListStatus(context.Background(), 7, ListInput{Status: "watching", IsRewatching: true}); err != nil {
		t.Fatalf("UpdateMyListStatus(rewatching): %v", err)
	}
	f = rec.forms[1]
	if f.Get("is_rewatching") != "true" {
		t.Errorf("is_rewatching = %q, want true", f.Get("is_rewatching"))
	}
}

// TestClientDegradesAfterFailedRefresh pins: a rejected token while the
// refresh also fails surfaces an auth-class error and no silent fake
// success.
func TestClientDegradesAfterFailedRefresh(t *testing.T) {
	t.Parallel()

	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})
	// 400 on the token endpoint: non-retriable, so the refresh fails
	// fast.
	auth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	c := newTestClient(t, bearerCfg(), api, auth)

	err := c.UpdateMyListStatus(context.Background(), 1, ListInput{Status: "watching"})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("error = %v, want ErrAuthRequired", err)
	}
}

// TestAuthRequiredWithoutCredentials pins fail-loud on unauthenticated
// use: the update must not silently no-op.
func TestAuthRequiredWithoutCredentials(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, config.MAL{Enabled: true},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("no request may leave in unauthenticated mode")
		}), nil)
	err := c.UpdateMyListStatus(context.Background(), 1, ListInput{Status: "watching"})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("error = %v, want ErrAuthRequired", err)
	}
}

// TestDisabledClientFailsLoud pins the disabled gate.
func TestDisabledClientFailsLoud(t *testing.T) {
	t.Parallel()

	c := New(config.MAL{}, APIBaseURL, OAuthBaseURL, nil)
	err := c.UpdateMyListStatus(context.Background(), 1, ListInput{Status: "watching"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("error = %v, want ErrDisabled", err)
	}
}

func TestDeleteMyListStatus(t *testing.T) {
	t.Parallel()

	rec := &apiRecorder{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		// Live-observed upstream: the DELETE answers 200 with a bare
		// `[]` body; any 2xx is success.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	if err := c.DeleteMyListStatus(context.Background(), 21); err != nil {
		t.Fatalf("DeleteMyListStatus: %v", err)
	}
	if len(rec.forms) != 1 {
		t.Fatalf("requests = %d, want 1", len(rec.forms))
	}
}

func TestGetAnimeList(t *testing.T) {
	t.Parallel()

	var paths []string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("fields"); got != "list_status{score,status,num_episodes_watched}" {
			t.Errorf("fields = %q", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{
					"node":        map[string]any{"id": 21, "title": "One Piece"},
					"list_status": map[string]any{"status": "watching", "score": 9, "num_episodes_watched": 1100},
				},
			},
			"paging": map[string]any{},
		})
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	page, err := c.GetAnimeList(context.Background())
	if err != nil {
		t.Fatalf("GetAnimeList: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("entries = %d, want 1", len(page))
	}
	e := page[0]
	if e.Node.ID != 21 || e.ListStatus.Status != "watching" ||
		e.ListStatus.NumEpisodesWatched != 1100 || e.ListStatus.Score != 9 {
		t.Errorf("entry = %+v", e)
	}
	if len(paths) != 1 {
		t.Fatalf("requests = %v, want one page (paging.next empty)", paths)
	}
	if !strings.Contains(paths[0], "/v2/users/@me/animelist") {
		t.Errorf("path = %s, want the @me animelist endpoint", paths[0])
	}
}

// TestGetAnimeListPaging pins the offset walk until paging.next is gone.
func TestGetAnimeListPaging(t *testing.T) {
	t.Parallel()

	var offsets []string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		offsets = append(offsets, r.Form.Get("offset"))
		if r.Form.Get("offset") == "0" {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": []map[string]any{
					{"node": map[string]any{"id": 1}, "list_status": map[string]any{"status": "watching"}},
				},
				"paging": map[string]any{"next": "https://api.example/v2/users/@me/animelist?offset=1"},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"node": map[string]any{"id": 2}, "list_status": map[string]any{"status": "completed"}},
			},
			"paging": map[string]any{},
		})
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	page, err := c.GetAnimeList(context.Background())
	if err != nil {
		t.Fatalf("GetAnimeList: %v", err)
	}
	if len(page) != 2 || offsets[0] != "0" || offsets[1] != "1" {
		t.Errorf("pages = %v entries = %d", offsets, len(page))
	}
}

func TestWhoAmI(t *testing.T) {
	t.Parallel()

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/users/@me" {
			t.Errorf("path = %s, want /v2/users/@me", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 777, "name": "owner"})
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	id, name, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if id != 777 || name != "owner" {
		t.Errorf("whoami = %d/%q, want 777/owner", id, name)
	}
}

func TestStatusToMAL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		shiki      string
		wantStatus string
		wantRew    bool
	}{
		{"watching", "watching", false},
		{"planned", "plan_to_watch", false}, // the Shikimori UI vocabulary
		{"plan_to_watch", "plan_to_watch", false},
		{"completed", "completed", false},
		{"on_hold", "on_hold", false},
		{"dropped", "dropped", false},
		{"rewatching", "watching", true}, // MAL encodes rewatching as a flag
		{"", "", false},
		{"nonsense", "", false},
	}
	for _, tc := range cases {
		status, rew := StatusToMAL(tc.shiki)
		if status != tc.wantStatus || rew != tc.wantRew {
			t.Errorf("StatusToMAL(%q) = (%q,%v), want (%q,%v)", tc.shiki, status, rew, tc.wantStatus, tc.wantRew)
		}
	}
}

func TestStatusFromMAL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mal      string
		rewatch  bool
		wantShik string
	}{
		{"watching", false, "watching"},
		{"watching", true, "rewatching"},
		{"plan_to_watch", false, "planned"},
		{"completed", false, "completed"},
		{"on_hold", false, "on_hold"},
		{"dropped", false, "dropped"},
		{"nonsense", false, ""},
	}
	for _, tc := range cases {
		if got := StatusFromMAL(tc.mal, tc.rewatch); got != tc.wantShik {
			t.Errorf("StatusFromMAL(%q,%v) = %q, want %q", tc.mal, tc.rewatch, got, tc.wantShik)
		}
	}
}
