package lua

import (
	"github.com/an0nx/anicli-go/internal/contracts"
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
}

func (a adapted) ContentLanguage() string { return a.contentLang }

func (a adapted) NamePreference() contracts.NamePreference { return a.namePref }

func (a adapted) SmokeQuery() string { return a.smokeQuery }

// Adapt wraps the provider in the capability adapter when the script
// declared at least one optional surface; bare returns the provider
// itself.
func (p *Provider) Adapt() contracts.Provider {
	if p.contentLang == "" && p.smokeQuery == "" && p.namePref == contracts.NamePrefDefault {
		return p
	}
	return adapted{
		Provider:    p,
		contentLang: p.contentLang,
		smokeQuery:  p.smokeQuery,
		namePref:    p.namePref,
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
