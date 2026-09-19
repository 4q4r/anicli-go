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
// dedicated fixture→DTO shape test. sovetromantica was removed in PR22
// (domain hijacked off the anime project, frozen 2025); anidub (no
// frozen Python original) joined the roster the same PR; nyaa (PR36,
// first torrent search provider) joined after anidub; anilibria-torrent
// (PR37, the new aniliberty.top API's per-release torrents) joined
// after nyaa; animetosho (PR38, the newznab feed) joined after
// anilibria-torrent; tokyotosho (PR38, the search RSS) joined after
// animetosho.
// Wave-2 integration (fix/60) seated the five parallel providers
// next to their peers instead of the tail: kickassanime (PR58)
// and anizone (PR59, sub-only) joined after animepahe in the
// latin block; animedia (PR56, the amd.online DLE site), shiza
// (PR57, the shizaproject.com GraphQL catalog) and anime365
// (PR55, the tokened smotret-anime JSON API) joined the RU-dub
// block; yanima left it in PR65 (dead Mitelis wall, roster pinned
// 21→20); yummy (PR68, the YummyAnime api.yani.tv JSON API) joined
// after anime365 closing the RU-dub block. The roster is frozen at
// 21.
var expectedProviderOrder = []string{
	"anilibria",
	"animevost",
	"anilib",
	"animego",
	"gogoanime",
	"animepahe",
	"kickassanime",
	"anizone",
	"dreamcast",
	"sameband",
	"kodik",
	"allanime",
	"anidub",
	"animedia",
	"shiza",
	"anime365",
	"yummy",
	"nyaa",
	"anilibria-torrent",
	"animetosho",
	"tokyotosho",
}

// TestProviderRosterComplete asserts the registry enumerates exactly
// the roster, unique, in the pinned order (prevents silent
// provider drop — the G1 gate and the parity tool both assume the
// roster size).
func TestProviderRosterComplete(t *testing.T) {
	// The full roster needs every credentialled provider configured
	// (PR24/PR55: a tokenless kodik or anime365 is disabled at
	// startup and dropped from the registry).
	cfg := config.Default()
	cfg.Providers.Kodik.Token = "test-token"
	cfg.Providers.Anime365.Token = "test-token" // keep anime365 in the roster (PR55)
	built, err := providers.All(cfg)
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

	if len(got) != len(expectedProviderOrder) {
		t.Errorf("registry must hold exactly %d providers, got %d: %v",
			len(expectedProviderOrder), len(got), got)
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
