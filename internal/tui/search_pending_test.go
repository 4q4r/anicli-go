package tui

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestSearchPendingGuardedBeforeSettlement: rows are PENDING from
// construction (PR24) — before any command settles, the view must not
// render error verdicts and Enter must not advance. This pins the
// live async behavior the synchronous drain-based tests cannot see.
func TestSearchPendingGuardedBeforeSettlement(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}

	t.Run("without shikimori: rows pending, enter blocked", func(t *testing.T) {
		deps := hybridDeps(fs, nil, nil, nil)
		sp := NewSearchProgress(deps, "наруто")
		_ = sp.Init()

		v := sp.View().Content
		if strings.Contains(v, "✗") {
			t.Fatalf("unsettled rows must not render error verdicts, got:\n%s", v)
		}
		if strings.Contains(v, "✓ Завершено") {
			t.Fatalf("unsettled rows must not render success verdicts, got:\n%s", v)
		}
		if _, cmd := sp.Update(enter()); cmd != nil {
			t.Fatalf("enter must be blocked while rows are pending")
		}
	})

	t.Run("during enrichment: shikimori notice, enter blocked", func(t *testing.T) {
		shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Наруто": 21}}
		deps := hybridDeps(fs, shiki, &fakeMetadata{}, nil)
		sp := NewSearchProgress(deps, "наруто")
		_ = sp.Init()

		if !sp.enriching {
			t.Fatalf("enabled shikimori must open in the enrichment phase")
		}
		v := sp.View().Content
		if !strings.Contains(v, "Shikimori") {
			t.Fatalf("the enrichment phase must render its notice, got:\n%s", v)
		}
		if strings.Contains(v, "✗") {
			t.Fatalf("unsettled rows must not render error verdicts, got:\n%s", v)
		}
		if _, cmd := sp.Update(enter()); cmd != nil {
			t.Fatalf("enter must be blocked during enrichment")
		}
		// Settling the enrichment without draining the fan-out keeps
		// the rows pending.
		_, _ = sp.Update(searchVariantsMsg{})
		if len(sp.pending) != 1 {
			t.Fatalf("fan-out start must mark rows pending, got %v", sp.pending)
		}
	})
}
