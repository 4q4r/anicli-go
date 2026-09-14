package tui

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestResolveSearchVariantsNilShikiSkipsEnrichment pins PR25 issue 1:
// with no Shikimori service wired the enrichment degrades to the bare
// query instead of panicking or blocking — Shikimori is a bonus, never
// a requirement of hybrid search.
func TestResolveSearchVariantsNilShikiSkipsEnrichment(t *testing.T) {
	t.Parallel()

	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, nil, md, nil) // Shiki deliberately nil

	msg := resolveSearchVariants(deps, "наруто")
	if len(msg.variants) != 0 {
		t.Fatalf("nil shiki must yield no variants, got %v", msg.variants)
	}
	if len(md.queries) != 0 {
		t.Fatalf("metadata must not be consulted without shikimori, got %v", md.queries)
	}
}

// TestResolveSearchVariantsDisabledModeSkipsEnrichment pins the guard
// contract: Mode() == "disabled" skips the enrichment entirely —
// SearchIDs is never called even though the service is wired.
func TestResolveSearchVariantsDisabledModeSkipsEnrichment(t *testing.T) {
	t.Parallel()

	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	shiki := &fakeShiki{enabled: false, mode: "disabled", ids: map[string]int64{"Наруто": 21}}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, shiki, md, nil)

	msg := resolveSearchVariants(deps, "наруто")
	if len(msg.variants) != 0 {
		t.Fatalf("disabled shikimori must yield no variants, got %v", msg.variants)
	}
	if len(shiki.queries) != 0 {
		t.Fatalf("SearchIDs must not run in disabled mode, got %v", shiki.queries)
	}
	if len(md.queries) != 0 {
		t.Fatalf("metadata must not be consulted in disabled mode, got %v", md.queries)
	}
}
