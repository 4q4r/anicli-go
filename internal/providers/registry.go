package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// statWriteTimeout bounds a single search-stat database write so a stuck
// store can never wedge the search path on top of the error swallow below.
const statWriteTimeout = 5 * time.Second

// Registry is the ordered provider set: registration order is preserved
// so consumers can render and fan out in a stable sequence.
type Registry struct {
	order []contracts.Provider
	byID  map[string]contracts.Provider
	// disabled records providers excluded at startup because they
	// cannot run without user configuration (PR24); the health and
	// doctor surfaces render them as ОТКЛЮЧЁН.
	disabled []DisabledProvider
	// cfClose releases the shared CF-bypass stack (browser session +
	// updater ticker) when the registry was built with [cf].enabled;
	// nil otherwise.
	cfClose func()
	// engine is the ONE shared lazy torrent engine (PR36): built when
	// [torrent].enabled, injected into every torrent provider and
	// torn down by Close. Nil when the subsystem is disabled.
	engine *torrent.Engine
}

// NewEmptyRegistry builds a registry with no providers registered.
func NewEmptyRegistry() *Registry {
	return &Registry{byID: make(map[string]contracts.Provider)}
}

// Close releases the shared resources: the torrent engine first
// (network teardown; safe on a never-started engine), then the CF
// bypass stack. Safe on disabled registries and idempotent.
func (r *Registry) Close() error {
	if r.engine != nil {
		if err := r.engine.Close(); err != nil {
			return err
		}
		r.engine = nil
	}
	if r.cfClose != nil {
		r.cfClose()
		r.cfClose = nil
	}
	return nil
}

// TorrentEngine returns the shared lazy torrent engine (nil when the
// [torrent] subsystem is disabled). Every torrent provider resolves
// its picks through this one client, one listen port.
func (r *Registry) TorrentEngine() *torrent.Engine {
	return r.engine
}

// wireTorrentEngine builds the shared lazy torrent engine (NewEngine
// starts nothing) and injects it into every torrent provider via
// SetEngine. A broken torrent transport fails registry construction
// loud — the same contract as any provider client. The engine is
// owned unconditionally: even when no torrent provider is registered
// (e.g. nyaa in [providers].exclude) it stays — it costs nothing
// while idle and Registry.Close tears it down.
func (r *Registry) wireTorrentEngine(cfg config.Settings, bare []contracts.Provider, log *slog.Logger) error {
	net, err := netclient.New(cfg.Network, netclient.WithProvider("torrent"))
	if err != nil {
		return fmt.Errorf("build torrent transport: %w", err)
	}
	r.engine = torrent.NewEngine(cfg.Torrent, net, log)
	for _, p := range bare {
		if se, ok := p.(interface{ SetEngine(*torrent.Engine) }); ok {
			se.SetEngine(r.engine)
		}
	}
	return nil
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

// Disabled returns the providers excluded at startup for missing
// configuration, with their user-facing reasons (PR24).
func (r *Registry) Disabled() []DisabledProvider {
	return append([]DisabledProvider(nil), r.disabled...)
}

// Get returns the provider registered under id.
func (r *Registry) Get(id string) (contracts.Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// ContentLanguage reports the content language the provider under id
// declares for its dubs ("ru", "ja", …); "" when the provider or its
// language is unknown. The lookup peels the wrapper layers NewRegistry
// puts around providers (SearchDelegator, the dub stream filter) down
// to the concrete provider.
func (r *Registry) ContentLanguage(id string) string {
	p, ok := r.byID[id]
	if !ok {
		return ""
	}
	lc, ok := bareProvider(p).(interface{ ContentLanguage() string })
	if !ok {
		return ""
	}
	return lc.ContentLanguage()
}

// NamePreference reports the search-name preference the provider under
// id declares (PR42): NamePrefLatin for the latin-only torrent feeds,
// NamePrefDefault for everyone else (including unknown ids). The
// lookup peels the wrapper layers down to the concrete provider.
func (r *Registry) NamePreference(id string) contracts.NamePreference {
	p, ok := r.byID[id]
	if !ok {
		return contracts.NamePrefDefault
	}
	np, ok := bareProvider(p).(contracts.NamePreferenceProvider)
	if !ok {
		return contracts.NamePrefDefault
	}
	return np.NamePreference()
}

// bareProvider peels the registry wrapper layers (SearchDelegator, the
// dub stream filter) down to the concrete provider they serve.
func bareProvider(p contracts.Provider) contracts.Provider {
	for {
		switch w := p.(type) {
		case SearchDelegator:
			p = w.Provider
		case dubFilteredProvider:
			p = w.Provider
		default:
			return p
		}
	}
}

// TorrentProviderIDs returns the ids of registered torrent-type
// providers (contracts.TorrentProvider capability): their results are
// torrent links resolved by the embedded core, not the HTTP-embed
// pipeline. Wrapper layers are peeled before the check.
func (r *Registry) TorrentProviderIDs() []string {
	out := make([]string, 0)
	for _, p := range r.order {
		if tp, ok := bareProvider(p).(contracts.TorrentProvider); ok && tp.IsTorrent() {
			out = append(out, p.ID())
		}
	}
	return out
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
