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
	"io"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// discardLogger is the unwired provider logger's sink: output goes
// nowhere, never to stderr (the TUI alt-screen contract, PR62 #4).
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// Base carries the fields every provider shares: identity, site root
// and content language. Providers embed it and implement the three
// operations themselves (port of the BaseSource attributes in
// anicli-py anicli/core/base.py:8-33).
type Base struct {
	id         string
	name       string
	baseURL    string
	sourceType contracts.SourceType
	// contentLang is the provider's primary content language ("ru",
	// "ja", …): the tag every dub the service emits carries. It is a
	// service-level declaration, not per-dub introspection (PR23).
	contentLang string
	http        *netclient.Client
	// logger routes provider-level diagnostics (search-preflight
	// drops, …). Nil degrades to discard — NEVER slog.Default, whose
	// stderr output corrupts the TUI alt-screen (PR62 #4; the PR42
	// flagged seam).
	logger *slog.Logger
}

// SetLogger injects the diagnostics sink (the registry does it for
// every provider carrying this seam).
func (b *Base) SetLogger(log *slog.Logger) { b.logger = log }

// loggerOrDiscard returns the injected logger; nil degrades to a
// discard logger so an unwired provider can never write to stderr.
func (b *Base) loggerOrDiscard() *slog.Logger {
	if b.logger == nil {
		return discardLogger
	}
	return b.logger
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

// ContentLanguage returns the provider's primary content language tag
// ("ru", "ja", …); "" when undeclared. The [RU]/[JA] dub tags in the
// TUI derive from it via dubLangTag — not from a per-dub field.
func (b Base) ContentLanguage() string { return b.contentLang }

// pyQuote (urllib.parse.quote with its default safe="/" set — spaces
// %20, one UTF-8 byte at a time) died with the PR141 hdrezka
// migration: its last Go consumer was hdrezka.go, and the anidub,
// anistar and hdrezka search queries all re-derive the encoding in
// their Lua scripts now (the SDK's query_escape is form-style "+",
// which is exactly why the scripts cannot use it).
