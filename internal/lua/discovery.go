package lua

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// DiscoveryError reports one skipped provider script with its
// directory identity — discovery is never fatal to startup.
type DiscoveryError struct {
	// Dir is the provider directory name (the intended provider id).
	Dir string
	// Err is the load/validation failure.
	Err error
}

func (e DiscoveryError) Error() string { return "provider " + e.Dir + ": " + e.Err.Error() }

// ProvidersDir resolves <user config>/anicli/providers, honoring
// XDG_CONFIG_HOME. ok is false when the user config dir itself cannot
// be resolved (headless environments without XDG).
func ProvidersDir() (dir string, ok bool) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(base, "anicli", "providers"), true
}

// Discover scans <user config>/anicli/providers/<id>/main.lua and
// loads every conforming script into a sandboxed Provider. Failures
// (missing id match, incomplete contract, syntax, budget overrun)
// are collected as DiscoveryErrors and skipped — one broken script
// never blocks the others or the startup. Every provider invocation
// later runs in a fresh sandboxed LState under the same Config
// budgets (see engine.go).
func Discover(cfg Config, log *slog.Logger) ([]*Provider, []DiscoveryError) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	var (
		provs []*Provider
		skips []DiscoveryError
	)

	dir, ok := ProvidersDir()
	if !ok {
		return provs, skips
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No providers tree (or unreadable): a plain no-op.
		return provs, skips
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		main := filepath.Join(dir, id, "main.lua")
		src, err := os.ReadFile(main) // #nosec G304 -- reading the user's own provider scripts is this package's purpose
		if err != nil {
			continue // no main.lua — not a provider directory
		}
		p, err := LoadProviderBytes(cfg, log, id, src)
		if err != nil {
			skips = append(skips, DiscoveryError{Dir: id, Err: err})
			log.Warn("lua: provider script skipped",
				"provider", id, "error", err.Error())
			continue
		}
		provs = append(provs, p)
		log.Info("lua: provider discovered", "provider", id)
	}
	return provs, skips
}

// Compile-time contract check for the discovery output.
var _ contracts.Provider = (*Provider)(nil)
