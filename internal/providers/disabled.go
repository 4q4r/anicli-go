package providers

import (
	"github.com/an0nx/anicli-go/internal/config"
)

// DisabledProvider names one provider that was excluded from the
// registry at startup because it cannot operate without user-supplied
// configuration (PR24). Reason is the user-facing RU explanation
// rendered by the startup notice, the doctor table and the health
// screen.
type DisabledProvider struct {
	ID     string
	Reason string
}

// unconfiguredRules lists every credential-gated provider and the
// predicate that decides whether it can run. Grow the table as new
// credentialled providers land.
var unconfiguredRules = []struct {
	id       string
	disabled func(cfg config.Settings) (reason string, disabled bool)
}{
	{
		id: "kodik",
		disabled: func(cfg config.Settings) (string, bool) {
			if cfg.Providers.Kodik.Token == "" {
				return "не задан токен (providers.kodik.token)", true
			}
			return "", false
		},
	},
	{
		// anilibria-torrent (PR37): no credentials of its own, but its
		// results resolve through the torrent core — without the
		// [torrent] subsystem it cannot play anything (kodik-parity:
		// never register a provider that cannot run).
		id: "anilibria-torrent",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// animetosho (PR38): no credentials, but its results resolve
		// through the torrent core — without the [torrent] subsystem
		// it cannot play anything (kodik-parity: never register a
		// provider that cannot run).
		id: "animetosho",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// tokyotosho (PR38): no credentials, but its results resolve
		// through the torrent core — without the [torrent] subsystem
		// it cannot play anything (kodik-parity: never register a
		// provider that cannot run).
		id: "tokyotosho",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// rutor (PR87): fully anonymous (search and .torrent
		// downloads), but its results resolve through
		// the torrent core — without the [torrent] subsystem it
		// cannot play anything (kodik-parity: never register a
		// provider that cannot run).
		id: "rutor",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// anirena (PR88): no credentials, but its results resolve
		// through the torrent core — without the
		// [torrent] subsystem it cannot play anything (kodik-parity:
		// never register a provider that cannot run).
		id: "anirena",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// subsplease (PR89): no credentials, but its results resolve
		// through the torrent core — without the
		// [torrent] subsystem it cannot play anything (kodik-parity:
		// never register a provider that cannot run). Integration
		// note (fix/93): the rule was missing from the PR89 branch and
		// is restored here so the whole torrent family shares the
		// same behavior.
		id: "subsplease",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
}

// UnconfiguredProviders reports every provider that cannot run with
// the given settings — the startup fan-out exclusion set (PR24). The
// set never includes providers already dropped by
// [providers].exclude: explicit user exclusion is a different (silent,
// logged) path that predates this table.
func UnconfiguredProviders(cfg config.Settings) []DisabledProvider {
	excluded := make(map[string]bool, len(cfg.Providers.Exclude))
	for _, id := range cfg.Providers.Exclude {
		excluded[id] = true
	}
	var out []DisabledProvider
	for _, rule := range unconfiguredRules {
		if excluded[rule.id] {
			continue
		}
		if reason, disabled := rule.disabled(cfg); disabled {
			out = append(out, DisabledProvider{ID: rule.id, Reason: reason})
		}
	}
	return out
}

// unconfiguredIDs renders the exclusion table as a set for the
// factory fast path.
func unconfiguredIDs(cfg config.Settings) map[string]DisabledProvider {
	list := UnconfiguredProviders(cfg)
	out := make(map[string]DisabledProvider, len(list))
	for _, d := range list {
		out[d.ID] = d
	}
	return out
}

// DisabledProvidersFor computes the unconfigured-provider set the way
// the factory assembles it (PR140): a Lua script serving a normally-
// unconfigured id UN-DISABLES it — the active script (bundled or
// user-dir) is the credential gate then, failing loud on use instead
// of warning at startup (the kodik fail-loud-on-use ruling). The
// startup notices derive from this so they never claim a provider is
// disabled while its script serves the roster slot.
//
// Precision note: the factory's own disabled set keys off LOADED
// scripts, so a broken user override of an unconfigured id keeps its
// notice there; this source-presence variant (no script loading)
// suppresses it — the registry-driven doctor rendering stays the
// precise surface.
func DisabledProvidersFor(cfg config.Settings) []DisabledProvider {
	unconfigured := UnconfiguredProviders(cfg)
	if !cfg.Providers.Lua.Enabled {
		return unconfigured
	}
	excluded := make(map[string]bool, len(cfg.Providers.Exclude))
	for _, id := range cfg.Providers.Exclude {
		excluded[id] = true
	}
	// The LoadSources precedence assembly, id-only: first occurrence
	// wins, exclusions drop (luaScriptSources is the factory's own
	// list — user config dir, [providers.lua].dir, bundled embeds).
	served := make(map[string]bool)
	seen := make(map[string]bool)
	for _, src := range luaScriptSources(cfg) {
		if seen[src.ID] || excluded[src.ID] {
			continue
		}
		seen[src.ID] = true
		served[src.ID] = true
	}
	out := make([]DisabledProvider, 0, len(unconfigured))
	for _, d := range unconfigured {
		if served[d.ID] {
			continue
		}
		out = append(out, d)
	}
	return out
}
