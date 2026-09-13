package regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
)

// expectedProviderOrder is the full roster in registry order. The
// meta-test fails when a provider is silently dropped, renamed,
// duplicated or re-ordered, and when a roster member loses its
// dedicated fixture→DTO shape test.
var expectedProviderOrder = []string{
	"anilibria",
	"animevost",
	"anilib",
	"animego",
	"sovetromantica",
	"gogoanime",
	"animepahe",
	"dreamcast",
	"sameband",
	"kodik",
	"allanime",
}

// TestProviderRosterComplete asserts the registry enumerates exactly
// the eleven providers, unique, in the pinned order (prevents silent
// provider drop — the G1 gate and the parity tool both assume 11).
func TestProviderRosterComplete(t *testing.T) {
	built, err := providers.All(config.Default())
	if err != nil {
		t.Fatalf("providers.All: %v", err)
	}

	got := make([]string, 0, len(built))
	seen := map[string]bool{}
	for _, p := range built {
		id := p.ID()
		if seen[id] {
			t.Fatalf("duplicate provider id %q in registry", id)
		}
		seen[id] = true
		got = append(got, id)
	}

	if len(got) != 11 {
		t.Errorf("registry must hold exactly 11 providers, got %d: %v", len(got), got)
	}
	if len(expectedProviderOrder) != 11 {
		t.Fatalf("expected roster table must list 11 providers, has %d", len(expectedProviderOrder))
	}
	for i, want := range expectedProviderOrder {
		if i >= len(got) {
			t.Fatalf("roster truncated at position %d: want %q, got %v", i, want, got)
		}
		if got[i] != want {
			t.Errorf("roster position %d: want %q, got %q (full: %v)", i, want, got[i], got)
		}
	}
}

// TestProviderShapeTestsExist asserts every roster member keeps its
// dedicated per-provider test file in internal/providers — the place
// where fixture→DTO response-shape stability is pinned. A provider
// without its own test file must not exist.
func TestProviderShapeTestsExist(t *testing.T) {
	for _, id := range expectedProviderOrder {
		path := filepath.Join("..", "providers", id+"_test.go")
		info, err := os.Stat(path) //nolint:gosec // fixed roster-derived name
		if err != nil {
			t.Errorf("provider %q has no dedicated shape test at %s: %v", id, path, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("provider %q shape test %s is empty", id, path)
		}
	}
}

// TestProviderFixtureFilesPresent asserts each roster member has at
// least one captured fixture file in internal/providers/testdata named
// after it (the fixture→DTO pins need raw material).
func TestProviderFixtureFilesPresent(t *testing.T) {
	dir := filepath.Join("..", "providers", "testdata")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read provider testdata: %v", err)
	}
	fixtures := map[string]bool{}
	for _, e := range entries {
		fixtures[strings.SplitN(e.Name(), "_", 2)[0]] = true
	}
	for _, id := range expectedProviderOrder {
		if !fixtures[id] {
			t.Errorf("provider %q has no fixture files under %s (expected prefix %q)", id, dir, id+"_")
		}
	}
}
