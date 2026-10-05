package providers

import (
	"encoding/json"
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
// base_url literal it pins (the harness rewrites exactly this
// literal; expectations keep the production domain because the
// fixture pages carry it).
var luaProductionBases = map[string]string{
	"anitokyo": "https://anitokyo.tv",
	"animedia": "https://amd.online",
	"animevib": "https://www.animevib.ru",
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

// luaProvider loads the bundled script for id with its base_url
// pointed at testURL.
func luaProvider(t testing.TB, id, testURL string) contracts.Provider {
	t.Helper()

	production, known := luaProductionBases[id]
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
	if !strings.Contains(src, production) {
		t.Fatalf("script %q does not pin its production base %q", id, production)
	}
	src = strings.Replace(src, production, testURL, 1)

	// Production-shaped transport: the factory wires every Lua
	// provider to its own netclient (status mapping, cookie jar,
	// fingerprint) — the harness mirrors that so pins like the 403
	// mapping hold.
	cfg := lua.DefaultConfig()
	client, err := netclient.New(config.Default().Network, netclient.WithProvider(id))
	if err != nil {
		t.Fatalf("netclient for %q: %v", id, err)
	}
	cfg.HTTP = client

	p, err := lua.LoadProviderBytes(cfg, nil, id, []byte(src))
	if err != nil {
		t.Fatalf("load lua script %q: %v", id, err)
	}
	return p.Adapt()
}

// luaProviderAtProduction loads the script unmodified (meta tests).
func luaProviderAtProduction(t testing.TB, id string) contracts.Provider {
	t.Helper()
	return luaProvider(t, id, luaProductionBases[id])
}
