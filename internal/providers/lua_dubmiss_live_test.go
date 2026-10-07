//go:build live

package providers

// The #159 LIVE dub-miss proof: one resolve with a deliberately wrong
// dub name per state-carrying sibling, on the route matrix's honest
// route (the smoke/live-walk characterizations), proving the typed
// `carries no dub "X" (episode dubs: …)` wall lands on the real wire
// for every sibling — the actionable carrier list the TUI dub menu
// and headless re-asks consume. Never a silent substitution.
//
// Run manually:
//
//	go test ./internal/providers/ -tags live -run TestLiveLuaDubMissTypesCarriers -v
//
// Route matrix (the batch's honest routes): anitokyo, anistar,
// anikoto and animedia ride the proxy (the geo-fenced /
// tarpit-on-direct class); animiku and anikado answer direct (the
// smoke matrix's honest routes). The proxy URL comes from
// ANICLI_LUA_LIVE_PROXY, defaulting to the characterization
// network's local inbound (socks5://127.0.0.1:10808).

import (
	"context"
	"errors"
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

func liveDubMissProvider(t *testing.T, id string, viaProxy bool) contracts.Provider {
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
	if viaProxy {
		proxy := os.Getenv("ANICLI_LUA_LIVE_PROXY")
		if proxy == "" {
			proxy = "socks5://127.0.0.1:10808"
		}
		network.ProxyURL = proxy
	}
	cfg := lua.DefaultConfig()
	cfg.Timeout = 90 * time.Second
	httpClient, err := netclient.New(network, netclient.WithProvider(id))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	cfg.HTTP = httpClient
	p, err := lua.LoadProviderBytes(cfg, nil, id, []byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p.Adapt()
}

func TestLiveLuaDubMissTypesCarriers(t *testing.T) {
	cases := []struct {
		id       string
		query    string
		wrongDub string
		viaProxy bool
		pick     string
	}{
		// anitokyo: the proxy is the honest route (the geo-fenced
		// class); the resolve re-fetches the release page, the scan
		// rides the episode's row. The pick names the TV release —
		// the query also surfaces the playerless anons page (the
		// documented no-RalodePlayer wall) and unrelated cards.
		{"anitokyo", "дандадан", "NoSuchDub Team", true, "8681-dandadan"},
		// animiku: the DLE GET search answers direct (the smoke
		// matrix's honest route); the resolve re-POSTs the bridge.
		{"animiku", "черная лагуна", "NoSuchDub Team", false, ""},
		// anikado: the DLE POST search answers direct (the smoke
		// matrix's honest route); the resolve re-fetches the episode
		// page and rebuilds the translator table.
		{"anikado", "черная лагуна", "NoSuchDub Team", false, ""},
		// anistar: the proxy is the honest route (the direct route
		// tarpits — the anifilm class); the miss is fetch-free (the
		// state IS the carrier table).
		{"anistar", "боруто", "NoSuchDub Team", true, ""},
		// anikoto: the proxy is the honest route (the direct route
		// tarpits); only the SUB/DUB groups exist, so the wrong dub
		// is the site's own label shape.
		{"anikoto", "black lagoon", "Дубляж", true, ""},
		// animedia: the proxy per the batch's route matrix; the
		// resolve re-fetches the page and rebuilds the voice set.
		{"animedia", "врата штейна", "NoSuchDub Team", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := liveDubMissProvider(t, tc.id, tc.viaProxy)
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			results, err := p.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			var picked *contracts.SearchResult
			for i := range results {
				if tc.pick == "" || strings.Contains(results[i].URL, tc.pick) {
					picked = &results[i]
					break
				}
			}
			if picked == nil {
				t.Fatalf("Search(%q) surfaced no result matching %q", tc.query, tc.pick)
			}
			episodes, err := p.GetEpisodes(ctx, picked.URL)
			if err != nil {
				t.Fatalf("GetEpisodes: %v", err)
			}
			if len(episodes) == 0 {
				t.Fatalf("GetEpisodes surfaced zero episodes for %q", picked.URL)
			}

			_, err = p.ResolveStream(ctx, episodes[0], tc.wrongDub)
			if !errors.Is(err, contracts.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound (never a silent substitution)", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `carries no dub "`) {
				t.Errorf("message = %q, want the stable marker", msg)
			}
			if !strings.Contains(msg, "(episode dubs: ") {
				t.Errorf("message = %q, want the actionable carrier list", msg)
			}
			at := strings.Index(msg, "(episode dubs:")
			t.Logf("%s (%s route) typed carrier list: %s", tc.id, routeName(tc.viaProxy), msg[at:])
		})
	}
}

func routeName(viaProxy bool) string {
	if viaProxy {
		return "proxy"
	}
	return "direct"
}
