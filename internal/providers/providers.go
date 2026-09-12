// Package providers hosts the anime source ports: one file per provider,
// a shared base helper, and the registry that wires providers to the
// network client and the search-stat repository.
//
// Every provider is a faithful Go port of its Python original in the
// frozen anicli-py repository (see the provenance comments in each file).
// Divergences are mandated by the Go contracts (context-first signatures,
// string quality keys) or by explicit task ruling and are documented at
// the divergence site.
package providers

import (
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Base carries the fields every provider shares: identity, site root and
// the per-provider HTTP headers applied on every request. Providers embed
// it and implement the three operations themselves (port of the
// BaseSource attributes in anicli-py anicli/core/base.py:8-33).
type Base struct {
	id         string
	name       string
	baseURL    string
	sourceType contracts.SourceType
	headers    map[string]string
	http       *netclient.Client
}

// ID returns the stable provider identifier (Python source_id).
func (b Base) ID() string { return b.id }

// Name returns the human-readable provider name (Python name).
func (b Base) Name() string { return b.name }

// BaseURL returns the provider site root URL (Python base_url).
func (b Base) BaseURL() string { return b.baseURL }

// SourceType reports what kind of content the provider serves (Python
// source_type / SourceCapability).
func (b Base) SourceType() contracts.SourceType { return b.sourceType }

// pythonFloatKey ports the Python episode sort keys of the form
//
//	float(x.num) if x.num.replace('.', '', 1).isdigit() else 0
//
// (anicli-py anilib.py:114, sovetromantica.py:82): strip the first dot,
// require at least one remaining digit rune, else the key is 0.
func pythonFloatKey(num string) float64 {
	stripped := strings.Replace(num, ".", "", 1)
	if stripped == "" {
		return 0
	}
	for _, r := range stripped {
		if r < '0' || r > '9' {
			return 0
		}
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		// Guarded by the digit check above (single dot); kept for safety.
		return 0
	}
	return f
}
