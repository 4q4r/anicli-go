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

// ScanDir reads one provider script directory into raw sources: every
// <dir>/<id>/main.lua becomes a Source (Dir carries the origin for
// diagnostics). A missing or unreadable dir scans to nothing — a
// plain no-op, never an error. The factory (PR116) assembles the
// bundled sources, the config dir and this user dir into the
// precedence-ordered list LoadSources consumes.
func ScanDir(dir string) []Source {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Source
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
		out = append(out, Source{ID: id, Src: string(src), Dir: dir})
	}
	return out
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

	for _, src := range ScanDir(dir) {
		p, err := LoadProviderBytes(cfg, log, src.ID, []byte(src.Src))
		if err != nil {
			skips = append(skips, DiscoveryError{Dir: src.ID, Err: err})
			log.Warn("lua: provider script skipped",
				"provider", src.ID, "error", err.Error())
			continue
		}
		provs = append(provs, p)
		log.Info("lua: provider discovered", "provider", src.ID)
	}
	return provs, skips
}

// Compile-time contract check for the discovery output.
var _ contracts.Provider = (*Provider)(nil)
