//go:build load

package loadtest

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// PR81 episode-hydration profile: the One Piece 1178-episode roster
// hydrated through the production loader pattern — netclient.Parallel
// over per-episode dub hydration (the contracts.DubsHydrator/FetchDubs
// shape) at the config bound (4). Asserts bounded parallelism, zero
// errors, no goroutine leak; reports wall time and bytes allocated.

const (
	hydrationEpisodes = 1178
	hydrationLimit    = 4 // config.Network.MaxParallel production default
	hydrationLatency  = 25 * time.Millisecond
	// With bound L and latency D the expected wall is ⌈N/L⌉·D ≈ 7.4s;
	// the budget catches a fall back to fully-serial hydration
	// (N·D ≈ 30s) — the bound itself is asserted exactly above.
	hydrationMaxWanted = 9 * time.Second
)

// TestLoadEpisodeHydration1178 hydrates the 1178-episode roster with
// per-episode FetchDubs fan-out at the production parallelism bound.
func TestLoadEpisodeHydration1178(t *testing.T) {
	ctx := context.Background()

	var inFlight, maxSeen atomic.Int64
	hydrate := func(_ context.Context, ep contracts.Episode) error {
		cur := inFlight.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(hydrationLatency)
		ep.RawEmbeds["Stub Dub"] = []string{"https://stub.example/" + ep.Num}
		return nil
	}

	// One Piece roster: reverse-descending numbers like gogoanime
	// (1178..1), the shape the session merge consumes.
	eps := make([]contracts.Episode, 0, hydrationEpisodes)
	for n := hydrationEpisodes; n >= 1; n-- {
		eps = append(eps, contracts.Episode{
			Num:       fmt.Sprint(n),
			Title:     fmt.Sprintf("Серия %d", n),
			RawID:     fmt.Sprintf("ep-%d", n),
			RawEmbeds: map[string][]string{},
		})
	}

	goroutinesBefore := runtime.NumGoroutine()
	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()
	if err := netclient.Parallel(ctx, eps, hydrationLimit, hydrate); err != nil {
		t.Fatalf("hydration fan-out: %v", err)
	}
	wall := time.Since(start)

	runtime.ReadMemStats(&memAfter)
	leaked := goroutineDelta(t, goroutinesBefore, 2*time.Second, 25)

	if observed := maxSeen.Load(); observed > hydrationLimit {
		t.Fatalf("parallelism bound violated: %d concurrent hydrations > %d", observed, hydrationLimit)
	}
	if wall > hydrationMaxWanted {
		t.Errorf("hydration wall %s > %s budget — loader serializing?", wall, hydrationMaxWanted)
	}

	fmt.Fprintf(os.Stdout, "\n=== episode hydration load results (%d episodes, bound %d, %s/ep) ===\n",
		hydrationEpisodes, hydrationLimit, hydrationLatency)
	fmt.Fprintf(os.Stdout, "wall\t%s\tepisodes/s\t%.0f\talloc\t%d MB\tobserved max parallel\t%d\tleak delta %d\n\n",
		wall.Round(time.Millisecond),
		float64(hydrationEpisodes)/wall.Seconds(),
		(memAfter.TotalAlloc-memBefore.TotalAlloc)/(1<<20),
		maxSeen.Load(), leaked)
}
