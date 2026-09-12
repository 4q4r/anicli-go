package contracts

import (
	"errors"
	"strconv"
	"strings"
)

// Sentinel errors of the provider pipeline. Callers classify failures with
// errors.Is; providers wrap these in ProviderError to attach provider
// context. Ported from the error routing contract of the Python original.
var (
	// ErrGeoBlocked: the provider refuses the client's region.
	ErrGeoBlocked = errors.New("geo blocked")
	// ErrProvider403: the provider answered 403 (WAF / Cloudflare wall).
	ErrProvider403 = errors.New("provider returned forbidden")
	// ErrProviderTimeout: the provider did not answer in time.
	ErrProviderTimeout = errors.New("provider timeout")
	// ErrExtractFailed: the stream link could not be extracted from the
	// embed page.
	ErrExtractFailed = errors.New("extract failed")
	// ErrAllCandidatesFailed: every candidate embed failed to yield a
	// playable stream.
	ErrAllCandidatesFailed = errors.New("all candidates failed")
	// ErrNotFound: the requested entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput: caller-supplied arguments are invalid.
	ErrInvalidInput = errors.New("invalid input")
)

// ProviderError annotates an error with provider context: which provider,
// which operation, and the HTTP status code when one applies.
//
// It is errors.Is-compatible with the sentinel it wraps:
//
//	err := contracts.WrapProvider("kodik", contracts.OpSearch, 403, contracts.ErrProvider403)
//	errors.Is(err, contracts.ErrProvider403) // true
type ProviderError struct {
	Provider   string
	Op         string
	StatusCode int
	Err        error
}

// WrapProvider builds a *ProviderError from provider context and a cause.
// A nil cause is tolerated (the resulting Error stays usable); prefer always
// passing a real sentinel or cause.
func WrapProvider(provider, op string, statusCode int, err error) *ProviderError {
	return &ProviderError{Provider: provider, Op: op, StatusCode: statusCode, Err: err}
}

// Error implements error, rendering every non-empty field.
func (e *ProviderError) Error() string {
	var b strings.Builder
	b.WriteString("provider ")
	b.WriteString(e.Provider)
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.StatusCode > 0 {
		b.WriteString(": HTTP ")
		b.WriteString(strconv.Itoa(e.StatusCode))
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause for errors.Is / errors.As.
func (e *ProviderError) Unwrap() error {
	return e.Err
}
