package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// stubProvider is a contracts.Provider test double used to exercise the
// registry and the search-stat delegator without touching the network.
type stubProvider struct {
	id      string
	results []contracts.SearchResult
	err     error

	gotQuery string
}

func (s *stubProvider) ID() string                       { return s.id }
func (s *stubProvider) Name() string                     { return "Stub " + s.id }
func (s *stubProvider) BaseURL() string                  { return "https://stub.example" }
func (s *stubProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (s *stubProvider) Search(_ context.Context, query string) ([]contracts.SearchResult, error) {
	s.gotQuery = query
	return s.results, s.err
}

func (s *stubProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return nil, nil
}

func (s *stubProvider) ResolveStream(_ context.Context, _ contracts.Episode, _ string) (contracts.MediaStream, error) {
	return contracts.MediaStream{}, nil
}

func TestRegistryRegisterListGet(t *testing.T) {
	t.Parallel()

	reg := NewEmptyRegistry()
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("empty registry List() = %d providers, want 0", len(got))
	}

	first := &stubProvider{id: "alpha"}
	second := &stubProvider{id: "beta"}
	if err := reg.Register(first); err != nil {
		t.Fatalf("Register(first): %v", err)
	}
	if err := reg.Register(second); err != nil {
		t.Fatalf("Register(second): %v", err)
	}

	got := reg.List()
	if len(got) != 2 {
		t.Fatalf("List() = %d providers, want 2", len(got))
	}
	// Registration order must be preserved (priority rendering relies on it).
	if got[0].ID() != "alpha" || got[1].ID() != "beta" {
		t.Errorf("List() order = [%s, %s], want [alpha, beta]", got[0].ID(), got[1].ID())
	}

	p, ok := reg.Get("beta")
	if !ok || p.ID() != "beta" {
		t.Errorf("Get(beta) = (%v, %v)", p, ok)
	}
	if _, ok := reg.Get("missing"); ok {
		t.Error("Get(missing) must not be found")
	}
}

func TestRegistryRegisterDuplicateIDFails(t *testing.T) {
	t.Parallel()

	reg := NewEmptyRegistry()
	if err := reg.Register(&stubProvider{id: "dup"}); err != nil {
		t.Fatalf("Register(first): %v", err)
	}
	err := reg.Register(&stubProvider{id: "dup"})
	if err == nil {
		t.Fatal("Register with duplicate ID must fail")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("duplicate Register error = %v, want ErrInvalidInput", err)
	}
}

func TestSearchDelegatorRecordsSuccess(t *testing.T) {
	t.Parallel()

	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	defer func() { _ = st.Close() }()

	want := []contracts.SearchResult{{Title: "T", URL: "u", SourceID: "stub"}}
	inner := &stubProvider{id: "stub", results: want}
	del := SearchDelegator{Provider: inner, stats: st.ProviderStats}

	got, err := del.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Title != "T" {
		t.Errorf("Search results = %+v, want the inner provider's results", got)
	}
	if inner.gotQuery != "naruto" {
		t.Errorf("inner provider query = %q, want %q", inner.gotQuery, "naruto")
	}

	stats, err := st.ProviderStats.TopProviders(context.Background(), 0)
	if err != nil {
		t.Fatalf("TopProviders: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("stat rows = %d, want 1", len(stats))
	}
	if stats[0].ProviderID != "stub" || stats[0].Successes != 1 || stats[0].Failures != 0 {
		t.Errorf("stat row = %+v, want stub with 1 success 0 failures", stats[0])
	}
}

func TestSearchDelegatorRecordsFailure(t *testing.T) {
	t.Parallel()

	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	defer func() { _ = st.Close() }()

	inner := &stubProvider{id: "stub", err: errors.New("boom")}
	del := SearchDelegator{Provider: inner, stats: st.ProviderStats}

	if _, err := del.Search(context.Background(), "q"); err == nil {
		t.Fatal("Search must propagate the inner error")
	}

	stats, err := st.ProviderStats.TopProviders(context.Background(), 0)
	if err != nil {
		t.Fatalf("TopProviders: %v", err)
	}
	if len(stats) != 1 || stats[0].Failures != 1 || stats[0].Successes != 0 {
		t.Fatalf("stat rows = %+v, want stub with 1 failure 0 successes", stats)
	}
}

func TestSearchDelegatorNilStatsIsNoop(t *testing.T) {
	t.Parallel()

	inner := &stubProvider{id: "stub", results: []contracts.SearchResult{{Title: "ok"}}}
	del := SearchDelegator{Provider: inner}

	got, err := del.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search with nil stats: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Search results = %+v", got)
	}
}

func TestSearchDelegatorWrapsBareError(t *testing.T) {
	t.Parallel()

	del := SearchDelegator{Provider: &stubProvider{id: "stub", err: errors.New("boom")}}

	_, err := del.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search must fail")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error %v is not a *contracts.ProviderError", err)
	}
	if perr.Provider != "stub" || perr.Op != contracts.OpSearch {
		t.Errorf("ProviderError = %+v, want provider stub op search", perr)
	}
}

func TestSearchDelegatorPassesThroughProviderErrors(t *testing.T) {
	t.Parallel()

	innerErr := contracts.WrapProvider("stub", contracts.OpSearch, 403, contracts.ErrProvider403)
	del := SearchDelegator{Provider: &stubProvider{id: "stub", err: innerErr}}

	_, err := del.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error %v must keep ErrProvider403", err)
	}
	// The original error object must be passed through unchanged (no
	// double wrapping).
	if !errors.Is(err, innerErr) {
		t.Fatalf("error %v must be the original ProviderError instance", err)
	}
}
