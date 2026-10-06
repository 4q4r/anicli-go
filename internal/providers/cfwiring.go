package providers

import (
	"context"
	"fmt"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// cfSolverAdapter bridges the cfbrowser solver onto the consumer-side
// netclient.CFSolver interface: converts the clearance shape and
// forwards host invalidation to the shared store (refresh-on-403).
type cfSolverAdapter struct {
	solver *cfbrowser.Solver
	store  *cfbrowser.ClearanceStore
}

// SolveChallenge replays/solves the clearance for targetURL's host.
// The timeout comes from [cf].solve_timeout baked into the solver.
func (a cfSolverAdapter) SolveChallenge(ctx context.Context, targetURL string) (netclient.CFClearance, error) {
	c, err := a.solver.SolveChallenge(ctx, targetURL, 0)
	if err != nil {
		return netclient.CFClearance{}, err
	}
	cookies := make([]netclient.CFCookie, 0, len(c.Cookies))
	for _, ck := range c.Cookies {
		cookies = append(cookies, netclient.CFCookie{
			Name:   ck.Name,
			Value:  ck.Value,
			Domain: ck.Domain,
			Path:   ck.Path,
		})
	}
	return netclient.CFClearance{
		Cookies:        cookies,
		UserAgent:      c.UserAgent,
		AcceptLanguage: c.AcceptLanguage,
	}, nil
}

// InvalidateHost drops the host's cached clearance before a re-solve.
func (a cfSolverAdapter) InvalidateHost(host string) {
	if err := a.store.Delete(host); err != nil {
		// Store persistence failure must not break the ladder; the
		// fresh solve overwrites the entry anyway.
		_ = err
	}
}

// baseURLs maps provider IDs onto their primary base URLs — the
// targets `anicli cf solve` opens the browser against. Migrated Lua
// providers leave the map as they go (the script owns its base; the
// anilib precedent).
var baseURLs = map[string]string{
	"anilibria": AniLibriaAPIBase,
	"sameband":  SameBandBase,
	"kodik":     KodikAPIBase,
	"anidub":    AnidubBase,
	"anizone":   AniZoneBase,
}

// BaseURLFor returns the primary base URL for a provider ID (ok is
// false for unknown IDs).
func BaseURLFor(id string) (string, bool) {
	u, ok := baseURLs[id]
	return u, ok
}

// buildCFOptions wires the CF ladder into provider clients (always
// on, PR80): receives the already-built manager (shared across the
// registry — NewRegistry builds it once) and returns the solver
// option plus its closer. mgr may be nil only for callers that skip
// NewManager; buildCFOptions then builds a fresh manager.
func buildCFOptions(cfg config.Settings, mgr *cfbrowser.Manager) (opts []netclient.Option, closer func(), err error) {
	if mgr == nil {
		mgr, err = cfbrowser.NewManager(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("build cf solver: %w", err)
		}
	}
	if mgr == nil {
		return nil, nil, nil
	}
	adapter := cfSolverAdapter{solver: mgr.Solver, store: mgr.Store}
	return []netclient.Option{netclient.WithCFSolver(adapter)}, func() { _ = mgr.Close() }, nil
}
