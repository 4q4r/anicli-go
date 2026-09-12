package shikimori

import (
	"errors"
	"fmt"
	"net/http"
)

// Typed errors of the Shikimori client. The client fails loud: every
// auth-mode violation surfaces as one of these sentinels (wrapped in
// operational detail), never as a silent empty result — the strict
// counterpart of the frozen Python original, which returned None/[] on
// every failure path.
var (
	// ErrDisabled: the integration is switched off in config.
	ErrDisabled = errors.New("shikimori: integration disabled")
	// ErrAuthRequired: the operation needs credentials but none are
	// configured, or the configured credentials were rejected.
	ErrAuthRequired = errors.New("shikimori: authentication required")
	// ErrRateLimited: the endpoint answered 429 even after the transport
	// layer's Retry-After-aware retries.
	ErrRateLimited = errors.New("shikimori: rate limited")
	// ErrUnprocessable: the endpoint rejected the payload with 422 for a
	// reason other than the known planned-status quirk.
	ErrUnprocessable = errors.New("shikimori: request rejected")
)

// APIError reports a non-2xx Shikimori API response that has no more
// specific sentinel mapping. StatusCode and Body carry the wire facts.
type APIError struct {
	// Op names the client operation, e.g. "create_rate".
	Op string
	// StatusCode is the HTTP status code.
	StatusCode int
	// Status is the status line text.
	Status string
	// Body is the (truncated) response body.
	Body string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("shikimori %s: HTTP %d %s: %s", e.Op, e.StatusCode, e.Status, truncate(e.Body, 200))
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// authFailure reports whether an HTTP status belongs to the auth class
// that triggers the CSRF re-bootstrap ladder (cookie mode ruling:
// 401/403 only — deliberately NOT 422).
func authFailure(statusCode int) bool {
	return statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden
}
