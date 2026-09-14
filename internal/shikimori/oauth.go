package shikimori

// OAuth2 authorization-code flow and token refresh against
// shikimori.io/oauth (PR25): the terminal flow (`anicli shikimori auth`)
// exchanges a loopback-callback code for the token pair, and the bearer
// client refreshes its access token automatically — proactively inside
// the last five minutes of its life, reactively after a 401/403.
//
// Wire contract (official docs): the token endpoint speaks
// application/x-www-form-urlencoded; access tokens live one day
// (86400s); a fresh refresh token is returned alongside every access
// token; the strict User-Agent ruling applies to the token endpoint too.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// refreshWindow is how close to expiry a bearer token gets before the
// client refreshes it proactively (PR25 C: five minutes).
const refreshWindow = 5 * time.Minute

// TokenSet is one OAuth2 token answer: the access/refresh pair plus the
// unix timestamp at which the access token expires.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is seconds-since-epoch; computed from expires_in.
	ExpiresAt int64
}

// AuthorizeURL renders the authorization-code URL the user opens in a
// browser: client_id + loopback redirect_uri + response_type=code + the
// user_rates scope required for rate CRUD (PR25 D).
func AuthorizeURL(clientID, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "user_rates")
	return DefaultBaseURL + "/oauth/authorize?" + q.Encode()
}

// ExchangeCode trades an authorization code for the token pair.
// It is mode-independent: the auth flow runs before any token exists.
func (c *Client) ExchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("code", code)
	return c.tokenRequest(ctx, form, "exchange authorization code")
}

// RefreshAccessToken redeems a refresh token for a new pair.
func (c *Client) RefreshAccessToken(ctx context.Context, clientID, clientSecret, refreshToken string) (*TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)
	return c.tokenRequest(ctx, form, "refresh token")
}

// tokenRequest posts the form to /oauth/token and parses the answer.
func (c *Client) tokenRequest(ctx context.Context, form url.Values, op string) (*TokenSet, error) {
	resp, err := c.net.Do(ctx, netclient.Request{
		Method: http.MethodPost,
		URL:    c.baseURL + "/oauth/token",
		Headers: map[string]string{
			"User-Agent":   UserAgent,
			"Accept":       "application/json",
			"Content-Type": "application/x-www-form-urlencoded",
		},
		Body: strings.NewReader(form.Encode()),
	})
	if err != nil {
		return nil, fmt.Errorf("shikimori oauth %s: %w", op, err)
	}

	var reply struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		CreatedAt    int64  `json:"created_at"`
	}
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		return nil, fmt.Errorf("shikimori oauth %s: decode response: %w", op, err)
	}
	if reply.AccessToken == "" {
		return nil, fmt.Errorf("shikimori oauth %s: response carried no access token", op)
	}
	set := &TokenSet{
		AccessToken:  reply.AccessToken,
		RefreshToken: reply.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second).Unix(),
	}
	if reply.ExpiresIn <= 0 {
		set.ExpiresAt = 0 // unknown lifetime: no proactive refresh, 401-reactive only
	}
	return set, nil
}

// Option customizes a Client at construction (New).
type Option func(*Client)

// WithTokenPersister installs the sink a successful token refresh
// reports the updated [shikimori] section to (PR25 E: tokens persist to
// settings.toml). Persist errors are logged, never fatal.
func WithTokenPersister(p func(config.Shikimori) error) Option {
	return func(c *Client) { c.persist = p }
}

// refreshStateLocked snapshots the bearer refresh material under mu.
type refreshStateLocked struct {
	ok           bool
	clientID     string
	clientSecret string
	refreshToken string
}

// refreshState reports whether the client can refresh right now
// (bearer mode with the full app-credential set). Call with mu held.
func (c *Client) refreshStateLocked() refreshStateLocked {
	s := refreshStateLocked{
		clientID:     c.cfg.ClientID,
		clientSecret: c.cfg.ClientSecret,
		refreshToken: c.cfg.RefreshToken,
	}
	s.ok = c.mode == modeBearer &&
		s.clientID != "" && s.clientSecret != "" && s.refreshToken != ""
	return s
}

// ensureFreshToken proactively refreshes a bearer token expiring inside
// the five-minute window (PR25 C). A failed proactive refresh only
// warns: the current token may still work, the 401 path arbitrates.
func (c *Client) ensureFreshToken(ctx context.Context) {
	c.mu.Lock()
	stale := c.mode == modeBearer && c.cfg.TokenExpiresAt > 0 &&
		time.Now().Add(refreshWindow).Unix() >= c.cfg.TokenExpiresAt
	refreshable := c.refreshStateLocked().ok
	c.mu.Unlock()

	if stale && refreshable && !c.refreshBearerToken(ctx) {
		c.log.Warn("shikimori: proactive token refresh failed; continuing with the current token")
	}
}

// refreshBearerToken performs one single-flight refresh: mu is held
// across the token round-trip so concurrent requests serialize instead
// of stampeding the endpoint. It always attempts the exchange — a
// token can be server-side-rejected long before its recorded expiry,
// so recorded freshness never short-circuits a demanded refresh.
func (c *Client) refreshBearerToken(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.refreshStateLocked()
	if !st.ok {
		return false
	}

	set, err := c.RefreshAccessToken(ctx, st.clientID, st.clientSecret, st.refreshToken)
	if err != nil {
		c.log.Warn("shikimori: token refresh failed", "error", err)
		return false
	}
	c.cfg.AccessToken = set.AccessToken
	if set.RefreshToken != "" {
		c.cfg.RefreshToken = set.RefreshToken
	}
	c.cfg.TokenExpiresAt = set.ExpiresAt

	if c.persist != nil {
		section := c.cfg // copy under the lock
		if err := c.persist(section); err != nil {
			c.log.Warn("shikimori: persisting refreshed tokens failed", "error", err)
		}
	}
	return true
}

// tryBearerRefreshOn401 reacts to an auth-class failure in bearer mode:
// refresh once and report whether the caller should replay. When no
// refresh can save the session, the client degrades to public reads
// (mode none) per PR25 C and the failure surfaces as ErrAuthRequired.
func (c *Client) tryBearerRefreshOn401(ctx context.Context) bool {
	c.mu.Lock()
	bearer := c.mode == modeBearer
	c.mu.Unlock()
	if !bearer {
		return false
	}
	if c.refreshBearerToken(ctx) {
		return true
	}

	c.mu.Lock()
	degrade := c.mode == modeBearer
	c.mode = modeNone
	c.mu.Unlock()
	if degrade {
		c.log.Warn("shikimori: bearer token rejected and refresh failed; degrading to public reads (re-run: anicli shikimori auth)")
	}
	return false
}
