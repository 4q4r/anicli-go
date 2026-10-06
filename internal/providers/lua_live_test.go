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
		// animego (PR124): the HTML surface answers direct on the
		// characterization network (the smoke matrix's honest route).
		"animego": "черная лагуна",
		// shiza (PR125): the GraphQL catalog answers direct from RU
		// networks (no Cloudflare challenge, the 2026-09-18 and
		// 2026-10-05 characterizations) — the default no-proxy route
		// is its honest one.
		"shiza": "черная лагуна",
		// animeheaven (PR126): the latin index answers the shared EN
		// probe (verified live 2026-10-05: 3 «black lagoon» cards).
		"animeheaven": "black lagoon",
		// anikoto (PR127): the EN catalog rides the proxy too (the
		// direct route tarpits on the characterization network — the
		// compiled provider's route note verbatim).
		"anikoto": "black lagoon",
		// kickassanime (PR129): the kaa.lt JSON API tarpits the direct
		// route's episode fan-out on the characterization network (the
		// search answers, the episode pages stall — live 2026-10-06),
		// so the honest route is the proxy; run with
		// ANICLI_LUA_LIVE_PROXY.
		"kickassanime": "dandadan",
		// gogoanime (PR128): the declared "one piece" probe rides the
		// proxy (the honest route per the characterization network —
		// the animevost/anikoto pattern; the parity smoke command
		// passes --proxy for the same reason).
		"gogoanime": "one piece",
		// anizone (PR130): the Livewire catalog rides the proxy (the
		// direct route tarpits on the characterization network — the
		// route-matrix honest note; the latin index answers the shared
		// EN probe).
		"anizone": "black lagoon",
		// sameband (PR131): the DLE POST search answers direct (the
		// smoke matrix's honest route: 676ms for the «дьявол» probe;
		// LIVE-VERIFIED 2026-09-18: HTTP 200, no Cloudflare challenge,
		// no Referer needed).
		"sameband": "дьявол",
		// anidub (PR132): the proxy is the honest route — the true
		// no-proxy route cannot even resolve online.anidub.com on the
		// characterization network (live 2026-10-06, DNS failure;
		// the animevost/anikoto/gogoanime class). Through the proxy
		// the DLE legs answer (story=naruto → 13 cards, HTTP 200;
		// the anime page after its short-slug 301). The resolve leg
		// walls at the shared sibnet extractor: video.sibnet.ru
		// answers HTTP 403 on every route (site-side drift, the
		// PR116 verification matrix) — the typed resolve failure the
		// walk logs, not a port defect.
		"anidub": "naruto",
		// anikado (PR133): the DLE POST search answers direct (the
		// route matrix's honest route — re-verified 2026-10-06:
		// «черная лагуна» → 2 cards in 0.73s, every leg anonymous on
		// the direct route).
		"anikado": "черная лагуна",
		// animiku (PR134): the DLE GET search answers direct (the
		// smoke matrix's honest route: the «черная лагуна» probe
		// surfaced 4 rows direct, LIVE-RE-VERIFIED 2026-10-06 at
		// ~0.7s/leg, no challenge, no Referer needed).
		"animiku": "черная лагуна",
		// anifilm (PR135): the Yii/Vue catalog rides the proxy (the
		// smoke matrix's honest route: the «дьявол» probe PASSes the
		// full chain — 4 cards surfaced, the kodik resolve lands — in
		// ~1.6s through the proxy; the hdrezka-class geo-fence per the
		// route notes above).
		"anifilm": "дьявол",
		// anipub (PR139): the EN catalog rides the proxy (the smoke
		// matrix's honest route: the «cowboy bebop» probe PASSes the
		// full chain — 3 cards surfaced, the megaplay AES decrypt
		// lands — in ~1.8s through the proxy; the shared EN probe
		// misses this Latin-only Name index).
		"anipub": "cowboy bebop",
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
