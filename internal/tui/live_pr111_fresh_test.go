//go:build live

package tui

// LIVE probe for PR111 (always-fresh stream resolution + the fixed
// «🔄 Refresh sources»). Excluded from the hermetic default suite
// by the `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR111FreshResolve -count=1 -v ./internal/tui/
//
// The probe takes a real provider episode and resolves its streams
// from TWO CONSECUTIVE FRESH hydration rounds (the PR111 pipeline):
// stream links rotate server-side, so each round may hand out
// different embed URLs and different playable links. The proof is the
// printed evidence — round 1 vs round 2 embeds and resolved links —
// plus the honest verdict: rotation observed, or the provider
// currently serves stable links (both resolves still succeed).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestLivePR111FreshResolve(t *testing.T) {
	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "магическая битва"
	}
	_, deps := liveDeps(t)
	bySource := liveSearch(t, deps, query)
	if len(bySource) == 0 {
		t.Skipf("no search results for %q", query)
	}
	if want := os.Getenv("ANICLI_LIVE_PROVIDER"); want != "" {
		if r, ok := bySource[want]; ok {
			bySource = map[string]contracts.SearchResult{want: r}
		}
	}
	group := stableGroup(func() []contracts.SearchResult {
		out := make([]contracts.SearchResult, 0, len(bySource))
		for _, r := range bySource {
			out = append(out, r)
		}
		return out
	}())

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Find the first provider whose episode 1 hydrates with embeds.
	var chosen struct {
		prov string
		ep   contracts.Episode
	}
walk:
	for _, res := range group {
		eps, err := deps.Episode.GetEpisodes(ctx, res.SourceID, res.URL)
		if err != nil || len(eps) == 0 {
			t.Logf("walk %-14s → episodes err: %v", res.SourceID, err)
			continue
		}
		for _, ep := range eps {
			local := contracts.Episode{Num: ep.Num, RawID: ep.RawID, RawEmbeds: map[string][]string{}}
			out, err := deps.Episode.HydrateDubs(ctx, res.SourceID, local)
			if err != nil {
				t.Logf("walk %-14s ep %-4s → hydrate err: %v", res.SourceID, ep.Num, err)
				continue
			}
			for dub, links := range out.RawEmbeds {
				if len(links) == 0 {
					continue
				}
				t.Logf("walk %-14s ep %-4s → dub %q: %d embed(s)", res.SourceID, ep.Num, dub, len(links))
				// The session pipeline consumes the MERGED shape
				// (provider-prefixed keys, "prov:id" RawID) — build it
				// exactly like the session does.
				merged, order := MergeEpisodeLists([]SourceEpisodes{{SourceID: res.SourceID, Episodes: eps}})
				chosen = struct {
					prov string
					ep   contracts.Episode
				}{prov: res.SourceID, ep: merged[order[0]]}
				break walk
			}
		}
	}
	if chosen.prov == "" {
		t.Skipf("no provider hydrated embeds for %q", query)
	}
	t.Logf("PROVIDER: %s · episode %q", chosen.prov, chosen.ep.Num)

	type round struct {
		embeds map[string][]string
		links  []string
	}
	collect := func(n int) round {
		fresh := hydrateEpisodeFresh(ctx, deps, chosen.ep)
		t.Logf("ROUND %d embeds:", n)
		for _, k := range sortedEmbedKeys(fresh.RawEmbeds) {
			t.Logf("  %s → %v", k, fresh.RawEmbeds[k])
		}
		entries, skipped, err := resolveAllStreams(ctx, deps.Episode, fresh, "")
		if err != nil {
			t.Fatalf("round %d resolve failed: %v (skipped: %v)", n, err, skipped)
		}
		links := make([]string, 0, len(entries))
		for _, e := range entries {
			links = append(links, e.Source.URL)
		}
		t.Logf("ROUND %d resolved %d streams:", n, len(entries))
		for _, e := range entries {
			fmt.Fprintf(os.Stderr, "  %sp %s → %s\n", e.Quality, e.DubKey, e.Source.URL)
		}
		return round{embeds: fresh.RawEmbeds, links: links}
	}

	r1 := collect(1)
	r2 := collect(2)

	sameEmbeds := fmt.Sprint(sortedEmbedKeys(r1.embeds)) == fmt.Sprint(sortedEmbedKeys(r2.embeds)) &&
		fmt.Sprint(r1.embeds) == fmt.Sprint(r2.embeds)
	sameLinks := fmt.Sprint(r1.links) == fmt.Sprint(r2.links)
	switch {
	case !sameEmbeds && !sameLinks:
		t.Logf("VERDICT: ROTATION OBSERVED — round 2 served NEW embeds AND new stream links; " +
			"the PR111 fresh resolve picked them up (the cached-links pipeline would have replayed round 1).")
	case !sameLinks:
		t.Logf("VERDICT: LINK ROTATION OBSERVED — same embeds, but the extraction returned fresh stream links on round 2.")
	case !sameEmbeds:
		t.Logf("VERDICT: EMBED ROTATION OBSERVED — new embed URLs, stream links coincided.")
	default:
		t.Logf("VERDICT: STABLE — this provider currently serves the same links; both fresh rounds succeeded " +
			"(rotation is server-timed; the pipeline re-resolves on every open regardless).")
	}
}
