package lua

import (
	"log/slog"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// The capability adapter (PR116): contracts.Provider is a small
// interface, but the registry and the parity smoke consume several
// OPTIONAL duck-typed capabilities on top of it (ContentLanguage,
// contracts.NamePreferenceProvider, contracts.SmokeQueryProvider).
// A script declares them via the optional table fields
// content_lang / name_preference / smoke_query; Adapt() wraps the
// provider in ONE composite carrying all three as plain fields.
//
// Why one composite (not one wrapper per capability): Go type
// assertions do not promote methods through nested interface
// embedding — a SmokeQuery wrapper around a ContentLanguage wrapper
// would HIDE ContentLanguage from the registry's duck check (found
// live on the anitokyo migration). The zero values are observationally
// identical to not-implemented at every consumer:
//
//   - Registry.ContentLanguage returns "" for both a non-implementer
//     and an empty declaration;
//   - Registry.NamePreference returns NamePrefDefault for both;
//   - the parity smoke guards `declared != ""` before overriding the
//     shared probes (parity.go).
//
// A script declaring NOTHING wraps to itself: no wrapper, no
// capability surfaces at all.
type adapted struct {
	contracts.Provider
	contentLang string
	smokeQuery  string
	namePref    contracts.NamePreference
	// torrent is the script's torrent-provider declaration (the rutor
	// PR142 precedent): the factory reads it to graft the Go engine
	// legs on; it is NOT the contracts.TorrentProvider capability —
	// that lives on the factory-built adapter.
	torrent bool
}

func (a adapted) ContentLanguage() string { return a.contentLang }

func (a adapted) NamePreference() contracts.NamePreference { return a.namePref }

func (a adapted) SmokeQuery() string { return a.smokeQuery }

// Torrent reports the script's torrent-provider declaration. A
// distinct accessor (not IsTorrent) on purpose: the capability the
// registry duck-types stays on the providers-package adapter, which
// also carries the engine plumbing — a bare declared script satisfies
// nothing torrent-shaped.
func (a adapted) Torrent() bool { return a.torrent }

// SetLogger forwards the registry's logger seam to the wrapped
// provider (the lua.Engine log sink). Without the forward the
// composite HIDES the concrete method from the registry's duck probe
// — the exact nesting trap this composite exists to prevent; the
// NewRegistry wiring documents this promotion as the Lua providers'
// log route.
func (a adapted) SetLogger(log *slog.Logger) {
	if sl, ok := a.Provider.(interface{ SetLogger(*slog.Logger) }); ok {
		sl.SetLogger(log)
	}
}

// HTTPClient forwards the wrapped provider's transport seam (the
// luaTorrent adapter probes the surfaced .torrent links through the
// SAME netclient the script's search used). Nil when the inner
// provider carries no wired transport.
func (a adapted) HTTPClient() *netclient.Client {
	if hc, ok := a.Provider.(interface{ HTTPClient() *netclient.Client }); ok {
		return hc.HTTPClient()
	}
	return nil
}

// Adapt wraps the provider in the capability adapter when the script
// declared at least one optional surface; bare returns the provider
// itself.
func (p *Provider) Adapt() contracts.Provider {
	if p.contentLang == "" && p.smokeQuery == "" && p.namePref == contracts.NamePrefDefault && !p.torrent {
		return p
	}
	return adapted{
		Provider:    p,
		contentLang: p.contentLang,
		smokeQuery:  p.smokeQuery,
		namePref:    p.namePref,
		torrent:     p.torrent,
	}
}

// ScriptProvider peels the capability adapter down to the script
// provider. The factory's torrent-wrap decision reads the script's
// torrent declaration off the peeled provider; the wrap itself keeps
// the OUTER surface so the declared capabilities survive on the
// roster entry. Non-Lua providers (the compiled factories) report
// false.
func ScriptProvider(p contracts.Provider) (*Provider, bool) {
	switch v := p.(type) {
	case *Provider:
		return v, true
	case adapted:
		lp, ok := v.Provider.(*Provider)
		return lp, ok
	default:
		return nil, false
	}
}
