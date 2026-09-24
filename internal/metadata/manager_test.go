package metadata

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider is an in-process Provider for ordering/limit tests.
type fakeProvider struct {
	id     string
	titles []string
	err    error
	// delay blocks until the context dies (timeout testing).
	delay bool
	// calls counts SearchAlternativeTitles invocations.
	calls int
	mu    sync.Mutex
}

func (p *fakeProvider) ID() string { return p.id }

func (p *fakeProvider) SearchAlternativeTitles(ctx context.Context, _ string) ([]string, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.delay {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.titles, nil
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// newFakeManager wires a Manager over fake providers with the given
// order and a tight per-provider timeout.
func newFakeManager(t *testing.T, providers []Provider, order []string) *Manager {
	t.Helper()
	m := NewManager(providers, order, nil)
	m.perProviderTimeout = 100 * time.Millisecond
	return m
}

// TestManagerNormalizesOrder pins the python active_order contract:
// configured-known entries (trimmed, lowercased) first in configured
// sequence, then the remaining known providers in canonical order;
// unknown entries are dropped.
func TestManagerNormalizesOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured []string
		want       []string
	}{
		{
			name:       "empty -> canonical default",
			configured: nil,
			want:       []string{"anilist", "kitsu", "anisearch", "anidb"},
		},
		{
			name:       "reordered + unknown dropped",
			configured: []string{"kitsu", "bogus", " ANILIST "},
			want:       []string{"kitsu", "anilist", "anisearch", "anidb"},
		},
		{
			name:       "subset first, rest appended",
			configured: []string{"anidb"},
			want:       []string{"anidb", "anilist", "kitsu", "anisearch"},
		},
		{
			name:       "duplicates collapse",
			configured: []string{"kitsu", "kitsu"},
			want:       []string{"kitsu", "anilist", "anisearch", "anidb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := NewManager(nil, tt.configured, nil)
			if got := m.order(); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestManagerMergesAndDedupes pins the aggregate semantics: aliases from
// every successful provider merge, dedupe on the lowercase key, and any
// alias case-insensitively equal to the query is excluded (python seeds
// the seen-set with the lowercased query).
func TestManagerMergesAndDedupes(t *testing.T) {
	t.Parallel()

	anilist := &fakeProvider{id: "anilist", titles: []string{"NARUTO", "Naru Op"}}
	kitsu := &fakeProvider{id: "kitsu", titles: []string{"naruto", "Shippuden"}}

	m := newFakeManager(t, []Provider{anilist, kitsu},
		[]string{"anilist", "kitsu"})

	got, err := m.SearchAlternativeTitles(context.Background(), "Naruto")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}

	// Every casing of the query itself is excluded; distinct aliases
	// from both providers survive.
	want := []string{"Naru Op", "Shippuden"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("aliases = %v, want %v", got, want)
	}
	// Both providers consulted (aggregate, not first-success-only).
	if anilist.callCount() != 1 || kitsu.callCount() != 1 {
		t.Errorf("calls = anilist:%d kitsu:%d, want 1/1", anilist.callCount(), kitsu.callCount())
	}
}

// TestManagerProviderFailureFallsThrough pins: a failing or timing-out
// provider is skipped and the remaining providers still contribute.
func TestManagerProviderFailureFallsThrough(t *testing.T) {
	t.Parallel()

	hanging := &fakeProvider{id: "anilist", delay: true}
	broken := &fakeProvider{id: "kitsu", err: errors.New("boom")}
	healthy := &fakeProvider{id: "anisearch", titles: []string{"Alias A"}}

	m := newFakeManager(t, []Provider{hanging, broken, healthy},
		[]string{"anilist", "kitsu", "anisearch"})

	start := time.Now()
	got, err := m.SearchAlternativeTitles(context.Background(), "query")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if len(got) != 1 || got[0] != "Alias A" {
		t.Errorf("aliases = %v, want [Alias A]", got)
	}
	// The hanging provider died at its per-provider timeout, not the
	// request timeout.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, per-provider timeout not applied", elapsed)
	}
}

// TestManagerPerProviderLimit pins the per-provider cap of 20 aliases.
func TestManagerPerProviderLimit(t *testing.T) {
	t.Parallel()

	var titles []string
	for i := range 50 {
		titles = append(titles, "Title "+string(rune('A'+i)))
	}
	chatty := &fakeProvider{id: "anilist", titles: titles}

	m := newFakeManager(t, []Provider{chatty}, []string{"anilist"})
	got, err := m.SearchAlternativeTitles(context.Background(), "q")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if n := len(got); n != m.perProviderLimit {
		t.Errorf("aliases = %d, want per-provider limit %d", n, m.perProviderLimit)
	}
}

// TestManagerGlobalLimit pins the global cap of 80 aliases: the four
// providers with disjoint alias pools each contribute their
// per-provider-limited share, totaling exactly the global cap.
func TestManagerGlobalLimit(t *testing.T) {
	t.Parallel()

	ids := []string{"anilist", "kitsu", "anisearch", "anidb"}
	var providers []Provider
	for p, id := range ids {
		var titles []string
		for i := range 60 {
			titles = append(titles,
				string(rune('0'+p))+string(rune('A'+i%26))+string(rune('a'+i)))
		}
		providers = append(providers, &fakeProvider{id: id, titles: titles})
	}

	m := newFakeManager(t, providers, nil)
	got, err := m.SearchAlternativeTitles(context.Background(), "q")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if n := len(got); n != m.globalLimit {
		t.Errorf("aliases = %d, want global limit %d (4 providers x 20)", n, m.globalLimit)
	}
}

// TestManagerEmptyQuery pins: an empty (or whitespace-only) query returns
// an empty result without consulting any provider.
func TestManagerEmptyQuery(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{id: "anilist", titles: []string{"X"}}
	m := newFakeManager(t, []Provider{p}, []string{"anilist"})

	got, err := m.SearchAlternativeTitles(context.Background(), "   ")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("aliases = %v, want empty", got)
	}
	if p.callCount() != 0 {
		t.Errorf("provider calls = %d, want 0", p.callCount())
	}
}

// TestQueryVariants pins the minimal build_query_variants port: original
// title first, then aliases, then lowercase variants, deduplicated and
// capped at MAX_QUERY_VARIANTS=16.
func TestQueryVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		title   string
		aliases []string
		want    []string
	}{
		{
			name:  "title only",
			title: "Naruto",
			want:  []string{"Naruto", "naruto"},
		},
		{
			name:    "aliases follow, exact dupes dropped",
			title:   "Naruto",
			aliases: []string{"Naruto", "NARUTO"},
			want:    []string{"Naruto", "NARUTO", "naruto"},
		},
		{
			name:    "cap at 16",
			title:   "T",
			aliases: []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q", "R"},
			want:    []string{"T", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N", "O"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := QueryVariants(tt.title, tt.aliases)
			if len(got) > maxQueryVariants {
				t.Errorf("variants = %v, exceeds cap %d", got, maxQueryVariants)
			}
			if len(tt.want) <= maxQueryVariants && strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("variants = %v, want %v", got, tt.want)
			}
			if len(got) > 0 && got[0] != tt.title {
				t.Errorf("first variant = %q, want the original title %q", got[0], tt.title)
			}
		})
	}
}

// TestQueryVariantsDedupe pins case-insensitive exact-dedupe: "Naruto"
// and a later lowercase "naruto" collapse when positions collide.
func TestQueryVariantsDedupe(t *testing.T) {
	t.Parallel()

	got := QueryVariants("Foo", []string{"foo", "Bar"})
	want := []string{"Foo", "foo", "Bar", "bar"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("variants = %v, want %v", got, want)
	}
}
