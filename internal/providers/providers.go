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
	"encoding/json"
	"io"
	"log/slog"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// discardLogger is the unwired provider logger's sink: output goes
// nowhere, never to stderr (the TUI alt-screen contract, PR62 #4).
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// Base carries the fields every provider shares: identity, site root,
// content language and the per-provider HTTP headers applied on every
// request. Providers embed it and implement the three operations
// themselves (port of the BaseSource attributes in anicli-py
// anicli/core/base.py:8-33).
type Base struct {
	id         string
	name       string
	baseURL    string
	sourceType contracts.SourceType
	// contentLang is the provider's primary content language ("ru",
	// "ja", …): the tag every dub the service emits carries. It is a
	// service-level declaration, not per-dub introspection (PR23).
	contentLang string
	headers     map[string]string
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

// pythonStr ports Python's str() over a JSON number field: the wire
// literal is preserved ("1" stays "1", "1.5" stays "1.5"), and a missing
// field yields "None" exactly like str(None) on a JSON null (this was
// load-bearing for anilib, whose Lua script renders the same "None"
// numbering; animevost/anilibria/yummy keep the Go shape).
func pythonStr(n json.Number) string {
	if n == "" {
		return "None"
	}
	return n.String()
}

// pyQuote ports urllib.parse.quote with its default safe="/" set:
// every byte outside the URL-unreserved set (and "/") is percent-
// encoded uppercase, one UTF-8 byte at a time — spaces become %20, not
// the form-style "+" of url.Values.Encode. Originally ported for
// sovetromantica (sovetromantica.py:30); its live consumers are the
// anidub, anistar and hdrezka search queries.
func pyQuote(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~' || c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}
