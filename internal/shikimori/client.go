// Package shikimori is the Shikimori (shikimori.io) tracker client,
// ported from anicli-py anicli/core/shikimori.py plus the user rulings in
// the feature inventory (section F) that the frozen Python tree had lost.
//
// Strict auth-mode split (ruling: "no brute-force probing"):
//   - AccessToken configured  -> Bearer JSON-API mode only;
//   - else Session configured -> cookie mode only (_kawai_session +
//     CSRF bootstrap from /users/sign_in first, root-page fallback);
//   - else                    -> public reads only; authed operations
//     fail loud with ErrAuthRequired.
//
// The client NEVER falls back between modes. Mutating cookie requests
// re-bootstrap CSRF and replay exactly once on 401/403 — never on 422.
// GET requests never bootstrap CSRF at all. The 422 handler narrows to
// the known planned->plan_to_watch status quirk and retries once with the
// canonical status.
//
// Rate budget (PR8 spec, stricter than the frozen Python 0.25s spacing):
// one request per second and five per rolling minute, enforced before
// every outgoing request; Retry-After on 429 is honored by the underlying
// netclient retries and additionally penalizes the budget here.
package shikimori

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// UserAgent must ride on every request to Shikimori: their API etiquette
// requires a distinctive app User-Agent (PR1 follow-up F2; Python
// anicli/config.py ShikimoriSettings.user_agent = "anicli-ru").
const UserAgent = "anicli-ru"

// DefaultBaseURL is the production site root. shikimori.io is the
// canonical domain since the 2026 migration: the old shikimori.one is
// blocked in Russia and answers 301/308 redirects to .io — which
// rewrite the OAuth token POST into a GET (observed upstream in Mihon
// #3497) — so every request, tokens included, goes straight to .io
// (the official OAuth guide itself uses shikimori.io/oauth).
const DefaultBaseURL = "https://shikimori.io"

// retryAfterPenalty is the budget penalty applied when a 429 surfaces
// past the netclient's Retry-After-aware retries; it mirrors the
// netclient Retry-After cap so both layers back off on the same scale.
const retryAfterPenalty = 10 * time.Second

// authMode is the credential state of a client.
type authMode int

const (
	// modeDisabled rejects everything: the integration is off.
	modeDisabled authMode = iota
	// modeNone has no credentials: public reads only.
	modeNone
	// modeCookie uses the _kawai_session cookie + CSRF flow.
	modeCookie
	// modeBearer uses the OAuth2 access token against the JSON API.
	modeBearer
)

// String renders the mode for diagnostics.
func (m authMode) String() string {
	switch m {
	case modeDisabled:
		return "disabled"
	case modeNone:
		return "none"
	case modeCookie:
		return "cookie"
	case modeBearer:
		return "bearer"
	}
	return "unknown"
}

// Client talks to Shikimori. Construct with New; it is safe for
// concurrent use.
type Client struct {
	cfg config.Shikimori
	net *netclient.Client
	log *slog.Logger

	mode    authMode
	baseURL string
	lim     *limiter

	// persist reports refreshed [shikimori] sections to the settings
	// file (nil in embedded/test clients; PR25 E).
	persist func(config.Shikimori) error

	mu           sync.Mutex
	csrf         string
	csrfWarned   bool // single-warning ruling
	csrfWarnings int  // test-visible count of emitted warnings
	userID       *int64
}

// New builds the client. cfg selects the auth mode; net is the shared
// transport (build it with netclient.WithProvider("shikimori")); logger
// may be nil (slog.Default is used). Options install the extras (e.g.
// WithTokenPersister).
func New(cfg config.Shikimori, net *netclient.Client, logger *slog.Logger, opts ...Option) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	mode := modeNone
	switch {
	case !cfg.Enabled:
		mode = modeDisabled
	case cfg.AccessToken != "":
		// Token wins when both are configured: bearer JSON-API only.
		mode = modeBearer
	case cfg.Session != "":
		mode = modeCookie
	}

	c := &Client{
		cfg:     cfg,
		net:     net,
		log:     logger,
		mode:    mode,
		baseURL: DefaultBaseURL,
		lim:     newLimiter(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// currentMode snapshots the auth mode; the bearer path may degrade it
// to modeNone at runtime after an unrecoverable token failure (PR25 C).
func (c *Client) currentMode() authMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// Mode reports the active auth mode as a diagnostic string
// ("disabled", "none", "cookie" or "bearer").
func (c *Client) Mode() string { return c.currentMode().String() }

// Authenticated reports whether the client carries user credentials
// (cookie or bearer) — the gate for personalized endpoints (python
// is_authenticated).
func (c *Client) Authenticated() bool {
	mode := c.currentMode()
	return mode == modeCookie || mode == modeBearer
}

// apiHeaders builds the headers every JSON API request carries: the
// strict User-Agent, JSON accept and the XMLHttpRequest marker (ported
// from the Python api_headers), plus mode credentials.
func (c *Client) apiHeaders() map[string]string {
	h := map[string]string{
		"User-Agent":       UserAgent,
		"Accept":           "application/json, text/plain, */*",
		"X-Requested-With": "XMLHttpRequest",
	}
	switch c.currentMode() {
	case modeCookie:
		c.mu.Lock()
		session := c.cfg.Session
		c.mu.Unlock()
		h["Cookie"] = "_kawai_session=" + session
	case modeBearer:
		c.mu.Lock()
		token := c.cfg.AccessToken
		c.mu.Unlock()
		h["Authorization"] = "Bearer " + token
	}
	return h
}

// pageHeaders builds headers for the CSRF bootstrap page fetches: same
// strict User-Agent ruling (F2) plus the cookie session; deliberately no
// Authorization header — bearer mode never fetches pages at all.
func (c *Client) pageHeaders() map[string]string {
	h := map[string]string{
		"User-Agent":       UserAgent,
		"Accept":           "text/html,application/xhtml+xml",
		"X-Requested-With": "XMLHttpRequest",
	}
	if c.currentMode() == modeCookie {
		c.mu.Lock()
		session := c.cfg.Session
		c.mu.Unlock()
		h["Cookie"] = "_kawai_session=" + session
	}
	return h
}

// requireMode gates an operation on an authentication level. Public
// reads pass in every non-disabled mode; authed operations demand cookie
// or bearer credentials.
func (c *Client) requireMode(authed bool) error {
	switch c.currentMode() {
	case modeDisabled:
		return ErrDisabled
	case modeNone:
		if authed {
			return fmt.Errorf("%w: configure shikimori.session or shikimori.access_token", ErrAuthRequired)
		}
	}
	return nil
}

// get performs a rate-limited JSON GET. It never bootstraps CSRF
// (GET-skips-CSRF ruling). Bearer mode proactively refreshes the token
// near expiry and retries once with the fresh token after an
// auth-class failure (PR25 C).
func (c *Client) get(ctx context.Context, path string, query url.Values) (*netclient.Response, error) {
	c.ensureFreshToken(ctx)

	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	resp, err := c.doGet(ctx, u)
	if err != nil && authFailure(statusCodeOf(err)) && c.tryBearerRefreshOn401(ctx) {
		resp, err = c.doGet(ctx, u)
	}
	return resp, err
}

// doGet is one rate-limited GET attempt.
func (c *Client) doGet(ctx context.Context, u string) (*netclient.Response, error) {
	if err := c.lim.Wait(ctx); err != nil {
		return nil, fmt.Errorf("shikimori: rate limiter: %w", err)
	}
	return c.net.Do(ctx, netclient.Request{
		Method:  http.MethodGet,
		URL:     u,
		Headers: c.apiHeaders(),
	})
}

// mutate performs a mutating JSON request under the auth-mode rules and
// returns the rate id from the response (0 for 204 No Content).
//
// build renders the full request body for a given status form; the 422
// handler re-renders it with the canonical status for the one
// planned->plan_to_watch retry.
//
// Cookie mode: bootstrap CSRF when no token is cached (sign_in first,
// root fallback), send X-CSRF-Token, and on 401/403 re-bootstrap and
// replay the request exactly once. Bearer mode: plain Authorization
// header, no CSRF, no cookie endpoints.
func (c *Client) mutate(ctx context.Context, method, path, op string,
	input RateInput, build func(RateInput) any,
) (int64, error) {
	if err := c.requireMode(true); err != nil {
		return 0, err
	}

	csrfRetried := false
	plannedRetried := false
	bearerRetried := false

	for range 4 { // original + one CSRF replay + one bearer refresh replay + one 422 replay
		csrf := ""
		if c.currentMode() == modeCookie {
			csrf, _ = c.csrfToken(ctx)
		}

		resp, err := c.sendMutating(ctx, method, path, build(input), csrf, op)
		if err != nil {
			status := statusCodeOf(err)

			// Bearer-mode refresh ladder (PR25 C): 401/403 refreshes
			// the token once and replays with the fresh one.
			if authFailure(status) && !bearerRetried && c.tryBearerRefreshOn401(ctx) {
				bearerRetried = true
				continue
			}
			// Cookie-mode CSRF ladder: 401/403 only, exactly one replay.
			if c.currentMode() == modeCookie && authFailure(status) && !csrfRetried {
				csrfRetried = true
				c.dropCSRF()
				continue
			}
			// 422 planned-status normalization: retry once with the
			// canonical status; never re-bootstraps CSRF.
			if status == http.StatusUnprocessableEntity &&
				input.Status == "planned" && !plannedRetried {
				plannedRetried = true
				input.Status = CanonicalStatus(input.Status)
				continue
			}
			return 0, c.mapMutationError(op, err)
		}

		if resp.StatusCode == http.StatusNoContent {
			return 0, nil
		}
		var reply rateResponse
		if err := json.Unmarshal(resp.Body, &reply); err != nil {
			return 0, fmt.Errorf("shikimori %s: decode response: %w", op, err)
		}
		return reply.ID, nil
	}
	return 0, fmt.Errorf("shikimori %s: exhausted retry ladder", op)
}

// sendMutating issues one mutating HTTP request.
func (c *Client) sendMutating(ctx context.Context, method, path string, body any, csrf, op string) (*netclient.Response, error) {
	c.ensureFreshToken(ctx)

	if err := c.lim.Wait(ctx); err != nil {
		return nil, fmt.Errorf("shikimori: rate limiter: %w", err)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("shikimori %s: marshal payload: %w", op, err)
	}

	headers := c.apiHeaders()
	headers["Content-Type"] = "application/json"
	if c.currentMode() == modeCookie {
		// Python parity: the header rides even when bootstrap failed
		// (empty value); the 401/403 ladder arbitrates.
		headers["X-CSRF-Token"] = csrf
	}

	resp, err := c.net.Do(ctx, netclient.Request{
		Method:  method,
		URL:     c.baseURL + path,
		Headers: headers,
		Body:    strings.NewReader(string(payload)),
	})
	if err != nil {
		// A surfaced 429 (netclient retries exhausted): penalize the
		// budget on the same scale as the transport Retry-After cap.
		if statusCodeOf(err) == http.StatusTooManyRequests {
			c.lim.Penalize(retryAfterPenalty)
		}
		return nil, err
	}
	return resp, nil
}

// mapMutationError routes a mutating failure onto the typed taxonomy.
func (c *Client) mapMutationError(op string, err error) error {
	switch status := statusCodeOf(err); {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s: %w", ErrRateLimited, op, err)
	case authFailure(status):
		return fmt.Errorf("%w: %s: %w", ErrAuthRequired, op, err)
	case status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s: %w", ErrUnprocessable, op, err)
	default:
		return err
	}
}

// csrfToken returns the cached token, bootstrapping it when absent.
func (c *Client) csrfToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.csrf
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	token := c.bootstrapCSRF(ctx)
	c.mu.Lock()
	c.csrf = token
	c.mu.Unlock()
	return token, nil
}

// dropCSRF invalidates the cached token (before the 401/403 replay).
func (c *Client) dropCSRF() {
	c.mu.Lock()
	c.csrf = ""
	c.mu.Unlock()
}

// bootstrapCSRF fetches the CSRF token: /users/sign_in first, the root
// page as fallback (04-29 noise-reduction ruling). Failures log a single
// warning and yield an empty token — the request still goes out and the
// 401/403 ladder arbitrates (Python sent the empty token too).
func (c *Client) bootstrapCSRF(ctx context.Context) string {
	for _, path := range []string{"/users/sign_in", "/"} {
		if err := c.lim.Wait(ctx); err != nil {
			break
		}
		resp, err := c.net.Do(ctx, netclient.Request{
			Method:  http.MethodGet,
			URL:     c.baseURL + path,
			Headers: c.pageHeaders(),
		})
		if err != nil {
			continue
		}
		if token := parseCSRFToken(resp.Body); token != "" {
			return token
		}
	}

	c.mu.Lock()
	if !c.csrfWarned {
		c.csrfWarned = true
		c.csrfWarnings++
		c.log.Warn("shikimori: CSRF token not found on sign_in/root pages; requests proceed without it")
	}
	c.mu.Unlock()
	return ""
}

// parseCSRFToken extracts the csrf-token meta tag from an HTML page.
func parseCSRFToken(html []byte) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(html)))
	if err != nil {
		return ""
	}
	token, _ := doc.Find(`meta[name="csrf-token"]`).First().Attr("content")
	return strings.TrimSpace(token)
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
