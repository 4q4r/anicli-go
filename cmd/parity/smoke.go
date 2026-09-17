// Live smoke suite (PR44): for every registered provider EXCEPT the
// credential-gated ones, run the full consumption chain against the
// live site — search a known-broad title (per the provider's
// NamePreference), take the first result, resolve the dubs of an
// episode, then resolve ONE stream link. A provider PASSES only when
// search > 0 AND dubs ≥ 1 AND ≥ 1 stream link came back; anything
// else is a FAIL row with the reason (dead providers are the desired
// visibility — never excluded, never special-cased).
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
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// smokeProviderTimeout is the default per-provider budget of the
// whole chain, so one corpse cannot hang the run.
const smokeProviderTimeout = 90 * time.Second

// The smoke probe title: a release KNOWN to exist broadly. Latin for
// the NamePrefLatin feeds, Russian for everyone else (the anicli
// default query language).
const (
	smokeQueryLatin = "black lagoon"
	smokeQueryRU    = "черная лагуна"
)

// smokeSkipped is the directive's exclusion roster: credential-gated
// or unimplemented providers render as SKIP rows (visible, exit-code
// neutral) whether or not the config managed to register them.
var smokeSkipped = map[string]string{
	"kodik":     "credential-gated (API token)",
	"yanima":    "credential-gated (DDoS cookies)",
	"rutracker": "not implemented",
}

// smokeResult is one provider's row of the damage table.
type smokeResult struct {
	id      string
	status  string // PASS / FAIL / SKIP
	query   string
	search  int
	dubs    int
	streams int
	took    time.Duration
	route   string
	reason  string
}

// paritySmokeCommand builds `parity smoke [provider|all]`.
func paritySmokeCommand(d deps, setup func(*cobra.Command) (*env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "smoke [provider|all]",
		Short: "Live smoke chain (search → dubs → stream) per provider; all = every non-credential provider",
		Long: "smoke runs the full consumption chain against live sites per provider: search a\n" +
			"known-broad title (NamePreference-routed), take the first result, resolve an\n" +
			"episode's dubs (through the DubsHydrator capability when the listing is lazy)\n" +
			"and resolve one stream link. PASS needs search>0 AND dubs>=1 AND streams>=1.\n" +
			"Credential-gated providers (kodik, yanima) and the unimplemented rutracker\n" +
			"are skipped with a visible reason. Exits non-zero when any provider FAILs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()

			target := args[0]
			var results []smokeResult
			switch target {
			case "all":
				seen := map[string]bool{}
				for _, p := range env.reg.List() {
					seen[p.ID()] = true
					if reason, skip := smokeSkipped[p.ID()]; skip {
						results = append(results, smokeResult{id: p.ID(), status: "SKIP", route: env.route, reason: reason})
						continue
					}
					results = append(results, smokeOne(cmd.Context(), d, env, p))
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
				results = append(results, smokeOne(cmd.Context(), d, env, p))
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
}

// smokeOne runs one provider's full chain under the per-provider
// budget. Failures settle as FAIL rows — never panics.
func smokeOne(parent context.Context, d deps, env *env, p contracts.Provider) smokeResult {
	budget := d.smokeTimeout
	if budget <= 0 {
		budget = smokeProviderTimeout
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	res := smokeResult{id: p.ID(), route: env.route}
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
	results, err := p.Search(ctx, query)
	if err != nil {
		return fail("search: %s", shorten(err.Error(), 80))
	}
	res.query = query
	res.search = len(results)
	// A default-preference provider deaf to the RU query gets ONE
	// latin retry: a language mismatch is not a dead provider. The row
	// names both queries so the fallback stays visible.
	if res.search == 0 && pref != contracts.NamePrefLatin {
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

	first := results[0]
	episodes, err := p.GetEpisodes(ctx, first.URL)
	if err != nil {
		return fail("episodes: %s", shorten(err.Error(), 80))
	}
	if len(episodes) == 0 {
		return fail("episodes: 0 for %q", first.URL)
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
	res.dubs = len(ep.RawEmbeds)
	if res.dubs == 0 {
		return fail("dubs: 0 after hydration")
	}

	dub := firstSortedKey(ep.RawEmbeds)
	stream, err := p.ResolveStream(ctx, ep, dub)
	if err != nil {
		return fail("resolve: %s", shorten(err.Error(), 80))
	}
	for _, src := range stream.Links {
		if src.URL != "" {
			res.streams++
		}
	}
	if res.streams == 0 {
		return fail("streams: 0 for dub %q", dub)
	}

	res.status = "PASS"
	return res
}

// printSmokeTable renders the damage table: provider / status / route
// / query / counts (search/dubs/streams) / duration / reason.
func printSmokeTable(out io.Writer, results []smokeResult) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "provider\tstatus\troute\tquery\tsearch/dubs/streams\ttime\tfailure")
	for _, r := range results {
		counts := fmt.Sprintf("%d/%d/%d", r.search, r.dubs, r.streams)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.id, r.status, r.route, r.query, counts, r.took.Round(time.Millisecond), r.reason)
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
