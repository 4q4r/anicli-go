// Command parity is the live-site parity capture tool: it runs REAL
// provider operations (search, episodes, resolve) through the real
// registry and config, prints JSON, and saves captures under
// testdata/parity/ for diffing against the frozen Python behaviour.
//
// This tool is meant to be run by a human/controller against live
// sites; it is excluded from the normal zero-network test discipline.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// env is the resolved per-command world: the provider registry plus
// the effective per-operation timeout (single config load per command).
type env struct {
	reg     *providers.Registry
	timeout time.Duration
	// route records the effective network route of the run ("direct"
	// or "proxy") — the smoke table notes it per row.
	route string
}

// close releases the registry's shared resources (the CF bypass stack
// when [cf] is enabled; a no-op otherwise).
func (e *env) close() {
	if e.reg != nil {
		_ = e.reg.Close()
	}
}

// gateProviders is the G1 gate threshold: `parity all` exits non-zero
// when fewer than this many providers answer both probe queries. It
// tracks the roster minus one dead-provider tolerance (11 of 12 at
// PR24's roster; 12 of 13 since nyaa joined in PR36; 13 of 14 since
// anilibria-torrent joined in PR37; 16 of 17 since animedia joined in
// PR56; integration re-pin: 17 of 18 with anime365+animedia both in; 18 of 19 since shiza joined in PR57; 19 of 20 since kickassanime joined in PR58; 20 of 21 since anizone joined in PR59; 21 of 22 since yummy joined in PR68).
const gateProviders = 21

// probeQueries are the two queries every provider must answer in
// `parity all`.
var probeQueries = []string{"test", "naruto"}

// deps carries the injectable seams: registry construction (tests
// substitute fakes), the clock (stable capture timestamps in tests),
// the capture save directory, the capture sequence counter and the
// smoke suite's per-provider budget (0 keeps the production default).
type deps struct {
	buildRegistry func(config.Settings) (*providers.Registry, error)
	now           func() time.Time
	saveDir       string
	// seq disambiguates capture filenames inside one process run:
	// second-granularity timestamps alone collide on back-to-back
	// captures. Shared by pointer across the deps value copies handed
	// to the subcommands; both constructors initialize it.
	seq *atomic.Uint64
	// smokeTimeout bounds ONE provider's whole smoke chain
	// (search → dubs → stream); 0 means smokeProviderTimeout.
	smokeTimeout time.Duration
}

func realDeps() deps {
	return deps{
		buildRegistry: func(cfg config.Settings) (*providers.Registry, error) {
			// The parity CLI has no alt-screen: provider diagnostics
			// (preflight drops) stay on the console sink (PR62 #4 —
			// the never-stderr rule is a TUI rule).
			return providers.NewRegistry(cfg, nil,
				providers.WithProviderLogger(slog.Default()))
		},
		now:     time.Now,
		saveDir: filepath.Join("testdata", "parity"),
		seq:     new(atomic.Uint64),
	}
}

// run executes the CLI and returns the process exit code. Errors are
// printed by cobra (SilenceUsage keeps usage noise off failure paths).
func run(args []string, out, errOut io.Writer, d deps) int {
	root := &cobra.Command{
		Use:   "parity",
		Short: "Live provider parity capture tool (search/episodes/resolve/all)",
		Long: "parity runs real provider operations against live sites via the real\n" +
			"registry+config, prints JSON captures and saves them under testdata/parity/.\n" +
			"`parity all` is the permanent G1 gate: all roster providers must answer\n" +
			"(up to one dead provider tolerated).",
		SilenceUsage: true,
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)

	var (
		cfgPath string
		proxy   string
		timeout time.Duration
	)
	root.PersistentFlags().StringVar(&cfgPath, "config", "",
		"path to settings.toml (default: $ANICLI_CONFIG > XDG > ~/.config/anicli/settings.toml)")
	root.PersistentFlags().StringVar(&proxy, "proxy", "",
		"proxy URL overriding network.proxy_url (default: config value; explicit empty = direct)")
	root.PersistentFlags().DurationVar(&timeout, "timeout", 0,
		"per-operation timeout (default: network.request_timeout, 30s)")

	// load resolves settings + the effective per-operation timeout.
	load := func(cmd *cobra.Command) (config.Settings, time.Duration, error) {
		settings, err := config.Load(config.ResolveConfigPath(cfgPath))
		if err != nil {
			return config.Settings{}, 0, fmt.Errorf("load settings: %w", err)
		}
		if cmd.Flags().Changed("proxy") {
			settings.Network.ProxyURL = proxy
		}
		effective := timeout
		if effective <= 0 {
			effective = settings.Network.RequestTimeout
		}
		return *settings, effective, nil
	}

	// env is the resolved per-command world: the provider registry
	// plus the effective per-operation timeout (one config load).
	setup := func(cmd *cobra.Command) (*env, error) {
		settings, timeout, err := load(cmd)
		if err != nil {
			return nil, err
		}
		reg, err := d.buildRegistry(settings)
		if err != nil {
			return nil, fmt.Errorf("build provider registry: %w", err)
		}
		route := "direct"
		if settings.Network.ProxyURL != "" {
			route = "proxy"
		}
		return &env{reg: reg, timeout: timeout, route: route}, nil
	}

	root.AddCommand(
		paritySearchCommand(d, setup),
		parityEpisodesCommand(d, setup),
		parityResolveCommand(d, setup),
		parityAllCommand(setup),
		paritySmokeCommand(d, setup),
	)

	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

// provider fetches one provider by id or fails with a clear message.
func provider(reg *providers.Registry, id string) (contracts.Provider, error) {
	p, ok := reg.Get(id)
	if !ok {
		known := make([]string, 0, len(reg.List()))
		for _, item := range reg.List() {
			known = append(known, item.ID())
		}
		return nil, fmt.Errorf("unknown provider %q (registered: %s)", id, strings.Join(known, ", "))
	}
	return p, nil
}

// capture is the saved/printed envelope of one operation.
type capture struct {
	Tool       string `json:"tool"`
	Op         string `json:"op"`
	Provider   string `json:"provider"`
	TookMS     int64  `json:"took_ms"`
	CapturedAt string `json:"captured_at"`

	Query    string                   `json:"query,omitempty"`
	URL      string                   `json:"url,omitempty"`
	Dub      string                   `json:"dub,omitempty"`
	Episode  string                   `json:"episode_num,omitempty"`
	Count    int                      `json:"count"`
	Results  []contracts.SearchResult `json:"results,omitempty"`
	Episodes []contracts.Episode      `json:"episodes,omitempty"`
	Stream   *contracts.MediaStream   `json:"stream,omitempty"`
}

// emit prints the capture as indented JSON and saves it under saveDir.
func emit(out io.Writer, d deps, cap capture) error {
	cap.Tool = "parity"
	cap.CapturedAt = d.now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode capture: %w", err)
	}

	if _, err := fmt.Fprintln(out, string(data)); err != nil {
		return err
	}

	// The -%04d sequence guards against second-granularity filename
	// collisions on back-to-back captures.
	name := fmt.Sprintf("%s-%s-%s-%04d.json", cap.Provider, cap.Op,
		d.now().UTC().Format("20060102T150405Z"), d.seq.Add(1))
	if err := os.MkdirAll(d.saveDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", d.saveDir, err)
	}
	path := filepath.Join(d.saveDir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil { //nolint:gosec // capture artifact
		return fmt.Errorf("write capture %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "saved: %s\n", path)
	return nil
}

// paritySearchCommand builds `parity search <provider> "<query>"`.
func paritySearchCommand(d deps, setup func(*cobra.Command) (*env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "search <provider> <query>",
		Short: "Run a real search against the live provider",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()
			p, err := provider(env.reg, args[0])
			if err != nil {
				return err
			}
			timeout := env.timeout

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			start := time.Now()
			results, err := p.Search(ctx, args[1])
			if err != nil {
				return fmt.Errorf("search %s %q: %w", args[0], args[1], err)
			}
			return emit(cmd.OutOrStdout(), d, capture{
				Op: "search", Provider: args[0], Query: args[1],
				Count: len(results), Results: results,
				TookMS: time.Since(start).Milliseconds(),
			})
		},
	}
}

// parityEpisodesCommand builds `parity episodes <provider> <url>`.
func parityEpisodesCommand(d deps, setup func(*cobra.Command) (*env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "episodes <provider> <url>",
		Short: "List episodes of a live anime URL",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()
			p, err := provider(env.reg, args[0])
			if err != nil {
				return err
			}
			timeout := env.timeout

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			start := time.Now()
			episodes, err := p.GetEpisodes(ctx, args[1])
			if err != nil {
				return fmt.Errorf("episodes %s %s: %w", args[0], args[1], err)
			}
			return emit(cmd.OutOrStdout(), d, capture{
				Op: "episodes", Provider: args[0], URL: args[1],
				Count: len(episodes), Episodes: episodes,
				TookMS: time.Since(start).Milliseconds(),
			})
		},
	}
}

// parityResolveCommand builds `parity resolve <provider> <url> <dub>`:
// episodes are fetched, the FIRST episode is resolved under the given
// dub (a video key of the episode, e.g. "1080"). Resolve triggers a
// real stream fetch — that is the point.
func parityResolveCommand(d deps, setup func(*cobra.Command) (*env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <provider> <url> <dub>",
		Short: "Resolve the first episode's stream for a dub (live)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()
			p, err := provider(env.reg, args[0])
			if err != nil {
				return err
			}
			timeout := env.timeout

			// Episodes and resolve each get the FULL per-operation
			// budget: a slow episodes fetch must not starve the
			// resolve that follows it.
			epCtx, cancelEp := context.WithTimeout(cmd.Context(), timeout)
			episodes, err := p.GetEpisodes(epCtx, args[1])
			cancelEp()
			if err != nil {
				return fmt.Errorf("episodes %s %s: %w", args[0], args[1], err)
			}
			if len(episodes) == 0 {
				return fmt.Errorf("resolve %s %s: provider returned no episodes", args[0], args[1])
			}
			episode := episodes[0]

			resCtx, cancelRes := context.WithTimeout(cmd.Context(), timeout)
			defer cancelRes()
			start := time.Now()
			stream, err := p.ResolveStream(resCtx, episode, args[2])
			if err != nil {
				return fmt.Errorf("resolve %s ep %s dub %q: %w", args[0], episode.Num, args[2], err)
			}
			return emit(cmd.OutOrStdout(), d, capture{
				Op: "resolve", Provider: args[0], URL: args[1], Dub: args[2],
				Episode: episode.Num, Count: len(stream.Links), Stream: &stream,
				TookMS: time.Since(start).Milliseconds(),
			})
		},
	}
}

// allRow is one provider's probe outcome for the summary table.
type allRow struct {
	id   string
	ok   bool
	took time.Duration
	hits string
	err  string
}

// parityAllCommand builds `parity all`: every registered provider is
// probed with both gate queries (or its own declared probe, PR51)
// under a per-operation timeout; the summary table is printed and the
// command fails when fewer than gateProviders providers answered.
func parityAllCommand(setup func(*cobra.Command) (*env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "Probe all providers (G1 gate): search 'test' + 'naruto' everywhere (declared own probes win)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := setup(cmd)
			if err != nil {
				return err
			}
			defer env.close()
			timeout := env.timeout
			out := cmd.OutOrStdout()

			rows := make([]allRow, 0, len(env.reg.List()))
			for _, p := range env.reg.List() {
				row := allRow{id: p.ID()}
				start := time.Now()
				row.ok = true
				// A provider declaring its own probe (PR51) is probed
				// with it instead of the shared queries: a RU-only
				// index (amd.online) answers 0 to «test»/«naruto» by
				// design and would permanently burn the gate's
				// one-dead tolerance. The wrapper layers peel the same
				// way the smoke peels them.
				queries := probeQueries
				capability := p
				if d, ok := capability.(providers.SearchDelegator); ok {
					capability = d.Provider
				}
				if sq, ok := capability.(contracts.SmokeQueryProvider); ok {
					if declared := sq.SmokeQuery(); declared != "" {
						queries = []string{declared}
					}
				}
				for _, query := range queries {
					ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
					results, err := p.Search(ctx, query)
					cancel()
					if err != nil {
						row.ok = false
						row.err = fmt.Sprintf("%s: %v", query, shorten(err.Error(), 60))
						break
					}
					row.hits += fmt.Sprintf("%s:%d ", query, len(results))
				}
				row.took = time.Since(start)
				row.hits = strings.TrimSpace(row.hits)
				rows = append(rows, row)
			}

			okCount := 0
			for _, row := range rows {
				if row.ok {
					okCount++
				}
			}

			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "provider\tstatus\ttime\tqueries\tfailure")
			for _, row := range rows {
				status, failure := "OK", ""
				if !row.ok {
					status, failure = "FAIL", row.err
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					row.id, status, row.took.Round(time.Millisecond), row.hits, failure)
			}
			if err := w.Flush(); err != nil {
				return err
			}

			if okCount < gateProviders {
				_, _ = fmt.Fprintf(out, "gate FAILED: %d/%d providers OK (need >= %d)\n",
					okCount, len(rows), gateProviders)
				return fmt.Errorf("parity gate FAILED: %d/%d providers OK (need >= %d)",
					okCount, len(rows), gateProviders)
			}
			_, _ = fmt.Fprintf(out, "gate PASSED: %d/%d providers OK\n", okCount, len(rows))
			return nil
		},
	}
}

// shorten truncates long strings for the summary table.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
