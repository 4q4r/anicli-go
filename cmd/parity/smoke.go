// Live smoke suite (PR44, review-corrected; PR54 kind-aware pass
// rule): for every registered provider EXCEPT the credential-gated
// ones, run the full consumption chain against the live site — search
// a known-broad title (per the provider's NamePreference), then
// resolve EVERY surfaced result bounded-concurrent under the
// per-provider budget: stream providers need dubs ≥ 1 AND ≥ 1 stream
// link per result; torrent providers need metadata-ready with files
// ≥ 1 (their Search filters seedless entries, so what surfaces is
// really seeding). PASS is kind-aware (PR54 owner ruling): STREAM
// providers pass when search>0 AND at least ONE surfaced result
// resolved fully — the surfaced/resolved column keeps the honest N/M
// and the failure column a sample reason; TORRENT providers still
// need EVERY surfaced result resolved (their Search filters dead
// hosts pre-surface). Anything else is a FAIL row; dead providers are
// the desired visibility — never excluded, never special-cased.
//
// The same core backs `parity smoke [provider|all]` and the
// //go:build live test file (smoke_live_test.go); the default
// `go test ./...` stays network-free.

package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// smokeProviderTimeout is the default per-provider budget of the
// whole chain, so one corpse cannot hang the run.
const smokeProviderTimeout = 90 * time.Second

// smokeResolveConcurrency bounds the parallel per-result resolution
// of one provider's surfaced results (the same bounded-pool pattern
// the TUI's hydration used): the fan stays polite to the site while
// the whole surface resolves inside the per-provider budget. A
// surface that still exceeds the budget is an honest FAIL naming the
// progress.
const smokeResolveConcurrency = 8

// The smoke probe title: a release KNOWN to exist broadly. Latin for
// the NamePrefLatin feeds, Russian for everyone else (the anicli
// default query language).
const (
	smokeQueryLatin = "black lagoon"
	smokeQueryRU    = "черная лагуна"
)

// smokeSkipped is the directive's exclusion roster: credential-gated
// providers render as SKIP rows (visible, exit-code neutral) whether or
// not the config managed to register them.
var smokeSkipped = map[string]string{
	"kodik": "credential-gated (API token)",
}

// smokeResult is one provider's row of the damage table.
type smokeResult struct {
	id       string
	status   string // PASS / FAIL / SKIP
	query    string
	search   int
	surfaced int
	resolved int
	dubs     int
	streams  int
	took     time.Duration
	route    string
	reason   string
}

// paritySmokeCommand builds `parity smoke [provider|all]`.
func paritySmokeCommand(d deps, setup func(*cobra.Command) (*env, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "smoke [provider|all]",
		Short: "Live smoke chain (search → dubs → stream) per provider; all = every non-credential provider",
		Long: "smoke runs the full consumption chain against live sites per provider: search a\n" +
			"known-broad title (NamePreference-routed), then resolve EVERY surfaced result\n" +
			"bounded-concurrent under the per-provider budget — stream providers need\n" +
			"dubs>=1 AND >=1 stream link per result; torrent providers need\n" +
			"metadata-ready with files>=1 (their Search filters seedless entries). PASS is\n" +
			"kind-aware: STREAM providers pass when search>0 AND at least ONE surfaced\n" +
			"result resolved fully (the surfaced/resolved column keeps the honest N/M and\n" +
			"the failure column a sample reason); TORRENT providers still need EVERY\n" +
			"surfaced result resolved. PASS needs search>0 either way; a budget\n" +
			"exhaustion is an honest FAIL naming the resolved/surfaced progress.\n" +
			"Credential-gated providers (kodik) are skipped with a visible\n" +
			"reason. Exits non-zero when any provider FAILs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The per-provider budget override (--smoke-timeout): the
			// 90s default predates browser-transport providers — a
			// browser-heavy chain is a dozen navigations, and a
			// budget meant for plain HTTP starves it mid-leg (PR71).
			if cmd.Flags().Changed("smoke-timeout") {
				v, _ := cmd.Flags().GetDuration("smoke-timeout")
				d.smokeTimeout = v
			}
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()

			target := args[0]
			var results []smokeResult
			// The torrent roster once (wrapper layers peeled by the
			// registry): torrent results resolve by metadata+files,
			// not by stream links.
			torrentIDs := map[string]bool{}
			for _, id := range env.reg.TorrentProviderIDs() {
				torrentIDs[id] = true
			}
			switch target {
			case "all":
				seen := map[string]bool{}
				for _, p := range env.reg.List() {
					seen[p.ID()] = true
					if reason, skip := smokeSkipped[p.ID()]; skip {
						results = append(results, smokeResult{id: p.ID(), status: "SKIP", route: env.route, reason: reason})
						continue
					}
					results = append(results, smokeOne(cmd.Context(), d, env, p, torrentIDs))
				}
				// The static gated roster stays visible even when the
				// config did not register it (unconfigured credentials).
				for id, reason := range smokeSkipped {
					if !seen[id] {
						results = append(results, smokeResult{id: id, status: "SKIP", route: env.route, reason: reason})
					}
				}
			default:
				if reason, skip := smokeSkipped[target]; skip {
					results = append(results, smokeResult{id: target, status: "SKIP", route: env.route, reason: reason})
					printSmokeTable(cmd.OutOrStdout(), results)
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "smoke SKIPPED: %s (%s)\n", target, reason)
					return nil
				}
				p, err := provider(env.reg, target)
				if err != nil {
					return err
				}
				results = append(results, smokeOne(cmd.Context(), d, env, p, torrentIDs))
			}

			printSmokeTable(cmd.OutOrStdout(), results)

			fails := 0
			for _, r := range results {
				if r.status == "FAIL" {
					fails++
				}
			}
			tested := len(results) - countStatus(results, "SKIP")
			if fails > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "smoke FAILED: %d/%d providers failed\n", fails, tested)
				return fmt.Errorf("smoke FAILED: %d/%d providers failed", fails, tested)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "smoke PASSED: %d/%d providers OK\n", tested, tested)
			return nil
		},
	}
	cmd.Flags().Duration("smoke-timeout", 0,
		"per-provider smoke budget override (default: 90s; a browser-heavy chain needs more)")
	return cmd
}

// smokeOne runs one provider's smoke: search (NamePreference-routed),
// then resolve EVERY surfaced result — the whole (provider-filtered)
// list — bounded-concurrent under the per-provider budget: stream
// providers need dubs≥1 AND streams≥1 per result; torrent providers
// need metadata-ready with files≥1. Budget exhaustion before the
// surface completes is an honest FAIL naming the progress. Failures
// settle as FAIL rows — never panics. The named return lets the
// timing defer see the final value.
func smokeOne(parent context.Context, d deps, env *env, p contracts.Provider, torrentIDs map[string]bool) (res smokeResult) {
	budget := d.smokeTimeout
	if budget <= 0 {
		budget = smokeProviderTimeout
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	res = smokeResult{id: p.ID(), route: env.route}
	start := time.Now()
	defer func() { res.took = time.Since(start) }()

	fail := func(format string, args ...any) smokeResult {
		res.status = "FAIL"
		res.reason = fmt.Sprintf(format, args...)
		return res
	}

	pref := env.reg.NamePreference(p.ID())
	query := smokeQueryRU
	if pref == contracts.NamePrefLatin {
		query = smokeQueryLatin
	}
	// Own-catalog providers (strict prefix search over the team's own
	// dubs) can miss both shared probes: a declared smoke query wins
	// and gets no RU/latin fallback (PR51). The stat delegator hides
	// optional capabilities, so peel it for the check; the dub stream
	// filter (only present when [providers].exclude_streams is set)
	// hides them too — those configs keep the shared probes.
	capability := p
	if d, ok := capability.(providers.SearchDelegator); ok {
		capability = d.Provider
	}
	declared := ""
	if sq, ok := capability.(contracts.SmokeQueryProvider); ok {
		declared = sq.SmokeQuery()
	}
	if declared != "" {
		query = declared
	}
	results, err := p.Search(ctx, query)
	if err != nil {
		return fail("search: %s", shorten(err.Error(), 80))
	}
	res.query = query
	res.search = len(results)
	// A default-preference provider deaf to the RU query gets ONE
	// latin retry: a language mismatch is not a dead provider. The row
	// names both queries so the fallback stays visible. Declaring
	// providers speak for themselves — no fallback.
	if res.search == 0 && declared == "" && pref != contracts.NamePrefLatin {
		results, err = p.Search(ctx, smokeQueryLatin)
		if err != nil {
			return fail("search: %s", shorten(err.Error(), 80))
		}
		res.query = query + "+" + smokeQueryLatin
		res.search = len(results)
	}
	if res.search == 0 {
		return fail("search: 0 results for %q", res.query)
	}

	// Resolve the WHOLE surfaced surface, bounded-concurrent. The
	// first failure is kept as the row's reason; a budget expiry
	// mid-surface surfaces as that leg's deadline error, and the
	// progress fields show how far the run got.
	res.surfaced = len(results)
	legs := make([]smokeLeg, len(results))
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, smokeResolveConcurrency)
	)
	for i, r := range results {
		wg.Add(1)
		go func(i int, r contracts.SearchResult) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			legs[i] = smokeResolveResult(ctx, p, torrentIDs[p.ID()], r)
		}(i, r)
	}
	wg.Wait()

	minDubs, minStreams := -1, -1
	firstFailure := ""
	for i, leg := range legs {
		if !leg.ok {
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("result %d/%d: %s", i+1, len(legs), leg.reason)
			}
			continue
		}
		res.resolved++
		if minDubs < 0 || leg.dubs < minDubs {
			minDubs = leg.dubs
		}
		if minStreams < 0 || leg.streams < minStreams {
			minStreams = leg.streams
		}
	}
	// Pass rule (PR54 owner ruling), kind-aware:
	//   stream  — search>0 ∧ ≥1 surfaced result fully resolved (the
	//             catalog decides how much of the surface is alive);
	//   torrent — ALL surfaced results resolved (their Search filters
	//             dead pre-surface, so a surviving result is a promise
	//             the whole surface must keep; metadata budget
	//             semantics unchanged).
	// Per-result failures stay visible either way: the
	// surfaced/resolved column carries N/M and the failure column a
	// sample reason.
	pass := res.resolved > 0
	if torrentIDs[p.ID()] {
		pass = res.resolved == res.surfaced
	}
	if !pass {
		if minDubs < 0 {
			minDubs, minStreams = 0, 0
		}
		res.dubs, res.streams = minDubs, minStreams
		return fail("resolved %d/%d: %s", res.resolved, res.surfaced, firstFailure)
	}
	res.dubs, res.streams = minDubs, minStreams
	res.status = "PASS"
	return res
}

// smokeLeg is one surfaced result's resolution outcome.
type smokeLeg struct {
	ok      bool
	reason  string
	dubs    int
	streams int
}

// smokeResolveResult resolves ONE surfaced result: torrent providers
// resolve by metadata-ready + files≥1 (the engine ingested the link
// and the release carries files — the really-seeding property the
// provider-level filter promises); stream providers need dubs≥1 (the
// DubsHydrator capability fills lazy listings) and ≥1 stream link.
func smokeResolveResult(ctx context.Context, p contracts.Provider, torrent bool, r contracts.SearchResult) smokeLeg {
	fail := func(format string, args ...any) smokeLeg {
		return smokeLeg{reason: fmt.Sprintf(format, args...)}
	}

	episodes, err := p.GetEpisodes(ctx, r.URL)
	if err != nil {
		return fail("episodes: %s", shorten(err.Error(), 80))
	}
	if len(episodes) == 0 {
		return fail("episodes: 0 for %q", r.URL)
	}
	if torrent {
		// Metadata arrived and the release carries files — the torrent
		// resolve is complete here (dubs: the single «Торрент» slot,
		// streams: the file count).
		return smokeLeg{ok: true, dubs: 1, streams: len(episodes)}
	}

	ep := episodes[0]
	if len(ep.RawEmbeds) == 0 {
		hydrator, ok := p.(contracts.DubsHydrator)
		if !ok {
			return fail("dubs: 0 and no DubsHydrator")
		}
		out, err := hydrator.FetchDubs(ctx, &ep)
		if err != nil {
			return fail("dubs: hydration: %s", shorten(err.Error(), 80))
		}
		if out != nil {
			ep = *out
		}
	}
	dubs := len(ep.RawEmbeds)
	if dubs == 0 {
		return fail("dubs: 0 after hydration")
	}

	dub := firstSortedKey(ep.RawEmbeds)
	stream, err := p.ResolveStream(ctx, ep, dub)
	if err != nil {
		return fail("resolve: %s", shorten(err.Error(), 80))
	}
	streams := 0
	for _, src := range stream.Links {
		if src.URL != "" {
			streams++
		}
	}
	if streams == 0 {
		return fail("streams: 0 for dub %q", dub)
	}
	return smokeLeg{ok: true, dubs: dubs, streams: streams}
}

// printSmokeTable renders the damage table: provider / status / route
// / query / counts (search/dubs/streams) / surfaced-resolved progress
// / duration / reason.
func printSmokeTable(out io.Writer, results []smokeResult) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "provider\tstatus\troute\tquery\tsearch/dubs/streams\tsurfaced/resolved\ttime\tfailure")
	for _, r := range results {
		counts := fmt.Sprintf("%d/%d/%d", r.search, r.dubs, r.streams)
		progress := ""
		if r.surfaced > 0 {
			progress = fmt.Sprintf("%d/%d", r.surfaced, r.resolved)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.id, r.status, r.route, r.query, counts, progress, r.took.Round(time.Millisecond), r.reason)
	}
	_ = w.Flush()
}

// countStatus totals the rows carrying status.
func countStatus(results []smokeResult, status string) int {
	n := 0
	for _, r := range results {
		if r.status == status {
			n++
		}
	}
	return n
}

// firstSortedKey returns the lexically first key of the embed map
// (deterministic dub pick for the resolve leg).
func firstSortedKey(m map[string][]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// Registry shape guard: the smoke suite runs the same wrapped
// providers the TUI consumes.
var _ = providers.NewEmptyRegistry
