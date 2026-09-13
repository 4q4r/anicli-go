package cfbrowser

import (
	"fmt"
	"path/filepath"

	"github.com/an0nx/anicli-go/internal/config"
)

// Manager composes the full CF-bypass stack from Settings: clearance
// store, lazy-launching solver and the network-gated auto-updater.
// It is the single wiring point for the provider registry and the
// `anicli cf` commands, so lifecycle (browser session, updater
// ticker) is owned in one place.
type Manager struct {
	// Solver harvests clearances (lazy browser launch).
	Solver *Solver
	// Store persists clearances under the data dir.
	Store *ClearanceStore
	// Updater keeps the cached stealth Chromium current.
	Updater *Updater
}

// NewManager builds the stack from settings. A disabled [cf] section
// returns a nil Manager without error — callers treat nil as "no
// ladder". An enabled section with no resolvable binary fails loud
// with the `anicli cf install` hint (BinaryMissingError).
func NewManager(cfg config.Settings) (*Manager, error) {
	if !cfg.CF.Enabled {
		return nil, nil
	}

	base := cfg.General.DataDir
	if base == "" {
		var err error
		base, err = config.DataDir()
		if err != nil {
			return nil, fmt.Errorf("cfbrowser: resolve data dir: %w", err)
		}
	}
	store := NewClearanceStore(filepath.Join(base, "cfstore.json"), DefaultClearanceTTL)

	bin, err := ResolveCurrentBinary(ResolveOptions{})
	if err != nil {
		return nil, err
	}

	solver := NewSolver(SolverConfig{
		ProxyURL:           cfg.Network.ProxyURL,
		SolveTimeout:       cfg.CF.SolveTimeout,
		BrowserIdleTimeout: cfg.CF.BrowserIdleTimeout,
		Store:              store,
		Binary:             bin,
	})
	updater := NewUpdater(UpdaterConfig{
		Enabled:  AutoUpdateFromConfig(cfg.CF.AutoUpdate),
		Interval: cfg.CF.UpdateInterval,
	})
	solver.SetUpdater(updater)
	updater.Start()

	return &Manager{Solver: solver, Store: store, Updater: updater}, nil
}

// Close tears the stack down: the solver first — its Close CANCELS
// in-flight solves (it does not wait for them) and kills the browser
// process group synchronously — then the updater ticker. Idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var firstErr error
	if m.Solver != nil {
		if err := m.Solver.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if m.Updater != nil {
		m.Updater.Close()
	}
	return firstErr
}
