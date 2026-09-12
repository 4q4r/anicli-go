package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// statWriteTimeout bounds a single search-stat database write so a stuck
// store can never wedge the search path on top of the error swallow below.
const statWriteTimeout = 5 * time.Second

// Registry is the ordered provider set: registration order is preserved
// so consumers can render and fan out in a stable sequence.
type Registry struct {
	order []contracts.Provider
	byID  map[string]contracts.Provider
}

// NewEmptyRegistry builds a registry with no providers registered.
func NewEmptyRegistry() *Registry {
	return &Registry{byID: make(map[string]contracts.Provider)}
}

// Register adds p to the registry; a duplicate ID fails loudly with
// contracts.ErrInvalidInput.
func (r *Registry) Register(p contracts.Provider) error {
	id := p.ID()
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("register provider %q: %w", id, contracts.ErrInvalidInput)
	}
	r.byID[id] = p
	r.order = append(r.order, p)
	return nil
}

// Get returns the provider registered under id.
func (r *Registry) Get(id string) (contracts.Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// List returns every registered provider in registration order.
func (r *Registry) List() []contracts.Provider {
	return append([]contracts.Provider(nil), r.order...)
}

// SearchDelegator wraps a Provider's Search with response-stat recording:
// success/failure and latency are folded into ProviderStatRepo after the
// search completes. Recording is best-effort: failures are logged via slog
// and never fail the search (the latency is captured before the write, so
// the recording itself never pollutes the measurement). The write runs on
// an uncanceled context so a search deadline expiring mid-flight cannot
// kill the stat bookkeeping.
type SearchDelegator struct {
	contracts.Provider

	// stats records search outcomes; nil disables recording.
	stats *storage.ProviderStatRepo
	// logger receives recording failures; nil means slog.Default().
	logger *slog.Logger
}

// Search runs the wrapped provider's Search and records its outcome.
// Errors that are not already a *contracts.ProviderError are wrapped via
// contracts.WrapProvider so consumers always see provider context.
func (d SearchDelegator) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	start := time.Now()
	results, err := d.Provider.Search(ctx, query)
	latencyMS := float64(time.Since(start).Microseconds()) / 1000

	d.recordStat(ctx, err == nil, latencyMS)

	if err != nil {
		return nil, d.wrapErr(err)
	}
	return results, nil
}

// recordStat folds one outcome into the stats repository. Every failure
// is logged and swallowed: stat bookkeeping must never break a search.
func (d SearchDelegator) recordStat(ctx context.Context, success bool, latencyMS float64) {
	if d.stats == nil {
		return
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statWriteTimeout)
	defer cancel()

	if err := d.stats.RecordResult(rctx, d.ID(), success, latencyMS); err != nil {
		logger := d.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("record provider search stat",
			"provider", d.ID(), "error", err)
	}
}

// wrapErr attaches provider context to a search error unless the error
// already carries it.
func (d SearchDelegator) wrapErr(err error) error {
	var perr *contracts.ProviderError
	if errors.As(err, &perr) {
		return err
	}
	return contracts.WrapProvider(d.ID(), contracts.OpSearch, 0, err)
}
