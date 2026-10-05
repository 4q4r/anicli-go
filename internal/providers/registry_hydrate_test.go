package providers

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// hydratingStub implements contracts.DubsHydrator over a base provider.
type hydratingStub struct {
	contracts.Provider
	calls int
}

func (h *hydratingStub) FetchDubs(_ context.Context, ep *contracts.Episode) (*contracts.Episode, error) {
	h.calls++
	ep.RawEmbeds["Stub Dub"] = []string{"stub-link"}
	return ep, nil
}

// plainStub is a provider WITHOUT the hydration capability.
type plainStub struct{ contracts.Provider }

// TestRegistryForwardsDubsHydrator pins the PR43 root-cause fix: the
// registry's wrapper stack (SearchDelegator over dubFilteredProvider)
// must expose the lazy-dub hydration capability of the inner provider
// — before the fix BOTH wrappers hid contracts.DubsHydrator and the
// session could never hydrate anilib/animego episodes («Ист: 0»).
// The stack is exercised over STUBS: live providers must never be
// called in tests.
func TestRegistryForwardsDubsHydrator(t *testing.T) {
	stub := &hydratingStub{}
	var wrapped contracts.Provider = SearchDelegator{Provider: dubFilteredProvider{Provider: stub}}

	h, ok := wrapped.(contracts.DubsHydrator)
	if !ok {
		t.Fatal("the wrapper stack hides contracts.DubsHydrator")
	}
	ep := contracts.Episode{Num: "1", RawID: "x", RawEmbeds: map[string][]string{}}
	out, err := h.FetchDubs(context.Background(), &ep)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("hydration calls = %d, want 1", stub.calls)
	}
	if out.RawEmbeds["Stub Dub"][0] != "stub-link" {
		t.Fatalf("hydration result lost: %v", out.RawEmbeds)
	}

	// A provider WITHOUT the capability resolves to the wrapper no-op:
	// the episode returns unchanged instead of hiding the interface.
	plain := contracts.Provider(SearchDelegator{Provider: dubFilteredProvider{Provider: &plainStub{}}})
	noop, ok := plain.(contracts.DubsHydrator)
	if !ok {
		t.Fatal("the wrapper stack must still expose the interface for no-op providers")
	}
	untouched := contracts.Episode{Num: "2", RawEmbeds: map[string][]string{}}
	if _, err := noop.FetchDubs(context.Background(), &untouched); err != nil {
		t.Fatalf("no-op hydration must not fail: %v", err)
	}
	if len(untouched.RawEmbeds) != 0 {
		t.Fatalf("no-op hydration must not add embeds: %v", untouched.RawEmbeds)
	}
}

// TestLiveRegistryExposesHydrationCapability builds the REAL registry
// (construction only — no network calls) and asserts the wrapper stack
// of the lazily-hydrating providers answers the capability assertion.
// PR122: anilib is served by the bundled Lua script — its hydration
// runs eagerly inside episodes() (the release-dub-keys model) and its
// resolve self-hydrates server-side, so the Go DubsHydrator surface
// no longer applies to it (the registry's no-op fallback covers the
// «Обновить источники» recovery; see contracts/provider.go).
func TestLiveRegistryExposesHydrationCapability(t *testing.T) {
	cfg := config.Default()
	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	for _, id := range []string{"animego", "gogoanime"} {
		p, ok := reg.Get(id)
		if !ok {
			t.Fatalf("provider %q missing", id)
		}
		if _, ok := p.(contracts.DubsHydrator); !ok {
			t.Fatalf("provider %q does not expose contracts.DubsHydrator through the wrapper stack", id)
		}
	}
}
