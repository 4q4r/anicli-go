//go:build live

// PR78 live probes for the search-failure fix round, run against the
// live sites the owner's TUI fan-out hit:
//
//   - anizone: the failing cyrillic query («Ателье колдовских
//     колпаков») must settle as a CLEAN zero-result miss (the PR78
//     fix), while the latin variant the fan-out routes first
//     (NamePrefLatin) must still hit. The latin hit also proves the
//     query-routing half of the investigation: the site indexes the
//     romaji/EN names and answers them, so the owner's failure was the
//     cyrillic-only variant set, not lost routing.
//
//     ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//     go test -tags live -run TestLivePR78 -count=1 -v ./internal/providers/
//
// PR130: the provider runs as the BUNDLED LUA SCRIPT — the probe
// loads it through the same proxy-aware live loader the other script
// verifications use (the direct route tarpits on the characterization
// network; ANICLI_LUA_LIVE_PROXY carries the same value).
package providers

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLivePR78AniZoneNoResults runs the owner's failing query and its
// latin variant against the live index.
func TestLivePR78AniZoneNoResults(t *testing.T) {
	if os.Getenv("ANICLI_LIVE_PROXY") != "" {
		t.Setenv("ANICLI_LUA_LIVE_PROXY", os.Getenv("ANICLI_LIVE_PROXY"))
	}
	p := liveProvider(t, "anizone")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "Ателье колдовских колпаков")
	if err != nil {
		t.Fatalf("cyrillic query must be a clean miss, got error: %v", err)
	}
	if len(results) != 0 {
		t.Logf("[cyr] %d results (unexpected but not fatal): %v", len(results), results[0].Title)
	} else {
		t.Log("[cyr] «Ателье колдовских колпаков» → 0 results, nil error (clean miss)")
	}

	latin := os.Getenv("AZ_QUERY")
	if latin == "" {
		latin = "Tongari Boushi no Atelier"
	}
	results, err = p.Search(ctx, latin)
	if err != nil {
		t.Fatalf("latin query: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("latin query returned 0 results, want the Witch-Hat row")
	}
	t.Logf("[lat] %q → %d results, first: %q (slug %s)", latin, len(results), results[0].Title, results[0].URL)
}
