package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// seedersOf extracts the SearchMetaSeeders meta of a result.
func seedersOf(r contracts.SearchResult) string {
	if r.Meta == nil {
		return ""
	}
	s, _ := r.Meta[SearchMetaSeeders].(string)
	return s
}

func resultTitles(rs []contracts.SearchResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Title)
	}
	return out
}

// TestAnimeToshoSearchFiltersSeedless: the seedless-drop rule over
// the JSON feed's seeder counts. The rule lives in the bundled script
// since the PR146 Lua migration — the pin rides the same inline
// fixtures through the harness (the anilibria-torrent precedent).
func TestAnimeToshoSearchFiltersSeedless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"title":"Seeded AT","torrent_url":"https://x/1.torrent","info_hash":"0123456789012345678901234567890123456789","seeders":7},
			{"title":"Dead AT","torrent_url":"https://x/2.torrent","info_hash":"0123456789012345678901234567890123456788","seeders":0}
		]`))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "animetosho", srv.URL)

	results, err := p.Search(context.Background(), "query")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if titles := resultTitles(results); len(titles) != 1 || titles[0] != "Seeded AT" {
		t.Fatalf("results = %v, want only the seeded item", titles)
	}
}

// TestAnilibriaTorrentSearchFiltersSeedless: the API's seeders int
// rides Meta; zero-seed torrents never surface. The seedless filter
// lives in the bundled script since the PR145 Lua migration — the pin
// rides the same inline fixtures through the harness.
func TestAnilibriaTorrentSearchFiltersSeedless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/app/search/releases":
			_, _ = w.Write([]byte(`[{"id":5,"alias":"rel","name":{"main":"Rel"}}]`))
		case "/api/v1/anime/torrents/release/5":
			_, _ = w.Write([]byte(`[
				{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","label":"Seeded AT rel","seeders":5,"leechers":1,"size":100},
				{"hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","label":"Dead AT rel","seeders":0,"leechers":0,"size":100}
			]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "anilibria-torrent", srv.URL)

	results, err := p.Search(context.Background(), "query")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range results {
		if seedersOf(r) == "0" {
			t.Fatalf("seedless result surfaced: %+v", r)
		}
	}
	if titles := resultTitles(results); len(titles) != 1 || titles[0] != "Seeded AT rel" {
		t.Fatalf("results = %v, want only the seeded torrent", titles)
	}
}

// TestTokyotoshoSearchFailSoftNoSeedField: the TT feed carries no seed
// counts (live-verified) — the fail-soft rule keeps every anime item.
func TestTokyotoshoSearchFailSoftNoSeedField(t *testing.T) {
	body := `<?xml version="1.0"?><rss><channel>
<item><category>Anime</category><title>TT A</title><link>https://x/a.torrent</link>
<description>&lt;b&gt;Size: 1.66GB&lt;/b&gt;</description></item>
<item><category>Anime</category><title>TT B</title><link>https://x/b.torrent</link>
<description>&lt;b&gt;Size: 700MB&lt;/b&gt;</description></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "tokyotosho", srv.URL)

	results, err := p.Search(context.Background(), "query")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("fail-soft: no seed field means no filtering, got %v", resultTitles(results))
	}
}

// TestFilterSeedlessUnit pins the rule directly: only a parsable
// seeders value of 0 drops a result; garbage/absent stays.
func TestFilterSeedlessUnit(t *testing.T) {
	mk := func(seeders string) contracts.SearchResult {
		return contracts.SearchResult{Title: "t", Meta: map[string]any{SearchMetaSeeders: seeders}}
	}
	out := filterSeedless([]contracts.SearchResult{
		mk("5"), mk("0"), mk(""), mk("garbage"), {Title: "nometa"},
	})
	if len(out) != 4 {
		t.Fatalf("filterSeedless kept %d, want 4 (only the explicit 0 dropped)", len(out))
	}
}

// filterSeedless is referenced from the providers package; pin that
// the helper (not the callers) owns the rule.
var (
	_ = filterSeedless
	_ sync.Mutex
)
