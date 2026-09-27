// Package mal is the MyAnimeList API v2 client (PR112): the OAuth2
// authorization-code flow with PKCE (the `plain` challenge method is
// the only one MAL supports), single-flight token refresh (proactive
// near expiry, reactive on 401) and the my-list CRUD the sync
// dispatcher needs.
//
// Wire contract (myanimelist.net/apiconfig/references/authorization):
// authorize at https://myanimelist.net/v1/oauth2/authorize with
// code_challenge_method=plain (the challenge IS the verifier); the
// token endpoint POSTs application/x-www-form-urlencoded to
// /v1/oauth2/token (Scheme 2: client credentials ride the body); the
// API v2 base is https://api.myanimelist.net/v2 with Bearer auth. The
// list-status form field is num_watched_episodes (the reply field is
// num_episodes_watched — the upstream asymmetry). Auth-mode split
// mirrors the shikimori client: enabled + token -> bearer; enabled
// without credentials -> public/none (authed calls fail loud with
// ErrAuthRequired); disabled -> ErrDisabled. A token rejected past a
// failed refresh degrades the client to none.
package mal

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Hosts, paths and policy constants.
const (
	OAuthBaseURL  = "https://myanimelist.net"
	authorizePath = "/v1/oauth2/authorize"
	// tokenPath is the URL path of the OAuth2 token endpoint. G101
	// false positive: a route, not a credential.
	tokenPath  = "/v1/oauth2/token" //nolint:gosec // G101: URL route, not a credential
	APIBaseURL = "https://api.myanimelist.net"
	UserAgent  = "anicli-go"

	refreshWindow  = 5 * time.Minute // proactive-refresh window (PR25 C parity)
	verifierBytes  = 64              // -> 86 base64url chars, within [43,128]
	listPageLimit  = 1000            // the API's page maximum
	maxListPageNum = 40              // safety bound on the paging walk
)

// Typed failures the sync dispatcher arbitrates on.
var (
	// ErrDisabled: the integration is switched off in settings.
	ErrDisabled = errors.New("mal: integration disabled")
	// ErrAuthRequired: no usable credentials (never configured, or the
	// token was rejected and the refresh failed).
	ErrAuthRequired = errors.New("mal: authorization required")
)

// authMode is the credential state of a client.
type authMode int

const (
	modeDisabled authMode = iota
	modeNone
	modeBearer
)

// String renders the mode for diagnostics.
func (m authMode) String() string {
	switch m {
	case modeDisabled:
		return "disabled"
	case modeNone:
		return "none"
	case modeBearer:
		return "bearer"
	}
	return "unknown"
}

// Client is the MyAnimeList API v2 client. It is safe for concurrent
// use.
type Client struct {
	net *netclient.Client
	cfg config.MAL
	// persist reports refreshed [mal] sections to the settings file
	// (nil in embedded/test clients).
	persist func(config.MAL) error
	log     *slog.Logger

	apiBase   string
	oauthBase string

	mode authMode

	mu         sync.Mutex
	refreshing bool
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithTokenPersister installs the sink a successful token refresh
// reports the updated [mal] section to (tokens persist to
// settings.toml). Persist errors are logged, never fatal.
func WithTokenPersister(p func(config.MAL) error) Option {
	return func(c *Client) { c.persist = p }
}

// WithLogger routes diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// New builds the client. apiBase/oauthBase are the production hosts
// (APIBaseURL/OAuthBaseURL); tests substitute httptest URLs. net is the
// shared transport (build it with netclient.WithProvider("mal")); nil
// is accepted for config-only use (Mode/Authenticated probes) — any
// request then fails loud.
func New(cfg config.MAL, apiBase, oauthBase string, net *netclient.Client, opts ...Option) *Client {
	mode := modeNone
	switch {
	case !cfg.Enabled:
		mode = modeDisabled
	case cfg.AccessToken != "":
		mode = modeBearer
	}
	c := &Client{
		net:       net,
		cfg:       cfg,
		log:       slog.Default(),
		apiBase:   strings.TrimSuffix(apiBase, "/"),
		oauthBase: strings.TrimSuffix(oauthBase, "/"),
		mode:      mode,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// currentMode snapshots the auth mode; the bearer path may degrade it
// to modeNone at runtime after an unrecoverable token failure.
func (c *Client) currentMode() authMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// Mode reports the active auth mode as a diagnostic string
// ("disabled", "none" or "bearer").
func (c *Client) Mode() string { return c.currentMode().String() }

// Authenticated reports whether the client carries a bearer token —
// the gate for personalized endpoints.
func (c *Client) Authenticated() bool { return c.currentMode() == modeBearer }

// Enabled reports whether the integration is switched on in settings
// (the sync dispatcher's participation gate).
func (c *Client) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Enabled
}

// Config exposes the live (refresh-mutated) [mal] section.
func (c *Client) Config() config.MAL {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// degradeToNone downgrades a bearer client to public mode after an
// unrecoverable token failure and warns once.
func (c *Client) degradeToNone() {
	c.mu.Lock()
	was := c.mode == modeBearer
	c.mode = modeNone
	c.mu.Unlock()
	if was {
		c.log.Warn("mal: bearer token rejected and refresh failed; degrading to public mode (re-run: anicli mal auth)")
	}
}

// NewOAuthState generates the CSRF state parameter for one
// authorization run (RFC 6749 §10.12): 32 crypto/rand bytes,
// hex-encoded.
func NewOAuthState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mal oauth: generate state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// VerifyOAuthState compares the callback's state against the expected
// one in constant time; any mismatch — including a missing value — is
// a login-CSRF/code-injection attempt and fails the flow.
func VerifyOAuthState(want, got string) error {
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return errors.New("mal oauth: state mismatch (возможна подмена кода авторизации)")
	}
	return nil
}

// NewCodeVerifier generates a PKCE code verifier: 64 random bytes
// base64url-encoded (86 chars of the RFC 7636 unreserved set). MAL's
// `plain` challenge method means the challenge IS the verifier.
func NewCodeVerifier() (string, error) {
	buf := make([]byte, verifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mal oauth: generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthorizeURL renders the authorization URL: response_type=code,
// the loopback redirect, the CSRF state and the plain PKCE challenge.
func AuthorizeURL(clientID, redirectURI, state, codeVerifier string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("state", state)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", codeVerifier)
	q.Set("code_challenge_method", "plain")
	return OAuthBaseURL + authorizePath + "?" + q.Encode()
}

// ExchangeCode trades an authorization code for the token pair. The
// verifier answers the plain challenge; client_secret is sent only
// when the MAL app registered one (official Scheme-2 rule).
func (c *Client) ExchangeCode(ctx context.Context, redirectURI, code, verifier string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", c.cfg.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	return c.tokenRequest(ctx, form, "exchange authorization code")
}

// RefreshAccessToken redeems a refresh token for a new pair.
func (c *Client) RefreshAccessToken(ctx context.Context) (*TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", c.cfg.ClientID)
	form.Set("refresh_token", c.cfg.RefreshToken)
	form.Set("grant_type", "refresh_token")
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	return c.tokenRequest(ctx, form, "refresh token")
}

// TokenSet is one OAuth2 token answer.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is seconds-since-epoch, computed from expires_in.
	ExpiresAt int64
}

// tokenRequest posts the form to the token endpoint and parses the
// answer, updating the live config with the fresh pair.
func (c *Client) tokenRequest(ctx context.Context, form url.Values, op string) (*TokenSet, error) {
	resp, err := c.net.Do(ctx, netclient.Request{
		Method: http.MethodPost,
		URL:    c.oauthBase + tokenPath,
		Headers: map[string]string{
			"User-Agent":   UserAgent,
			"Accept":       "application/json",
			"Content-Type": "application/x-www-form-urlencoded",
		},
		Body: strings.NewReader(form.Encode()),
	})
	if err != nil {
		return nil, fmt.Errorf("mal oauth %s: %w", op, err)
	}
	var reply struct {
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		return nil, fmt.Errorf("mal oauth %s: decode response: %w", op, err)
	}
	if reply.AccessToken == "" {
		return nil, fmt.Errorf("mal oauth %s: response carried no access token", op)
	}
	set := &TokenSet{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second).Unix(),
	}
	if reply.ExpiresIn <= 0 {
		set.ExpiresAt = 0 // unknown lifetime: no proactive refresh, 401-reactive only
	}
	c.mu.Lock()
	if reply.RefreshToken != "" {
		c.cfg.RefreshToken = reply.RefreshToken
	}
	c.cfg.AccessToken = reply.AccessToken
	c.cfg.TokenExpiresAt = set.ExpiresAt
	c.mu.Unlock()
	c.persistTokens("token " + op)
	return set, nil
}

// persistTokens reports the refreshed section to the persister
// (settings.toml); errors are logged, never fatal.
func (c *Client) persistTokens(what string) {
	c.mu.Lock()
	snapshot := c.cfg
	persist := c.persist
	c.mu.Unlock()
	if persist == nil {
		return
	}
	if err := persist(snapshot); err != nil {
		c.log.Warn("mal: token persist failed", "what", what, "error", err)
	}
}

// ListInput is the my_list_status update payload; zero fields are
// omitted (MAL updates only the specified parameters).
type ListInput struct {
	Status             string
	NumWatchedEpisodes int
	// IsRewatching rides the is_rewatching flag; false omits the field
	// (absent = no change upstream).
	IsRewatching bool
}

// UpdateMyListStatus PUTs the status form for one anime. MAL updates
// only the fields the form carries and creates the entry when absent.
func (c *Client) UpdateMyListStatus(ctx context.Context, animeID int64, in ListInput) error {
	form := url.Values{}
	if in.Status != "" {
		form.Set("status", in.Status)
	}
	if in.NumWatchedEpisodes > 0 {
		form.Set("num_watched_episodes", fmt.Sprintf("%d", in.NumWatchedEpisodes))
	}
	if in.IsRewatching {
		form.Set("is_rewatching", "true")
	}
	_, err := c.apiCall(ctx, http.MethodPut,
		fmt.Sprintf("%s/v2/anime/%d/my_list_status", c.apiBase, animeID), form)
	return err
}

// DeleteMyListStatus removes the anime from the user's list. Upstream
// answers 200 with a bare `[]` body; any non-2xx surfaces as an error.
func (c *Client) DeleteMyListStatus(ctx context.Context, animeID int64) error {
	_, err := c.apiCall(ctx, http.MethodDelete,
		fmt.Sprintf("%s/v2/anime/%d/my_list_status", c.apiBase, animeID), nil)
	return err
}

// ListEntry is one animelist row.
type ListEntry struct {
	Node struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"node"`
	ListStatus struct {
		Status             string `json:"status"`
		Score              int    `json:"score"`
		NumEpisodesWatched int    `json:"num_episodes_watched"`
	} `json:"list_status"`
}

// GetAnimeList pages the user's animelist with the status fields the
// sync parity needs.
func (c *Client) GetAnimeList(ctx context.Context) ([]ListEntry, error) {
	var out []ListEntry
	offset := 0
	for range maxListPageNum {
		q := url.Values{}
		q.Set("fields", "list_status{score,status,num_episodes_watched}")
		q.Set("limit", fmt.Sprintf("%d", listPageLimit))
		q.Set("offset", fmt.Sprintf("%d", offset))
		body, err := c.apiCall(ctx, http.MethodGet,
			c.apiBase+"/v2/users/@me/animelist?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var pageData struct {
			Data   []ListEntry `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(body, &pageData); err != nil {
			return nil, fmt.Errorf("mal animelist decode: %w", err)
		}
		out = append(out, pageData.Data...)
		if pageData.Paging.Next == "" || len(pageData.Data) == 0 {
			return out, nil
		}
		offset += len(pageData.Data)
	}
	return out, nil
}

// WhoAmI resolves the MAL user id and name via /v2/users/@me — the
// credential probe the setup flows greet the user with.
func (c *Client) WhoAmI(ctx context.Context) (int64, string, error) {
	body, err := c.apiCall(ctx, http.MethodGet, c.apiBase+"/v2/users/@me", nil)
	if err != nil {
		if authFailure(statusCodeOf(err)) {
			return 0, "", fmt.Errorf("%w: whoami: %w", ErrAuthRequired, err)
		}
		return 0, "", fmt.Errorf("mal whoami: %w", err)
	}
	var reply struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return 0, "", fmt.Errorf("mal whoami: decode response: %w", err)
	}
	if reply.ID == 0 {
		return 0, "", fmt.Errorf("%w: whoami returned no user", ErrAuthRequired)
	}
	return reply.ID, reply.Name, nil
}

// StatusToMAL maps a Shikimori rate status onto the MAL pair (status +
// is_rewatching). "planned" is the Shikimori UI vocabulary for
// "plan_to_watch"; "rewatching" is a flag on MAL, not a status. An
// unknown status maps to "" (callers fall back to watching).
func StatusToMAL(shikiStatus string) (status string, rewatching bool) {
	switch shikiStatus {
	case "watching":
		return "watching", false
	case "planned", "plan_to_watch":
		return "plan_to_watch", false
	case "completed":
		return "completed", false
	case "on_hold":
		return "on_hold", false
	case "dropped":
		return "dropped", false
	case "rewatching":
		return "watching", true
	default:
		return "", false
	}
}

// StatusFromMAL maps the MAL pair back onto the Shikimori status
// vocabulary ("" for an unknown status).
func StatusFromMAL(malStatus string, rewatching bool) string {
	switch malStatus {
	case "watching":
		if rewatching {
			return "rewatching"
		}
		return "watching"
	case "plan_to_watch":
		return "planned"
	case "completed":
		return "completed"
	case "on_hold":
		return "on_hold"
	case "dropped":
		return "dropped"
	default:
		return ""
	}
}

// apiCall runs one bearer request with the proactive + reactive
// refresh policy: the token is refreshed single-flight when stale and
// after a 401 the request is retried exactly once with the new token.
func (c *Client) apiCall(ctx context.Context, method, rawURL string, form url.Values) ([]byte, error) {
	if err := c.requireMode(); err != nil {
		return nil, err
	}
	c.ensureFreshToken(ctx)

	body, err := c.do(ctx, method, rawURL, form)
	if err != nil && authFailure(statusCodeOf(err)) && c.refreshOnce(ctx) {
		body, err = c.do(ctx, method, rawURL, form)
	}
	if err != nil {
		if authFailure(statusCodeOf(err)) {
			return nil, fmt.Errorf("%w: %s %s: %w", ErrAuthRequired, method,
				strings.TrimPrefix(rawURL, c.apiBase), err)
		}
		return nil, err
	}
	return body, nil
}

// requireMode gates an operation on the credential level.
func (c *Client) requireMode() error {
	switch c.currentMode() {
	case modeDisabled:
		return ErrDisabled
	case modeNone:
		return fmt.Errorf("%w: run anicli mal auth or configure [mal] access_token", ErrAuthRequired)
	}
	return nil
}

// do runs one raw bearer request through the shared transport.
func (c *Client) do(ctx context.Context, method, rawURL string, form url.Values) ([]byte, error) {
	c.mu.Lock()
	tok := c.cfg.AccessToken
	c.mu.Unlock()

	headers := map[string]string{
		"User-Agent":    UserAgent,
		"Accept":        "application/json",
		"Authorization": "Bearer " + tok,
		"Content-Type":  "application/x-www-form-urlencoded",
	}
	var body interface{ Read([]byte) (int, error) }
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	resp, err := c.net.Do(ctx, netclient.Request{
		Method:  method,
		URL:     rawURL,
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ensureFreshToken proactively refreshes a token inside the
// five-minute expiry window; failures warn (the 401 path arbitrates).
func (c *Client) ensureFreshToken(ctx context.Context) {
	c.mu.Lock()
	stale := c.cfg.TokenExpiresAt > 0 && time.Now().Add(refreshWindow).Unix() >= c.cfg.TokenExpiresAt
	refreshable := c.cfg.RefreshToken != "" && c.cfg.ClientID != ""
	c.mu.Unlock()
	if stale && refreshable {
		if _, err := c.RefreshAccessToken(ctx); err != nil {
			c.log.Warn("mal: proactive token refresh failed", "error", err)
		}
	}
}

// refreshOnce single-flights one refresh grant; false when another
// goroutine already refreshed or the material is missing. A failed
// refresh degrades the client to public mode.
func (c *Client) refreshOnce(ctx context.Context) bool {
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return false
	}
	if c.cfg.RefreshToken == "" || c.cfg.ClientID == "" {
		c.mu.Unlock()
		c.degradeToNone()
		return false
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	if _, err := c.RefreshAccessToken(ctx); err != nil {
		c.log.Warn("mal: token refresh failed", "error", err)
		c.degradeToNone()
		return false
	}
	return true
}

// authFailure reports auth-class HTTP statuses (the refresh ladder's
// trigger, mirroring the shikimori client).
func authFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// statusCodeOf extracts the HTTP status from an error chain: the
// netclient wraps non-2xx verdicts in *contracts.ProviderError.
func statusCodeOf(err error) int {
	var pe *contracts.ProviderError
	if errors.As(err, &pe) {
		return pe.StatusCode
	}
	return 0
}
