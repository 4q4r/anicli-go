package lua

import (
	"log/slog"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Source is one provider script handed to LoadSources: the directory
// identity (the script id MUST equal it — LoadProvider validates),
// the script bytes and the origin label for diagnostics ("bundled",
// a user directory path, …).
type Source struct {
	ID  string
	Src string
	Dir string
}

// HTTPFor builds the per-provider transport for the provider id (the
// same one-netclient-per-provider isolation the compiled providers
// get: own cookie jar, browser fingerprint, CF ladder). Nil disables
// the wiring (SDK HTTP falls back to the plain stdlib client — tests
// and sandboxes).
type HTTPFor func(id string) *netclient.Client

// SettingsFor builds the flattened per-provider settings map for the
// provider id — the providers.<id>.<key> string values
// anicli.provider_setting reads (PR140). Nil disables the wiring
// (scripts read nil for every key — tests and sandboxes).
type SettingsFor func(id string) map[string]string

// LoadSources loads and validates provider scripts in the caller's
// precedence order (highest-precedence caller first — the factory
// passes the user config dir, then [providers.lua].dir, then the
// bundled embeds): the FIRST occurrence of an id wins and later
// duplicates become skips — the same-shadowing rule the factory
// applies against the Go roster.
// The returned providers carry the script-declared capability
// adapters (Adapt) — the exact shape the registry consumes.
//
// Load-time failures (syntax, contract shape, id mismatch) and
// duplicate ids are collected as DiscoveryErrors and logged; one
// broken script never blocks the others and never fails the call —
// discovery is never fatal to startup (the discovery.go contract).
// Every returned provider routes its SDK HTTP through httpFor(id) and
// its anicli.provider_setting reads through settingsFor(id).
func LoadSources(cfg Config, log *slog.Logger, sources []Source, httpFor HTTPFor, settingsFor SettingsFor) ([]contracts.Provider, []DiscoveryError) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}

	var (
		provs []contracts.Provider
		skips []DiscoveryError
		seen  = map[string]bool{}
	)
	for _, src := range sources {
		if seen[src.ID] {
			err := DiscoveryError{Dir: src.ID, Err: loadErrf("duplicate id — shadowed by a higher-precedence source")}
			skips = append(skips, err)
			log.Warn("lua: provider script skipped", "provider", src.ID, "source", src.Dir, "error", err.Err.Error())
			continue
		}

		engineCfg := cfg
		if httpFor != nil {
			engineCfg.HTTP = httpFor(src.ID)
		}
		if settingsFor != nil {
			engineCfg.ProviderSettings = settingsFor(src.ID)
		}
		p, err := NewEngine(engineCfg, log).LoadProvider(src.ID, src.Src)
		if err != nil {
			skips = append(skips, DiscoveryError{Dir: src.ID, Err: err})
			log.Warn("lua: provider script skipped", "provider", src.ID, "source", src.Dir, "error", err.Error())
			continue
		}
		seen[src.ID] = true
		provs = append(provs, p.Adapt())
		log.Info("lua: provider loaded", "provider", src.ID, "source", src.Dir)
	}
	return provs, skips
}
