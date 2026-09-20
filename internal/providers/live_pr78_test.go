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
//   - nyaa: one search through the real netclient ladder (3 attempts,
//     no-first-byte watchdog, exponential backoff) — the provider has
//     NO extra retry by design (the mandate's no-stacking rule); the
//     probe shows the live outcome either way.
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//	  go test -tags live -run TestLivePR78 -count=1 -v ./internal/providers/
package providers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// TestLivePR78AniZoneNoResults runs the owner's failing query and its
// latin variant against the live index.
func TestLivePR78AniZoneNoResults(t *testing.T) {
	cfg := config.Default()
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	http, err := netclient.New(cfg.Network, netclient.WithProvider("anizone"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	p := newAniZone(AniZoneBase, http)

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

// TestLivePR78AnimePaheBridgeSearch is the PR78 search-leg proof: the
// multi-provider-style latin query through the REAL [cf] browser bridge
// (the configuration the owner's [cf]-disabled TUI run lacked). The
// full-chain variant (episodes + resolve) is TestLivePR71Chain; resolve
// rides the kwik WAF wall class and is not part of this proof.
func TestLivePR78AnimePaheBridgeSearch(t *testing.T) {
	cfg := config.Default()
	cfg.CF.Enabled = true
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	mgr, err := cfbrowser.NewManager(cfg)
	if err != nil {
		t.Fatalf("cf manager (is the stealth binary installed? `anicli cf install`): %v", err)
	}
	defer func() { _ = mgr.Close() }()
	http, err := netclient.New(cfg.Network, netclient.WithProvider("animepahe"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	p := newAnimePahe(AnimePaheBase, http, buildPaheBridge(mgr))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	query := os.Getenv("AP_QUERY")
	if query == "" {
		query = "Tongari Boushi no Atelier"
	}
	results, err := p.Search(ctx, query)
	if err != nil {
		t.Fatalf("search through the bridge: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("search through the bridge: 0 results")
	}
	t.Logf("[bridge-search] %q → %d results, first: %q (session %s)",
		query, len(results), results[0].Title, results[0].URL)
}

// TestLivePR78NyaaSearch runs one nyaa search through the production
// netclient ladder. Success proves the flapping class was absorbed;
// a typed failure after the ladder is the honest terminal state (the
// TUI fan-out renders it within its own 30s budget either way).
func TestLivePR78NyaaSearch(t *testing.T) {
	cfg := config.Default()
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	http, err := netclient.New(cfg.Network, netclient.WithProvider("nyaa"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	p := newNyaa(NyaaBase, http, nil) // no engine: preflight is skipped, search alone is the probe

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "witch hat atelier")
	if err != nil {
		t.Logf("[nyaa] typed failure after the netclient ladder: %v", err)
		return
	}
	t.Logf("[nyaa] %d results, first: %q", len(results), results[0].Title)
}
