package providers

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/lua"
	"github.com/an0nx/anicli-go/internal/luaproviders"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// The Lua-script test harness (PR116): the migrated providers load
// their BUNDLED script and point its production base_url literal at
// the fixture server — the exact same fixture captures the compiled
// Go providers were pinned against, through the same
// contracts.Provider surface consumers use.

// luaProductionBases maps each bundled script to the production
// base_url literals it pins (the harness rewrites exactly these
// literals; expectations keep the production domain because the
// fixture pages carry it). Multi-entry lists cover scripts with more
// than one fetched host — yummy's API base AND its CDNVideoHub player
// API base both route to the fixture server.
var luaProductionBases = map[string][]string{
	"anilibria": {"https://aniliberty.top"},
	"anitokyo":  {"https://anitokyo.tv"},
	"animedia":  {"https://amd.online"},
	"animevib":  {"https://www.animevib.ru"},
	"anilib":    {"https://api.cdnlibs.org/api"},
	// animevost (PR119): the JSON API root — the literal must stay the
	// script's FIRST occurrence of the domain (the harness rewrites
	// exactly this one).
	"animevost": {"https://api.animevost.org/v1"},
	"yummy":     {"https://api.yani.tv", "https://plapi.cdnvideohub.com"},
	// animego (PR124): the site root — the literal must stay the
	// script's FIRST occurrence of the domain (the harness rewrites
	// exactly this one; the Referer header derives from the base_url
	// local, never a second literal).
	"animego": {"https://animego.me"},
	// shiza (PR125): the site root — the GraphQL endpoint rides the
	// same host (the Nuxt PUBLIC_API_URL), the release-page URLs too.
	"shiza": {"https://shizaproject.com"},
	// animeheaven (PR126): the site root — every leg (fastsearch,
	// anime.php, gate.php) hangs off the one base_url literal, and the
	// Referer derives from it.
	"animeheaven": {"https://animeheaven.me"},
	// anikoto (PR127): the catalog root only. The megaplay embed origin
	// is never a literal — the script takes the embed URL from the
	// stream resolver's answer (the fixtures rewrite it to the test
	// server at serve time).
	"anikoto": {"https://anikototv.to"},
	// kickassanime (PR129): the site root — every leg (fsearch, show,
	// episodes, servers) hangs off the one base_url literal. The
	// krussdomi HLS edge and its Referer are constructed constants, not
	// fetched hosts.
	"kickassanime": {"https://kaa.lt"},
	// gogoanime (PR128): the site root — the admin-ajax search endpoint,
	// every series page and every episode page hang off the one
	// base_url literal; the episode hrefs inside the fixture pages are
	// rewritten to the test server at serve time (the harness rewrites
	// exactly this one literal).
	"gogoanime": {"https://anitaku.io"},
	// anizone (PR130): the site root — every leg (search, the series
	// page, /livewire/update, the watch page) hangs off the one
	// base_url literal, and the Referer/Origin headers derive from it.
	"anizone": {"https://anizone.to"},
	// sameband (PR131): the site root — every leg (the DLE search POST,
	// the anime page iframe, the Playerjs player page and the playlist)
	// hangs off the one base_url literal, and the stream Referer
	// derives from it.
	"sameband": {"https://sameband.studio"},
	// anifilm (PR135): the site root — every leg (the GET-form search,
	// the release page, the api:online playlist and the api:video page)
	// hangs off the one base_url literal (the site fronts no anti-bot
	// wall, so no Referer/header set is derived).
	"anifilm": {"https://anifilm.pro"},
}

// luaStateJSON builds the {n, u} state JSON the migrated scripts
// encode into raw_id (the fresh-sandbox streams() state carrier).
func luaStateJSON(pageURL, num string) (string, error) {
	b, err := json.Marshal(map[string]string{"n": num, "u": pageURL})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// luaProvider loads the bundled script for id with every pinned
// production base literal pointed at testURL.
func luaProvider(t testing.TB, id, testURL string) contracts.Provider {
	t.Helper()
	return luaProviderFull(t, id, testURL, config.Default().Network, nil)
}

// luaProviderWithNet is luaProvider with a caller-supplied network
// config (the transport-failure tests need the short-timeout ladder).
func luaProviderWithNet(t testing.TB, id, testURL string, ncfg config.Network) contracts.Provider {
	t.Helper()
	return luaProviderFull(t, id, testURL, ncfg, nil)
}

// luaProviderWithLogger loads the bundled script with an explicit
// engine logger (the construction-time injection the registry threads
// for production providers — the Lua equivalent of the Base seam).
func luaProviderWithLogger(t testing.TB, id, testURL string, log *slog.Logger) contracts.Provider {
	t.Helper()
	return luaProviderFull(t, id, testURL, config.Default().Network, log)
}

// luaProviderFull is the shared harness core: the production base
// literal rewrite plus a production-shaped per-provider transport and
// engine logger.
func luaProviderFull(t testing.TB, id, testURL string, ncfg config.Network, log *slog.Logger) contracts.Provider {
	t.Helper()

	productions, known := luaProductionBases[id]
	if !known {
		t.Fatalf("no production base pinned for lua script %q", id)
	}
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
	for _, production := range productions {
		if !strings.Contains(src, production) {
			t.Fatalf("script %q does not pin its production base %q", id, production)
		}
		src = strings.Replace(src, production, testURL, 1)
	}

	// Production-shaped transport: the factory wires every Lua
	// provider to its own netclient (status mapping, cookie jar,
	// fingerprint) — the harness mirrors that so pins like the 403
	// mapping hold.
	cfg := lua.DefaultConfig()
	ncfg.ProxyURL = ""
	client, err := netclient.New(ncfg, netclient.WithProvider(id))
	if err != nil {
		t.Fatalf("netclient for %q: %v", id, err)
	}
	cfg.HTTP = client

	p, err := lua.LoadProviderBytes(cfg, log, id, []byte(src))
	if err != nil {
		t.Fatalf("load lua script %q: %v", id, err)
	}
	return p.Adapt()
}

// luaProviderAtProduction loads the script unmodified (meta tests):
// the first pinned literal is the identity base.
func luaProviderAtProduction(t testing.TB, id string) contracts.Provider {
	t.Helper()
	return luaProvider(t, id, luaProductionBases[id][0])
}
