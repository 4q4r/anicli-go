//go:build live

package tui

// LIVE probe for PR97 (all-names search redesign). Excluded from the
// hermetic default suite by the `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR97AllNames -count=1 -v ./internal/tui/
//
// Prints, for «Ателье колдовских колпаков»:
//  1. the resolved variant list (must carry all 5 names of the card:
//     russian, original, english, japanese, synonyms);
//  2. the per-provider fan-out: every variant each provider ran and
//     what each variant returned — proving results arrive from BOTH
//     the RU and the EN variants.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestLivePR97AllNames(t *testing.T) {
	_, deps := liveDeps(t)

	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "Ателье колдовских колпаков"
	}

	// Phase 1: the variant resolution (top card → GetAnime → all
	// names).
	msg := resolveSearchVariants(deps, query)
	if len(msg.variants) == 0 {
		t.Fatalf("no variants resolved (is Shikimori reachable?) for %q", query)
	}
	t.Logf("=== VARIANTS (%d) for %q ===", len(msg.variants), query)
	for i, v := range msg.variants {
		t.Logf("  %2d. %s", i+1, v)
	}
	for _, want := range []string{
		"Ателье колдовских колпаков",
		"Tongari Boushi no Atelier",
		"Witch Hat Atelier",
		"とんがり帽子のアトリエ",
		"Atelier of Witch Hat",
	} {
		found := false
		for _, v := range msg.variants {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("variant list missing card name %q", want)
		}
	}

	// Phase 2: the fan-out over three routed providers — a RU stream
	// site, a latin stream site, a latin torrent feed.
	probe := []struct {
		id   string
		lang string
	}{
		{"anilibria", deps.Episode.ContentLanguage("anilibria")},
		{"gogoanime", deps.Episode.ContentLanguage("gogoanime")},
		{"subsplease", deps.Episode.ContentLanguage("subsplease")},
	}
	for _, p := range probe {
		queries := queriesForLanguage(msg.variants, p.lang)
		if deps.Search.NamePreference(p.id) == contracts.NamePrefLatin {
			if latin := latinOnlyQueries(queries); len(latin) > 0 {
				queries = latin
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		out := searchProviderVariants(ctx, deps, p.id, queries)
		cancel()
		t.Logf("=== %s (%s-first, %d variants) → %d merged results, err=%v ===",
			p.id, p.lang, len(queries), len(out.results), out.err)
		for i, q := range queries {
			n := 0
			suffix := ""
			if out.err == nil {
				// Re-run counting per variant for the proof log only:
				// the merge itself happened inside
				// searchProviderVariants; this walk shows which
				// variant produced what.
				sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
				res, err := deps.Search.Search(sctx, p.id, q)
				scancel()
				n = len(res)
				if err != nil {
					suffix = fmt.Sprintf(" (err: %v)", err)
				}
			}
			t.Logf("    %2d. %-40q → %d results%s", i+1, q, n, suffix)
			_ = i
		}
		for j, r := range out.results {
			if j >= 8 {
				t.Logf("    … and %d more", len(out.results)-8)
				break
			}
			t.Logf("    → [%s] %s", r.SourceID, r.Title)
		}
	}
}
