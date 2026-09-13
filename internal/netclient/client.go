// Package netclient is the shared HTTP layer of anicli-go: a production
// wrapper around github.com/bogdanfinn/tls-client (browser-fingerprint
// parity with cloudscraper) with cookie jar, retry/backoff, typed error
// mapping onto the contracts taxonomy and a bounded parallel fan-out
// helper. Port of anicli-py anicli/core/network.py.
//
// Timeout semantics: the Python original passed the user-tuned pair
// (connect_timeout=10, read_timeout=30) to requests. tls-client exposes a
// single combined timeout that covers dial and request (WithDialer.Timeout
// is overwritten by the client timeout — see tls-client connect.go
// newDirectDialer), so this wrapper feeds it Network.RequestTimeout (30s,
// verbatim) and additionally applies the same value as a per-attempt
// context deadline. A separate dial budget cannot be expressed against
// tls-client v1.16.0 and is not faked here; Network.ConnectTimeout stays
// available in config for layers that can honor it.
package netclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// Retry policy constants (task-fixed): three attempts, exponential
// backoff with equal jitter, Retry-After honored up to 10 seconds.
const (
	maxAttempts     = 3
	backoffBase     = 500 * time.Millisecond
	backoffCap      = 5 * time.Second
	retryAfterCap   = 10 * time.Second
	defaultBodyRead = 8 << 20 // 8 MiB

	defaultAcceptLanguage = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"
)

// ErrBodyLimit reports a response body exceeding the configured cap.
var ErrBodyLimit = errors.New("netclient: response body exceeds limit")

// StatusError reports a final non-retriable HTTP status that has no more
// specific sentinel mapping.
type StatusError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Status is the status line text.
	Status string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected http status %d %s", e.StatusCode, e.Status)
}

// Request is one outgoing HTTP request. Op names the provider operation
// for error context (contracts.Op*); empty defaults to "request".
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    io.Reader
	Op      string
}

// Response is a fully read response: Body is already buffered and the
// underlying connection has been drained and closed by the time Do
// returns.
type Response struct {
	StatusCode int
	Status     string
	Header     stdhttp.Header
	Body       []byte
	// FinalURL is the URL of the last request in the redirect chain —
	// equal to the request URL when no redirect occurred. Extractors
	// that recover a media Location via a redirecting POST (kwik) read
	// it instead of the body.
	FinalURL string
}

// Option customizes a Client.
type Option func(*Client)

// WithProvider sets the provider identifier attached to ProviderError
// mapping (contracts.WrapProvider).
func WithProvider(providerID string) Option {
	return func(c *Client) { c.provider = providerID }
}

// WithBodyLimit caps the response body size; n <= 0 keeps the 8 MiB
// default.
func WithBodyLimit(n int64) Option {
	return func(c *Client) {
		if n > 0 {
			c.bodyLimit = n
		}
	}
}

// Client is a concurrency-safe HTTP client. Build one per provider
// (each carries its own cookie jar) via New.
type Client struct {
	provider  string
	http      tls_client.HttpClient
	cfg       config.Network
	bodyLimit int64
	// sleep pauses between retries; swapped by tests for determinism.
	sleep func(context.Context, time.Duration) error

	// cfSolver clears Cloudflare challenges when attached. nil keeps
	// the pre-CF behavior for plain responses, with one deliberate
	// delta: a genuine challenge page (403/503 carrying challenge
	// markers) now maps onto the typed CFChallengeError instead of the
	// old ErrProvider403/StatusError — a challenge is actionable
	// (enable [cf], install the solver), a plain 403 is not.
	cfSolver CFSolver

	// mu guards the clearance UA/language overrides set after a solve.
	mu                     sync.RWMutex
	userAgentOverride      string
	acceptLanguageOverride string
}

// New builds the tls-client-backed Client from config.Network:
// Chrome_150 profile, HTTP/3 always disabled (known H3-racing data race,
// design spec §2), cookie jar, optional proxy.
func New(cfg config.Network, opts ...Option) (*Client, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutMilliseconds(int(cfg.RequestTimeout.Milliseconds())),
		tls_client.WithClientProfile(profiles.Chrome_150),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithDisableHttp3(),
	}
	if cfg.ProxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(cfg.ProxyURL))
	}
	httpClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("netclient: init tls-client (proxy %q): %w", cfg.ProxyURL, err)
	}

	c := &Client{
		http:      httpClient,
		cfg:       cfg,
		bodyLimit: defaultBodyRead,
		sleep:     ctxSleep,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Do executes req with the retry policy: up to three attempts, retrying
// only transient failures (network errors and 408/425/429/500/502/503/
// 504) with exponential backoff and equal jitter, honoring integer
// Retry-After up to 10s. Response bodies are drained and closed on every
// path. Errors are mapped onto the contracts taxonomy via
// contracts.WrapProvider.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	op := req.Op
	if op == "" {
		op = "request"
	}

	// Retries need a re-readable body: buffer once, bounded by the same
	// cap applied to response bodies.
	var payload []byte
	if req.Body != nil {
		buf, err := io.ReadAll(io.LimitReader(req.Body, c.bodyLimit+1))
		if err != nil {
			return nil, c.wrap(op, 0, fmt.Errorf("read request body: %w", err))
		}
		if int64(len(buf)) > c.bodyLimit {
			return nil, c.wrap(op, 0, fmt.Errorf("request body exceeds limit: %w", ErrBodyLimit))
		}
		payload = buf
	}

	for attempt := range maxAttempts {
		resp, err := c.attempt(ctx, req, payload)
		if err != nil {
			// Caller cancellation is never retried.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %w", ctx.Err(), err)
			}
			// An oversized body is deterministic: never retried.
			if errors.Is(err, ErrBodyLimit) {
				return nil, c.wrap(op, 0, err)
			}
			if attempt == maxAttempts-1 {
				// Network failure after the final attempt: timeout class.
				return nil, c.wrap(op, 0, fmt.Errorf("%w: %w", contracts.ErrProviderTimeout, err))
			}
			if err := c.sleep(ctx, backoffDelay(attempt)); err != nil {
				return nil, c.wrap(op, 0, fmt.Errorf("backoff interrupted: %w", err))
			}
			continue
		}

		// Cloudflare challenge ladder: detected challenges bypass the
		// generic retry loop entirely (a challenge never clears by
		// waiting) and either solve-and-retry-once or fail typed.
		if detectCFChallenge(resp) {
			return c.solveChallenge(ctx, op, req, payload, resp)
		}

		if resp.StatusCode < http.StatusBadRequest {
			return resp, nil
		}
		if retriableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			delay := backoffDelay(attempt)
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > delay {
				delay = min(ra, retryAfterCap)
			}
			if err := c.sleep(ctx, delay); err != nil {
				return nil, c.wrap(op, 0, fmt.Errorf("backoff interrupted: %w", err))
			}
			continue
		}
		return nil, c.mapStatus(op, resp)
	}
	// Unreachable: the loop always returns.
	return nil, c.wrap(op, 0, errors.New("netclient: exhausted attempts"))
}

// attempt performs one request/response cycle, reading and closing the
// body. Network-level failures are returned raw; statuses are classified
// by the caller.
func (c *Client) attempt(ctx context.Context, req Request, payload []byte) (*Response, error) {
	// Per-attempt deadline: a fresh budget per retry, overridable by the
	// caller's own (tighter) deadline.
	actx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	freq, err := http.NewRequestWithContext(actx, req.Method, req.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	ua, lang := c.effectiveUA()
	freq.Header.Set("User-Agent", ua)
	freq.Header.Set("Accept-Language", lang)
	for k, v := range req.Headers {
		freq.Header.Set(k, v)
	}

	resp, err := c.http.Do(freq)
	if err != nil {
		return nil, err
	}
	// Drained and closed on every path.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	limited := io.LimitReader(resp.Body, c.bodyLimit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > c.bodyLimit {
		return nil, fmt.Errorf("%w: %d bytes > %d", ErrBodyLimit, len(data), c.bodyLimit)
	}
	// fhttp mirrors net/http: resp.Request is the request that produced
	// this (final) response, i.e. the post-redirect URL when one occurred.
	finalURL := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Header:     stdhttp.Header(resp.Header),
		Body:       data,
		FinalURL:   finalURL,
	}, nil
}

// mapStatus routes an HTTP failure onto the contracts taxonomy.
func (c *Client) mapStatus(op string, resp *Response) error {
	switch resp.StatusCode {
	case http.StatusForbidden:
		return c.wrap(op, resp.StatusCode, contracts.ErrProvider403)
	case http.StatusNotFound:
		return c.wrap(op, resp.StatusCode, contracts.ErrNotFound)
	default:
		return c.wrap(op, resp.StatusCode, &StatusError{StatusCode: resp.StatusCode, Status: resp.Status})
	}
}

// wrap attaches provider context via contracts.WrapProvider.
func (c *Client) wrap(op string, status int, err error) error {
	return contracts.WrapProvider(c.provider, op, status, err)
}

// Get performs a GET with optional extra headers.
func (c *Client) Get(ctx context.Context, url string, headers map[string]string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, URL: url, Headers: headers})
}

// PostJSON marshals body as JSON and posts it with the JSON content type.
func (c *Client) PostJSON(ctx context.Context, url string, body any, headers map[string]string) (*Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("netclient: marshal json body: %w", err)
	}
	hdrs := withHeader(headers, "Content-Type", "application/json")
	return c.Do(ctx, Request{Method: http.MethodPost, URL: url, Headers: hdrs, Body: bytes.NewReader(payload)})
}

// PostForm encodes form and posts it with the form content type.
func (c *Client) PostForm(ctx context.Context, url string, form url.Values, headers map[string]string) (*Response, error) {
	hdrs := withHeader(headers, "Content-Type", "application/x-www-form-urlencoded")
	return c.Do(ctx, Request{Method: http.MethodPost, URL: url, Headers: hdrs, Body: strings.NewReader(form.Encode())})
}

// withHeader returns a copied header map with key set.
func withHeader(headers map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		out[k] = v
	}
	out[key] = value
	return out
}

// retriableStatus reports whether an HTTP status is worth retrying.
func retriableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// parseRetryAfter parses the integer-seconds Retry-After form (the one
// providers emit); anything else counts as absent.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	secs, err := strconv.ParseInt(header, 10, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > int64(retryAfterCap/time.Second) {
		return retryAfterCap
	}
	return time.Duration(secs) * time.Second
}

// backoffDelay computes exponential backoff with equal jitter (AWS
// style): half the floor plus up to half random, capped.
func backoffDelay(attempt int) time.Duration {
	lo, hi := backoffBounds(attempt)
	n, err := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)))
	if err != nil {
		// crypto/rand failure: take the deterministic floor.
		return lo
	}
	return lo + time.Duration(n.Int64())
}

// backoffBounds returns [lo, hi] for backoffDelay: base doubling capped,
// with equal jitter splitting the range.
func backoffBounds(attempt int) (lo, hi time.Duration) {
	d := backoffBase << attempt
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	return d / 2, d
}

// ctxSleep sleeps for d unless ctx finishes first.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
