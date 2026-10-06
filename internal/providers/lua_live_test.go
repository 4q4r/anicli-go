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
	"github.com/an0nx/anicli-go/internal/torrent"
)

// kodik (PR140) deliberately has NO entry in the live queries: the
// kodik-api.com API answers 401 without an owner token and none
// exists, so a live leg is unprovable by design — the fixture suite
// (internal/providers/kodik_test.go) carries the whole proof and the
// script header documents it loudly. Adding a query to the map below
// would be a fabricated live claim.

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
	adapted := p.Adapt()

	// Torrent-declared scripts (rutor, PR142): the search surface is
	// the script's; the engine legs are Go machinery. The production
	// composition is the factory's luaTorrent adapter over a real
	// engine — mirrored here so the walk exercises the same
	// preflight → metadata → resolve chain the registry serves.
	if declared, ok := adapted.(interface{ Torrent() bool }); ok && declared.Torrent() {
		transport, err := netclient.New(network, netclient.WithProvider("torrent"))
		if err != nil {
			t.Fatalf("torrent netclient: %v", err)
		}
		eng := torrent.NewEngine(config.Default().Torrent, transport, nil)
		t.Cleanup(func() { _ = eng.Close() })
		return newLuaTorrent(adapted, transport, eng)
	}
	return adapted
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
		// animemobi (PR137): the declared probe rides the proxy (the
		// route-matrix honest route; the shared RU probe «черная
		// лагуна» misses the catalog — DLE's word-prefix search never
		// matches the inflected site titles). LIVE DRIFT NOTE
		// (2026-10-06): since the PR116 verification matrix the site
		// tarpits the netclient's TLS fingerprint — the search POST
		// times out on BOTH routes (silent connection drop direct,
		// EOF through the proxy; a plain browser-UA curl answers 200
		// with 10 shortstory rows in ~2-3s). The compiled PR92
		// provider failed the smoke the same way on the same day
		// (identical live behavior — the migration-success rule);
		// the subtest fails until the site's transport wall lifts.
		"animemobi": "боруто",
		// anistar (PR138): the cp1251 DLE catalog rides the proxy (the
		// smoke matrix's honest route: the declared «боруто» probe
		// PASSes the full chain — 3 cards surfaced, 2 resolved fully —
		// in ~1.2s through the proxy; the direct route tarpits on the
		// characterization network like the anifilm class).
		"anistar": "боруто",
		// anipub (PR139): the EN catalog rides the proxy (the smoke
		// matrix's honest route: the «cowboy bebop» probe PASSes the
		// full chain — 3 cards surfaced, the megaplay AES decrypt
		// lands — in ~1.8s through the proxy; the shared EN probe
		// misses this Latin-only Name index).
		"anipub": "cowboy bebop",
		// hdrezka (PR141): the rezka family rides the proxy (the smoke
		// matrix's honest route: the «черная лагуна» probe PASSes the
		// full chain through the proxy — the Anubis gate engages on
		// the anime leg and the hybrid's Go solver clears it; the
		// search leg answered the DLE listing in ~0.5s through the
		// proxy, live 2026-10-06).
		"hdrezka": "черная лагуна",
		// rutor (PR142): the proxy is the honest route (the route
		// matrix's note, pre-probed 2026-10-06: the direct route does
		// not even resolve on the characterization network — DNS
		// failure, the anidub class — while the standard per-provider
		// netclient answers the search 200 in ~0.3s through the
		// proxy; the PR87-era uTLS tarpit is gone on the honest
		// route). The torrent walk (preflight → metadata → resolve)
		// rides the adapter + engine composition liveProvider builds
		// for torrent-declared scripts.
		"rutor": "черная лагуна",
		// anirena (PR143): the RSS search rides the proxy (the route
		// matrix's honest route: the «black lagoon» probe surfaced
		// 11/11 anime entries in ~1.6s through the proxy, live
		// 2026-10-06; the JA/multilingual torrent index answers the
		// shared EN probe). Torrent slot: the episodes leg resolves
		// through the Go engine — the same adapter + engine
		// composition the rutor entry rides.
		"anirena": "black lagoon",
		// subsplease (PR144): the whole-catalog JSON API answers on
		// BOTH routes (live 2026-10-06: the three search legs ~0.3s
		// direct and ~1-1.5s through the proxy; the tz=0 parameter is
		// mandatory — without it every /api/ leg answers HTTP 200 with
		// zero bytes, re-verified the same day). The declared "re:zero"
		// probe rides either route; the parity smoke keeps --proxy for
		// the engine-resolve legs (the route matrix's honest route —
		// its 58.9s row is the metadata-resolve chain, not the origin).
		"subsplease": "re:zero",
		// anilibria-torrent (PR145): the proxy is the honest route per
		// the smoke matrix («черная лагуна» → 4 prefix-matched
		// releases expanding to 5 seeded torrents, 5/5 resolved in
		// ~8.8s through the proxy, live 2026-10-06). The subtest's
		// torrent branch below resolves the surface through the REAL
		// engine (metadata+files), not the script stubs — the parity
		// smoke's torrent rule.
		"anilibria-torrent": "черная лагуна",
		// animetosho (PR146): the JSON search API answers DIRECT (the
		// route matrix's honest route — live 2026-10-06: the
		// «black lagoon» probe surfaced 30 bounded records in ~0.4s
		// direct; the preflighted storage.animetosho.org bytes ride
		// the same route). Torrent slot: the episodes leg resolves
		// through the Go engine — the same adapter + engine
		// composition the rutor/anirena/subsplease/anilibria-torrent
		// entries ride.
		"animetosho": "black lagoon",
		// tokyotosho (PR147): the proxy is the honest route per the
		// smoke matrix (the «black lagoon» probe PASSes the torrent
		// chain — 2 of 36 surfaced entries fully resolved to 2/2
		// playable — in ~21.8s through the proxy; the origin is slow
		// and the cross-posted .torrent mirrors ride the preflight's
		// budget timeouts, so the direct route is not the honest
		// one). The latin-only index takes the shared EN probe; the
		// torrent walk below resolves through the REAL engine — the
		// parity smoke's torrent rule.
		"tokyotosho": "black lagoon",
	}
	for id, query := range queries {
		t.Run(id, func(t *testing.T) {
			// Torrent scripts resolve through the REAL engine, not the
			// script stubs: the provider rides the factory's hybrid
			// wrap with a live engine wired (the parity smoke's torrent
			// rule — metadata-ready with files ≥ 1 is the complete
			// torrent resolve; the loopback stream link is the last
			// leg).
			if liveIsTorrentID(t, id) {
				liveTorrentWalk(t, id, query)
				return
			}
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

			// subsplease (PR144) is the torrent-search hybrid: the
			// script owns the SEARCH surface pinned here — every
			// surfaced link must be an engine-ingestable btih magnet —
			// while the metadata/resolve legs ride the Go torrent
			// engine (the parity smoke carries that proof end to end;
			// the sandbox has no engine and must not fake the chain).
			if id == "subsplease" {
				for _, res := range results {
					if !strings.HasPrefix(res.URL, "magnet:?xt=urn:btih:") {
						t.Fatalf("result %q: url %q is not a btih magnet", res.Title, res.URL)
					}
				}
				t.Logf("subsplease: %d btih magnet results surfaced (the engine resolve is the parity smoke's proof)", len(results))
				return
			}

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

// liveIsTorrentID reports whether the bundled script for id declares
// torrent = true (the hybrid roster shape the factory wraps).
func liveIsTorrentID(t *testing.T, id string) bool {
	t.Helper()
	for _, s := range luaproviders.Sources() {
		if s.ID != id {
			continue
		}
		cfg := lua.DefaultConfig()
		p, err := lua.LoadProviderBytes(cfg, nil, id, []byte(s.Src))
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		return p.Torrent()
	}
	t.Fatalf("no bundled lua script %q", id)
	return false
}

// liveTorrentWalk runs the torrent provider against the live site the
// way the parity smoke does: search through the script, then resolve
// EVERY surfaced result through the REAL engine (ingest + bounded
// metadata wait; metadata-ready with files ≥ 1 is the complete torrent
// resolve), and take the loopback stream link of the first. The hybrid
// comes from the same factory path the registry serves, with a live
// engine wired in place of the registry's.
func liveTorrentWalk(t *testing.T, id, query string) {
	t.Helper()

	network := config.Default().Network
	if proxy := os.Getenv("ANICLI_LUA_LIVE_PROXY"); proxy != "" {
		network.ProxyURL = proxy
	}
	cfg := config.Default()
	cfg.Network = network

	built, clients, _, err := luaProviders(cfg, nil, map[string]bool{}, nil)
	if err != nil {
		t.Fatalf("luaProviders: %v", err)
	}
	p, ok := built[id]
	if !ok {
		t.Fatalf("provider %q missing from the factory build", id)
	}
	// The factory wraps torrent-declared scripts in the luaTorrent
	// adapter after luaProviders returns (the luaTorrentServe duck);
	// the walk mirrors that composition so the engine injection below
	// lands on the TorrentBase the resolve legs ride.
	if declared, isTorrent := p.(interface{ Torrent() bool }); isTorrent && declared.Torrent() {
		p = newLuaTorrent(p, clients[id], nil)
	}
	client, err := netclient.New(network, netclient.WithProvider("torrent"))
	if err != nil {
		t.Fatalf("torrent netclient: %v", err)
	}
	engine := torrent.NewEngine(cfg.Torrent, client, nil)
	t.Cleanup(func() { _ = engine.Close() })
	se, ok := p.(interface{ SetEngine(*torrent.Engine) })
	if !ok {
		t.Fatal("the roster provider must accept the engine injection")
	}
	se.SetEngine(engine)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	results, err := p.Search(ctx, query)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	if len(results) == 0 {
		t.Fatalf("Search(%q) = 0 results (the live catalog moved?)", query)
	}
	t.Logf("search %q: %d results, first = %q", query, len(results), results[0].Title)

	// Torrent rule: every surfaced result resolves (their Search
	// filters seedless entries pre-surface, so what surfaces is really
	// seeding — the promise the walk keeps).
	resolved := 0
	for _, res := range results {
		eps, err := p.GetEpisodes(ctx, res.URL)
		if err != nil {
			t.Errorf("GetEpisodes(%s): the engine could not resolve a surfaced result: %v", res.URL, err)
			continue
		}
		if len(eps) == 0 {
			t.Errorf("GetEpisodes(%s) = 0 episodes for a surfaced result", res.URL)
			continue
		}
		resolved++
		if resolved == 1 {
			stream, err := p.ResolveStream(ctx, eps[0], torrentDubLabel)
			if err != nil {
				t.Logf("ResolveStream: typed failure: %v", err)
			} else if len(stream.Links) == 0 {
				t.Error("ResolveStream = zero links")
			} else {
				t.Logf("resolved %d files; first stream link rides the loopback server", len(eps))
			}
		}
	}
	if resolved != len(results) {
		t.Fatalf("resolved %d/%d surfaced results — the torrent rule needs the whole surface", resolved, len(results))
	}
	t.Logf("torrent surface fully resolved: %d/%d", resolved, len(results))
}
