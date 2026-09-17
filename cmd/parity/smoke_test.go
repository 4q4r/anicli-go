package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// smokeDeps builds run() dependencies over the GIVEN fake roster
// (explicit per scenario). A per-provider smoke budget of 0 keeps the
// production default (90s) — the timeout scenario overrides it.
func smokeDeps(t *testing.T, smokeTimeout time.Duration, ps ...contracts.Provider) deps {
	t.Helper()

	return deps{
		buildRegistry: func(config.Settings) (*providers.Registry, error) {
			reg := providers.NewEmptyRegistry()
			for _, p := range ps {
				if err := reg.Register(p); err != nil {
					return nil, err
				}
			}
			return reg, nil
		},
		now:          time.Now,
		saveDir:      t.TempDir(),
		seq:          new(atomic.Uint64),
		smokeTimeout: smokeTimeout,
	}
}

// runSmoke executes `parity smoke ...` and returns the captured
// output, stderr and exit code.
func runSmoke(t *testing.T, d deps, argv ...string) (string, string, int) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(argv, &out, &errOut, d)
	return out.String(), errOut.String(), code
}

// TestSmokeAllPasses: every fake resolves the full chain
// (search → dubs → stream link), the table shows PASS rows with the
// counts and the run exits zero.
func TestSmokeAllPasses(t *testing.T) {
	d := smokeDeps(t, 0,
		newParityProvider(t, "p00", false),
		newParityProvider(t, "p01", false),
	)
	out, errOut, code := runSmoke(t, d, "smoke", "all")
	if code != 0 {
		t.Fatalf("all-PASS smoke must exit 0, got %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "p00") || !strings.Contains(out, "p01") {
		t.Fatalf("table missing PASS rows:\n%s", out)
	}
	if !strings.Contains(out, "smoke PASSED: 2/2") {
		t.Fatalf("summary missing:\n%s", out)
	}
	// The counts column carries the chain (1 search hit, ≥1 dub,
	// ≥1 stream).
	if !strings.Contains(out, "1/1/1") {
		t.Fatalf("counts column missing the search/dubs/streams triple:\n%s", out)
	}
}

// TestSmokeSearchZeroFails: a provider answering zero search results
// is a FAIL row with the reason, and the run exits non-zero.
func TestSmokeSearchZeroFails(t *testing.T) {
	dead := newParityProvider(t, "dead", false)
	dead.searchEmpty = true
	d := smokeDeps(t, 0,
		newParityProvider(t, "alive", false),
		dead,
	)
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("a FAIL row must exit non-zero, stdout:\n%s", out)
	}
	if !strings.Contains(out, "dead") || !strings.Contains(out, "FAIL") {
		t.Fatalf("table missing the FAIL row:\n%s", out)
	}
	if !strings.Contains(out, "search") {
		t.Fatalf("FAIL row must name the failing leg:\n%s", out)
	}
	if !strings.Contains(out, "smoke FAILED: 1/2") {
		t.Fatalf("summary missing:\n%s", out)
	}
}

// TestSmokeNoDubsFails: zero dubs (and no hydrator) fails the chain.
func TestSmokeNoDubsFails(t *testing.T) {
	bare := newParityProvider(t, "bare", false)
	bare.epEmpty = true
	d := smokeDeps(t, 0, bare)
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("no-dubs provider must exit non-zero, stdout:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "dubs") {
		t.Fatalf("table missing the dubs FAIL:\n%s", out)
	}
}

// TestSmokeHydratorLegResolves: a lazily-hydrating provider (empty
// embeds + FetchDubs) passes through the hydrator leg.
func TestSmokeHydratorLegResolves(t *testing.T) {
	lazy := &smokeHydrator{parityProvider: newParityProvider(t, "lazy", false)}
	lazy.epEmpty = true
	d := smokeDeps(t, 0, lazy)
	out, errOut, code := runSmoke(t, d, "smoke", "all")
	if code != 0 {
		t.Fatalf("hydrator leg must complete the chain, exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "lazy") || !strings.Contains(out, "PASS") {
		t.Fatalf("table missing the PASS row:\n%s", out)
	}
}

// TestSmokeHydratorFailureFails: a failing hydration is an honest
// FAIL, never a panic.
func TestSmokeHydratorFailureFails(t *testing.T) {
	lazy := &smokeHydrator{parityProvider: newParityProvider(t, "lazy", false), fail: true}
	lazy.epEmpty = true
	d := smokeDeps(t, 0, lazy)
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("failing hydration must exit non-zero, stdout:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "dubs") {
		t.Fatalf("table missing the dubs FAIL:\n%s", out)
	}
}

// TestSmokeNoStreamsFails: dubs without a single resolvable link fail
// the chain (PASS needs search>0 ∧ dubs≥1 ∧ streams≥1).
func TestSmokeNoStreamsFails(t *testing.T) {
	mute := newParityProvider(t, "mute", false)
	mute.streamEmpty = true
	d := smokeDeps(t, 0, mute)
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("zero-streams provider must exit non-zero, stdout:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "streams") {
		t.Fatalf("table missing the streams FAIL:\n%s", out)
	}
}

// TestSmokeLatinFallbackOnZeroRUHits: a default-preference provider
// answering zero to the RU query gets ONE latin retry (a language
// mismatch is not a dead provider); the row names both queries.
func TestSmokeLatinFallbackOnZeroRUHits(t *testing.T) {
	// ruDeafSearch answers empty for the RU query and defers to the
	// loopback hit otherwise.
	ls := &ruDeafSearch{parityProvider: newParityProvider(t, "latinonly", false)}
	d := smokeDeps(t, 0, ls)
	out, errOut, code := runSmoke(t, d, "smoke", "all")
	if code != 0 {
		t.Fatalf("latin fallback must save a deaf-to-RU provider, exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, smokeQueryRU+"+"+smokeQueryLatin) {
		t.Fatalf("row must show the retried query pair:\n%s", out)
	}
}

// ruDeafSearch answers empty for Cyrillic queries, one hit otherwise.
type ruDeafSearch struct{ *parityProvider }

func (p *ruDeafSearch) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	if strings.ContainsRune(query, []rune(smokeQueryRU)[0]) {
		return nil, nil
	}
	return p.parityProvider.Search(ctx, query)
}

// TestSmokeSkipsCredentialProviders: the credential-gated roster
// members render as SKIP rows with their reason and never affect the
// exit code — including gated ids absent from the registry.
func TestSmokeSkipsCredentialProviders(t *testing.T) {
	d := smokeDeps(t, 0, newParityProvider(t, "alive", false))
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code != 0 {
		t.Fatalf("SKIP rows must not fail the run, stdout:\n%s", out)
	}
	for id := range smokeSkipped {
		if !strings.Contains(out, id) || !strings.Contains(out, "SKIP") {
			t.Fatalf("table missing the SKIP row for %s:\n%s", id, out)
		}
	}
}

// TestSmokeSingleProvider: an explicit provider id runs just that row.
func TestSmokeSingleProvider(t *testing.T) {
	d := smokeDeps(t, 0,
		newParityProvider(t, "p00", false),
		newParityProvider(t, "p01", false),
	)
	out, _, code := runSmoke(t, d, "smoke", "p01")
	if code != 0 {
		t.Fatalf("single smoke must honour the target, stdout:\n%s", out)
	}
	if !strings.Contains(out, "p01") || strings.Contains(out, "p00 ") {
		t.Fatalf("single smoke ran the wrong roster:\n%s", out)
	}
}

// TestSmokeSingleUnknownProvider: an unknown id is a clean error.
func TestSmokeSingleUnknownProvider(t *testing.T) {
	d := smokeDeps(t, 0, newParityProvider(t, "p00", false))
	_, errOut, code := runSmoke(t, d, "smoke", "nope")
	if code == 0 {
		t.Fatal("unknown provider must exit non-zero")
	}
	if !strings.Contains(errOut, "nope") {
		t.Fatalf("stderr must name the unknown provider, got: %s", errOut)
	}
}

// TestSmokePerProviderBudget: one corpse must not hang the run — the
// provider leg is bounded by the per-provider smoke budget and its
// timeout surfaces as an honest FAIL.
func TestSmokePerProviderBudget(t *testing.T) {
	corpse := newParityProvider(t, "corpse", false)
	corpse.epSleep = 5 * time.Second // far beyond the tiny test budget
	d := smokeDeps(t, 50*time.Millisecond,
		newParityProvider(t, "alive", false),
		corpse,
	)
	start := time.Now()
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("timed-out provider must exit non-zero, stdout:\n%s", out)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the per-provider budget must bound the run, took %s", elapsed)
	}
	if !strings.Contains(out, "corpse") || !strings.Contains(out, "FAIL") {
		t.Fatalf("table missing the corpse FAIL row:\n%s", out)
	}
}

// TestSmokeSearchErrorFails: a failing search (transport etc.) is a
// FAIL row naming the leg, not a panic (error paths settle honestly).
func TestSmokeSearchErrorFails(t *testing.T) {
	d := smokeDeps(t, 0, newParityProvider(t, "broken", true))
	out, _, code := runSmoke(t, d, "smoke", "all")
	if code == 0 {
		t.Fatalf("failing search must exit non-zero, stdout:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("table missing the FAIL row:\n%s", out)
	}
}

// Compile-time guard: the smoke suite exercises the hydration
// contract production uses (kept honest against accidental drift).
var _ contracts.DubsHydrator = (*smokeHydrator)(nil)
