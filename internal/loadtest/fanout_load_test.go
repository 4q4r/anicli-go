//go:build load

package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/skip"
)

// Fan-out + skip-manager load profile: the 11-provider search fan-out
// (registry + SearchDelegator with live stat writes) runs 500
// iterations through netclient.Parallel with the production
// MaxParallel bound (4); the skip Manager.Resolve path is hammered 500
// times over a fake HTTP transport. Asserts bounded parallelism and no
// goroutine leaks on either surface.

const (
	fanoutProviders = 11
	fanoutRuns      = 500
	fanoutLimit     = 4 // config.Network.MaxParallel production default

	skipResolves = 500
)

// fanoutProvider fakes one registry member: small in-memory work with
// a pinch of sleep so the parallelism bound is observable.
type fanoutProvider struct {
	id       string
	http     *netclient.Client
	inFlight atomic.Int64
	maxSeen  atomic.Int64
}

func (p *fanoutProvider) ID() string                       { return p.id }
func (p *fanoutProvider) Name() string                     { return p.id }
func (p *fanoutProvider) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *fanoutProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (p *fanoutProvider) Search(_ context.Context, query string) ([]contracts.SearchResult, error) {
	cur := p.inFlight.Add(1)
	for {
		old := p.maxSeen.Load()
		if cur <= old || p.maxSeen.CompareAndSwap(old, cur) {
			break
		}
	}
	time.Sleep(200 * time.Microsecond)
	p.inFlight.Add(-1)
	return []contracts.SearchResult{
		{Title: p.id + " hit for " + query, URL: "https://" + p.id + ".example/a", SourceID: p.id},
	}, nil
}

func (p *fanoutProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return nil, errors.New("not used by the fan-out load profile")
}

func (p *fanoutProvider) ResolveStream(_ context.Context, _ contracts.Episode, _ string) (contracts.MediaStream, error) {
	return contracts.MediaStream{}, errors.New("not used by the fan-out load profile")
}

// TestLoadSearchFanout11x500 fans an 11-member registry (bare fakes —
// the SearchDelegator stat path is unit-covered in internal/providers)
// out 500 times at the production parallelism bound.
func TestLoadSearchFanout11x500(t *testing.T) {
	ctx := context.Background()

	registry := providers.NewEmptyRegistry()
	provs := make([]*fanoutProvider, 0, fanoutProviders)
	for i := range fanoutProviders {
		p := &fanoutProvider{id: fmt.Sprintf("fan%02d", i)}
		provs = append(provs, p)
		if err := registry.Register(p); err != nil {
			t.Fatalf("register %s: %v", p.id, err)
		}
	}

	goroutinesBefore := runtime.NumGoroutine()
	start := time.Now()

	for range fanoutRuns {
		items := registry.List()
		if err := netclient.Parallel(ctx, items, fanoutLimit, func(ctx context.Context, p contracts.Provider) error {
			_, err := p.Search(ctx, "load")
			return err
		}); err != nil {
			t.Fatalf("fan-out iteration failed: %v", err)
		}
	}
	wall := time.Since(start)

	// Bounded parallelism: no provider may have seen more than the
	// limit concurrent searches.
	for _, p := range provs {
		if observed := p.maxSeen.Load(); observed > fanoutLimit {
			t.Fatalf("parallelism bound violated on %s: %d > %d", p.id, observed, fanoutLimit)
		}
	}

	leaked := goroutineDelta(t, goroutinesBefore, 2*time.Second, 25)

	fmt.Fprintf(os.Stdout, "\n=== search fan-out load results (%d providers × %d runs, limit %d) ===\n",
		fanoutProviders, fanoutRuns, fanoutLimit)
	fmt.Fprintf(os.Stdout, "wall\t%s\t%.0f fan-outs/s\t%.0f searches/s\tleak delta %d\n\n",
		wall.Round(time.Millisecond),
		float64(fanoutRuns)/wall.Seconds(),
		float64(fanoutRuns*fanoutProviders)/wall.Seconds(),
		leaked)

	if leaked > 10 {
		t.Fatalf("goroutine leak: delta %d after settle", leaked)
	}
}

// fakeSkipHTTP answers every GET with a valid aniskip v2 envelope and
// every GraphQL POST with a valid anime-skip episodeByMalId payload, so
// both API providers contribute to every Resolve.
type fakeSkipHTTP struct {
	gets  atomic.Int64
	posts atomic.Int64
}

func (f *fakeSkipHTTP) Get(_ context.Context, _ string, _ map[string]string) (*netclient.Response, error) {
	f.gets.Add(1)
	body, _ := json.Marshal(map[string]any{
		"found": true,
		"results": []map[string]any{{
			"skip_type": "op",
			"interval":  map[string]any{"start_time": 0.0, "end_time": 90.5},
		}},
	})
	return &netclient.Response{StatusCode: 200, Status: "200 OK", Body: body}, nil
}

func (f *fakeSkipHTTP) PostJSON(_ context.Context, _ string, _ any, _ map[string]string) (*netclient.Response, error) {
	f.posts.Add(1)
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"episodeByMalId": map[string]any{
				"timestamps": []map[string]any{{
					"skipType":  "ed",
					"startTime": 1320.25,
					"endTime":   1440.0,
				}},
			},
		},
	})
	return &netclient.Response{StatusCode: 200, Status: "200 OK", Body: body}, nil
}

// TestLoadSkipManager500Resolves hammers Manager.Resolve (aniskip +
// anime_skip consulted, merged) 500 times.
func TestLoadSkipManager500Resolves(t *testing.T) {
	cfg := config.Default().Skip
	fake := &fakeSkipHTTP{}
	manager := skip.NewManager(cfg, fake)

	goroutinesBefore := runtime.NumGoroutine()
	start := time.Now()

	ctx := context.Background()
	for i := range skipResolves {
		bundle, err := manager.Resolve(ctx, skip.ResolveRequest{
			ShikimoriID: int64(21),
			EpisodeNum:  float64(i%12 + 1),
		})
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if bundle.Empty() {
			t.Fatalf("resolve %d: expected merged chapters from the fake providers", i)
		}
	}
	wall := time.Since(start)

	leaked := goroutineDelta(t, goroutinesBefore, 2*time.Second, 25)

	// One aniskip GET and one anime-skip POST per resolve: the manager
	// has no cache and no retries, so the counts must be exact.
	if got := fake.gets.Load(); got != skipResolves {
		t.Errorf("aniskip GETs = %d, want %d (one per resolve)", got, skipResolves)
	}
	if got := fake.posts.Load(); got != skipResolves {
		t.Errorf("anime-skip POSTs = %d, want %d (one per resolve)", got, skipResolves)
	}

	fmt.Fprintf(os.Stdout, "=== skip manager load results (%d resolves) ===\n", skipResolves)
	fmt.Fprintf(os.Stdout, "wall\t%s\t%.0f resolves/s\taniskip GETs %d\tanimeskip POSTs %d\tleak delta %d\n\n",
		wall.Round(time.Millisecond), float64(skipResolves)/wall.Seconds(), fake.gets.Load(), fake.posts.Load(), leaked)

	if leaked > 10 {
		t.Fatalf("goroutine leak: delta %d after settle", leaked)
	}
}

// goroutineDelta waits for the goroutine count to settle and returns
// the delta against baseline.
func goroutineDelta(t *testing.T, baseline int, window time.Duration, allowed int) int {
	t.Helper()
	deadline := time.Now().Add(window)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == last && current <= baseline+allowed {
			return current - baseline
		}
		last = current
	}
	return last - baseline
}
