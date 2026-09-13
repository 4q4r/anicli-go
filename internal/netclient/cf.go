package netclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

// cfChallengeMarkers are body fingerprints of a Cloudflare
// interstitial (checked inside the first bytes of the body).
var cfChallengeMarkers = []string{
	"just a moment",
	"challenge-platform",
	"cf-chl",
	"cf-turnstile",
}

// cfBodyScan is the body prefix scanned for markers.
const cfBodyScan = 4096

// CFChallengeError reports a Cloudflare challenge response that could
// not be cleared (no solver attached, or the solve failed).
type CFChallengeError struct {
	// URL is the challenged request URL.
	URL string
	// Cause carries the solve failure when one was attempted.
	Cause error
}

// Error implements error.
func (e *CFChallengeError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("cloudflare challenge on %s (solve failed: %v)", e.URL, e.Cause)
	}
	return fmt.Sprintf("cloudflare challenge on %s (no solver attached; enable [cf] and run `anicli cf install`)", e.URL)
}

// Unwrap exposes the solve failure.
func (e *CFChallengeError) Unwrap() error { return e.Cause }

// CFCookie is one clearance cookie to replay.
type CFCookie struct {
	Name, Value, Domain, Path string
}

// CFClearance is the solved state netclient replays: cookies plus the
// User-Agent that MUST accompany them (Cloudflare binds clearances to
// the fingerprint that solved them) and the matching language header.
type CFClearance struct {
	Cookies        []CFCookie
	UserAgent      string
	AcceptLanguage string
}

// CFSolver obtains clearances for challenged URLs. Implemented by the
// cfbrowser package; netclient stays browser-agnostic.
type CFSolver interface {
	// SolveChallenge returns a live clearance for the host of
	// targetURL (solving in a stealth browser when needed).
	SolveChallenge(ctx context.Context, targetURL string) (CFClearance, error)
	// InvalidateHost drops any cached clearance for a host — the
	// refresh-on-403 path before re-solving.
	InvalidateHost(host string)
}

// WithCFSolver attaches a Cloudflare-challenge solver: a detected
// challenge is solved once, the cookies plus the clearance UA are
// applied and the request retried a single time. Without a solver a
// detected challenge maps onto the typed CFChallengeError.
func WithCFSolver(solver CFSolver) Option {
	return func(c *Client) { c.cfSolver = solver }
}

// applyClearance installs a solved clearance on the client: cookies
// into the jar for the challenged origin, the clearance User-Agent as
// this client's UA override (per-client: each provider client must
// replay the UA its clearance was solved with), and the language
// override when provided.
func (c *Client) applyClearance(target string, clearance CFClearance) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("netclient: parse challenged url: %w", err)
	}
	if len(clearance.Cookies) > 0 {
		cookies := make([]*http.Cookie, 0, len(clearance.Cookies))
		for _, ck := range clearance.Cookies {
			cookies = append(cookies, &http.Cookie{
				Name:   ck.Name,
				Value:  ck.Value,
				Domain: ck.Domain,
				Path:   ck.Path,
			})
		}
		c.http.SetCookies(u, cookies)
	}
	if clearance.UserAgent != "" {
		c.mu.Lock()
		c.userAgentOverride = clearance.UserAgent
		if clearance.AcceptLanguage != "" {
			c.acceptLanguageOverride = clearance.AcceptLanguage
		}
		c.mu.Unlock()
	}
	return nil
}

// userAgent resolves the effective UA header (clearance override
// wins — it must equal the UA the clearance was solved with).
func (c *Client) effectiveUA() (ua, lang string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ua, lang = c.cfg.UserAgent, defaultAcceptLanguage
	if c.userAgentOverride != "" {
		ua = c.userAgentOverride
	}
	if c.acceptLanguageOverride != "" {
		lang = c.acceptLanguageOverride
	}
	return ua, lang
}

// solveChallenge runs the ladder step for a detected challenge:
// invalidate the host's stale clearance (refresh-on-403), solve,
// apply, retry once. Returns the retried response or the typed
// challenge error.
func (c *Client) solveChallenge(ctx context.Context, op string, req Request, challenged *Response) (*Response, error) {
	challengeErr := &CFChallengeError{URL: req.URL}
	if c.cfSolver == nil {
		return nil, c.wrap(op, challenged.StatusCode, challengeErr)
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		challengeErr.Cause = err
		return nil, c.wrap(op, challenged.StatusCode, challengeErr)
	}
	c.cfSolver.InvalidateHost(u.Hostname())

	clearance, solveErr := c.cfSolver.SolveChallenge(ctx, req.URL)
	if solveErr != nil {
		challengeErr.Cause = solveErr
		return nil, c.wrap(op, challenged.StatusCode, challengeErr)
	}
	if err := c.applyClearance(req.URL, clearance); err != nil {
		challengeErr.Cause = err
		return nil, c.wrap(op, challenged.StatusCode, challengeErr)
	}

	retried, retryErr := c.attempt(ctx, req, nil)
	if retryErr != nil {
		return nil, c.wrap(op, 0, fmt.Errorf("retry after cf solve: %w", retryErr))
	}
	if detectCFChallenge(retried) {
		// Still challenged after a fresh solve: report, never loop.
		challengeErr.Cause = errors.New("challenge persisted after solve")
		return nil, c.wrap(op, retried.StatusCode, challengeErr)
	}
	if retried.StatusCode >= http.StatusBadRequest {
		return nil, c.mapStatus(op, retried)
	}
	return retried, nil
}

// detectCFChallenge fingerprints a response as a Cloudflare
// challenge: 403 with the cf-mitigated: challenge header, or
// 403/503 whose HTML body carries a challenge marker in the first
// kilobytes.
func detectCFChallenge(resp *Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") {
		return true
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		return false
	}
	body := strings.ToLower(string(resp.Body[:min(len(resp.Body), cfBodyScan)]))
	for _, m := range cfChallengeMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}
