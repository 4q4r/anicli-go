//go:build load

package loadtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR81 latency fan-out profile: the full 29-provider roster fans out
// ONE search each at simulated site latencies (50/200/1000ms) using
// the production fan-out shape (goroutine per provider — the
// searchProgress.startFanOut tea.Batch layout). Wall time must equal
// max(latency) + ε per round: the parallelism proof. Also reports
// allocations per fan-out at the slowest round.

const (
	rosterProviders   = 29
	fanoutLatencyEps  = 100 * time.Millisecond // scheduling jitter allowance
	fanoutSettleLimit = 25
)

// latencyProvider sleeps for latency on every Search (the simulated
// site) and returns one synthetic hit.
type latencyProvider struct {
	id      string
	latency time.Duration
}

func (p *latencyProvider) ID() string                       { return p.id }
func (p *latencyProvider) Name() string                     { return p.id }
func (p *latencyProvider) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *latencyProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (p *latencyProvider) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	select {
	case <-time.After(p.latency):
		return []contracts.SearchResult{
			{Title: p.id + " hit for " + query, URL: "https://" + p.id + ".example/a", SourceID: p.id},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *latencyProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return nil, errors.New("not used by the latency fan-out profile")
}

func (p *latencyProvider) ResolveStream(_ context.Context, _ contracts.Episode, _ string) (contracts.MediaStream, error) {
	return contracts.MediaStream{}, errors.New("not used by the latency fan-out profile")
}

// fanOutRosterOnce mirrors searchProgress.startFanOut: one goroutine
// per provider, results collected over a channel, bounded by the
// caller's context.
func fanOutRosterOnce(ctx context.Context, provs []*latencyProvider, query string) []contracts.SearchResult {
	type reply struct {
		results []contracts.SearchResult
	}
	out := make(chan reply, len(provs))
	var wg sync.WaitGroup
	for _, p := range provs {
		wg.Add(1)
		go func(p *latencyProvider) {
			defer wg.Done()
			results, err := p.Search(ctx, query)
			if err == nil {
				out <- reply{results: results}
			}
		}(p)
	}
	wg.Wait()
	close(out)

	all := make([]contracts.SearchResult, 0, len(provs))
	for r := range out {
		all = append(all, r.results...)
	}
	return all
}

// TestLoadSearchFanout29Latencies proves 29-provider fan-out wall time
// tracks the slowest provider, not the sum — at 50/200/1000ms
// simulated latencies.
func TestLoadSearchFanout29Latencies(t *testing.T) {
	ctx := context.Background()

	provs := make([]*latencyProvider, 0, rosterProviders)
	for i := range rosterProviders {
		provs = append(provs, &latencyProvider{id: fmt.Sprintf("prov%02d", i)})
	}

	rounds := []struct {
		latency time.Duration
	}{
		{latency: 50 * time.Millisecond},
		{latency: 200 * time.Millisecond},
		{latency: 1000 * time.Millisecond},
	}

	goroutinesBefore := runtime.NumGoroutine()

	fmt.Fprintf(os.Stdout, "\n=== 29-provider fan-out latency results ===\n")
	fmt.Fprintln(os.Stdout, "latency\twall\toverhead\tresults\talloc_B/op")

	for _, round := range rounds {
		for _, p := range provs {
			p.latency = round.latency
		}

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		start := time.Now()
		results := fanOutRosterOnce(ctx, provs, "van pis")
		wall := time.Since(start)

		runtime.ReadMemStats(&after)

		if len(results) != rosterProviders {
			t.Fatalf("round %s: %d results, want %d", round.latency, len(results), rosterProviders)
		}
		overhead := wall - round.latency
		if overhead > fanoutLatencyEps {
			t.Errorf("round %s: wall %s exceeds max(latency)+ε by %s (> %s) — fan-out is serializing?",
				round.latency, wall, overhead, fanoutLatencyEps)
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t+%s\t%d\t%d\n",
			round.latency, wall.Round(time.Millisecond), overhead.Round(time.Microsecond),
			len(results), after.TotalAlloc-before.TotalAlloc)
	}

	leaked := goroutineDelta(t, goroutinesBefore, 2*time.Second, fanoutSettleLimit)
	if leaked > 10 {
		t.Fatalf("goroutine leak: delta %d after settle", leaked)
	}
	fmt.Fprintf(os.Stdout, "leak delta %d\n\n", leaked)
}
