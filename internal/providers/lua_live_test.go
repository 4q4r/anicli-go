//go:build live

package providers

// The PR116 LIVE verification: the three migrated Lua providers driven
// against the real sites (the fixture pins above prove shape parity
// with the compiled Go implementations; these prove the live wire).
// Run manually:
//
//	go test ./internal/providers/ -tags live -run TestLiveLua -v
//
// Route notes (the 2026-09-25 characterizations): anitokyo answers
// through the configured proxy on the characterization network
// (network.proxy_url), animedia and animevib answer direct. The test
// uses the DEFAULT config network (no proxy) — flip
// ANICLI_LUA_LIVE_PROXY for the proxied route.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/lua"
	"github.com/an0nx/anicli-go/internal/luaproviders"
	"github.com/an0nx/anicli-go/internal/netclient"
)

func liveProvider(t *testing.T, id string) contracts.Provider {
	t.Helper()

	var src string
	for _, s := range luaproviders.Sources() {
		if s.ID == id {
			src = s.Src
			break
		}
	}
	if src == "" {
		t.Fatalf("no bundled lua script %q", id)
	}

	network := config.Default().Network
	// ANICLI_LUA_LIVE_PROXY re-routes the geo-fenced legs (anitokyo
	// answers through the proxy on the characterization network — the
	// hdrezka/anifilm class).
	if proxy := os.Getenv("ANICLI_LUA_LIVE_PROXY"); proxy != "" {
		network.ProxyURL = proxy
	}
	cfg := lua.DefaultConfig()
	cfg.Timeout = 90 * time.Second // the full serial fan-out rides it
	client, err := netclient.New(network, netclient.WithProvider(id))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	cfg.HTTP = client
	p, err := lua.LoadProviderBytes(cfg, nil, id, []byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p.Adapt()
}

func TestLiveLuaProvidersAgainstRealSites(t *testing.T) {
	queries := map[string]string{
		"anitokyo": "дандадан",
		"animedia": "врата штейна",
		"animevib": "дандадан",
		// animevost (PR119): the API answers through the configured
		// proxy (the direct route does not resolve on the
		// characterization network) — run with ANICLI_LUA_LIVE_PROXY.
		"animevost": "naruto",
		"anilib":    "черная лагуна",
		"yummy":     "лагуна",
		// animeheaven (PR126): the latin index answers the shared EN
		// probe (verified live 2026-10-05: 3 «black lagoon» cards).
		"animeheaven": "black lagoon",
	}
	for id, query := range queries {
		t.Run(id, func(t *testing.T) {
			p := liveProvider(t, id)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			results, err := p.Search(ctx, query)
			if err != nil {
				t.Fatalf("Search(%q): %v", query, err)
			}
			if len(results) == 0 {
				t.Fatalf("Search(%q) = 0 results (the live catalog moved?)", query)
			}
			t.Logf("search %q: %d results, first = %q", query, len(results), results[0].Title)

			// Walk the results: a typed not-found is the documented
			// wall for announcement («Анонс») pages — the site renders
			// them playerless (the Go provider's live behavior too;
			// the anitokyo smoke notes: 2 of the 3 Dandadan releases
			// are fully playable).
			var episodes []contracts.Episode
			for _, res := range results {
				eps, err := p.GetEpisodes(ctx, res.URL)
				if err != nil {
					t.Logf("GetEpisodes(%s): typed wall: %v", res.URL, err)
					continue
				}
				if len(eps) > 0 {
					episodes = eps
					break
				}
			}
			if len(episodes) == 0 {
				t.Fatalf("every result walled — the catalog's playable surface moved?")
			}
			t.Logf("episodes: %d (first num %q, dubs %d)", len(episodes), episodes[0].Num, len(episodes[0].RawEmbeds))

			// Resolve the FIRST dub of the first episode: a typed wall
			// (anons pages, airing titles) is an acceptable live
			// outcome only if named — a silent empty never is. A dub
			// carrying an iframeCVH embed takes precedence — it drives
			// the provider's dedicated player chain (yummy), the leg
			// the shared extractor factory does NOT cover.
			var dub, cvhDub string
			for name, links := range episodes[0].RawEmbeds {
				if dub == "" {
					dub = name
				}
				for _, link := range links {
					if strings.Contains(link, "/iframeCVH.html?") {
						cvhDub = name
						break
					}
				}
			}
			if cvhDub != "" {
				dub = cvhDub
			}
			stream, err := p.ResolveStream(ctx, episodes[0], dub)
			if err != nil {
				t.Logf("ResolveStream(%q): typed failure (acceptable if the title is walled): %v", dub, err)
				return
			}
			if len(stream.Links) == 0 {
				t.Fatalf("ResolveStream(%q) = zero links", dub)
			}
			for quality, src := range stream.Links {
				t.Logf("resolved %q: %s → %s (%s)", dub, quality, src.URL, src.Type)
				break
			}
		})
	}
}
