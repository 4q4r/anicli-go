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
		// nyaa (PR36) has no credentials of its own, but its results
		// resolve through the torrent core — without the [torrent]
		// subsystem it cannot play anything (kodik-parity: never
		// register a provider that cannot run).
		id: "nyaa",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// anilibria-torrent (PR37) is nyaa's sibling: no credentials,
		// but its results resolve through the torrent core — without
		// the [torrent] subsystem it cannot play anything
		// (kodik-parity: never register a provider that cannot run).
		id: "anilibria-torrent",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// animetosho (PR38) is nyaa's sibling too: no credentials, but
		// its results resolve through the torrent core — without the
		// [torrent] subsystem it cannot play anything (kodik-parity:
		// never register a provider that cannot run).
		id: "animetosho",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// tokyotosho (PR38) is nyaa's sibling too: no credentials, but
		// its results resolve through the torrent core — without the
		// [torrent] subsystem it cannot play anything (kodik-parity:
		// never register a provider that cannot run).
		id: "tokyotosho",
		disabled: func(cfg config.Settings) (string, bool) {
			if !cfg.Torrent.Enabled {
				return "выключена подсистема [torrent] (torrent.enabled)", true
			}
			return "", false
		},
	},
	{
		// rutor (PR87) is nyaa's sibling too: fully anonymous (search
		// and .torrent downloads), but its results resolve through
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
		// anirena (PR88) is nyaa's sibling too: no credentials, but its
		// results resolve through the torrent core — without the
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
		// subsplease (PR89) is nyaa's sibling too: no credentials, but
		// its results resolve through the torrent core — without the
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
