//go:build live

package tui

// LIVE probe for PR94 (fail-soft merged resolve with honest
// attribution). Excluded from the hermetic default suite by the
// `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR94 -count=1 -v ./internal/tui/
//
// Optional proxy for foreign networks:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 go test -tags live ...
//
// The probe merges a 2+ provider episode and runs the REAL
// resolveAllStreams: broken providers (allanime is the known
// CF-pending 400 case) must degrade fail-soft — named in the SKIP
// SUMMARY, absent from the list — while the healthy entries still
// surface. A fully broken roster must produce the typed
// *errResolveFailed naming every consulted provider.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestLivePR94FailSoftSummary(t *testing.T) {
	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "магическая битва"
	}
	t.Logf("fan-out search: %q", query)
	_, deps := liveDeps(t)
	bySource := liveSearch(t, deps, query)
	if len(bySource) < 2 {
		t.Skipf("need results from 2+ providers for the fail-soft proof, got %d", len(bySource))
	}
	group := stableGroup(func() []contracts.SearchResult {
		out := make([]contracts.SearchResult, 0, len(bySource))
		for _, r := range bySource {
			out = append(out, r)
		}
		return out
	}())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	parts := []SourceEpisodes{}
	for _, res := range group {
		eps, err := deps.Episode.GetEpisodes(ctx, res.SourceID, res.URL)
		if err != nil {
			t.Logf("  episodes %-14s → err: %v", res.SourceID, err)
			continue
		}
		parts = append(parts, SourceEpisodes{SourceID: res.SourceID, Episodes: eps})
	}
	if len(parts) < 2 {
		t.Skipf("need episode lists from 2+ providers, got %d", len(parts))
	}
	merged, order := MergeEpisodeLists(parts)
	ep := merged[order[0]]
	for _, part := range strings.Split(ep.RawID, "|") {
		prov, id, found := strings.Cut(part, ":")
		if !found || hasProviderEmbeds(ep.RawEmbeds, prov) {
			continue
		}
		local := contracts.Episode{Num: ep.Num, RawID: id, RawEmbeds: map[string][]string{}}
		out, err := deps.Episode.HydrateDubs(ctx, prov, local)
		if err != nil {
			t.Logf("  hydrate %-12s → err: %v", prov, err)
			continue
		}
		for dub, links := range out.RawEmbeds {
			if len(links) == 0 {
				continue
			}
			if ep.RawEmbeds == nil {
				ep.RawEmbeds = map[string][]string{}
			}
			ep.RawEmbeds["["+prov+"] "+dub] = links
		}
	}
	if len(ep.RawEmbeds) == 0 {
		t.Skipf("no hydrated embeds for episode %q", ep.Num)
	}
	t.Logf("episode %q consulted dub keys: %v", ep.Num, sortedEmbedKeys(ep.RawEmbeds))

	// The production wiring passes the deps logger (the file sink):
	// the same seam logs every full provider error while the screen
	// line stays compact (#161). The live probe mirrors it so the
	// run's log shows both forms side by side.
	entries, skipped, err := resolveAllStreams(ctx, deps.Episode, ep, "", deps.Log)
	if err != nil {
		var typed *errResolveFailed
		if !errors.As(err, &typed) {
			t.Fatalf("a zero-entry resolve must fail with the typed all-failed error, got %T: %v", err, err)
		}
		t.Logf("ALL-BROKEN VERDICT (typed): %v", err)
		return
	}

	if line := skippedSummary(skipped); line != "" {
		t.Logf("SKIP SUMMARY: %s", line)
		for _, f := range skipped {
			consulted := false
			for _, k := range sortedEmbedKeys(ep.RawEmbeds) {
				if providerOfTrackKey(k) == f.Provider {
					consulted = true
					break
				}
			}
			if !consulted {
				t.Errorf("skip record names %q — never consulted on this episode", f.Provider)
			}
		}
	} else {
		t.Logf("SKIP SUMMARY: (none — every consulted provider resolved)")
	}
	s := &sessionScreen{deps: deps, dubStats: DubStats([]contracts.Episode{ep})}
	t.Logf("HEALTHY MERGED ENTRIES (%d, quality-sorted):", len(entries))
	for i, e := range entries {
		t.Logf("  %2d. %s | url=%s", i, s.streamEntryLabel(e), e.Source.URL)
	}
	if len(entries) == 0 {
		t.Fatal("the fail-soft proof needs at least one healthy entry")
	}
}
