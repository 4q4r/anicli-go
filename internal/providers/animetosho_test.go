package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// Fixture provenance (PR38, re-captured for the PR146 Lua migration):
// animetosho_search.json carries verbatim live captures of the JSON
// search API (GET https://feed.animetosho.org/json?q=black+lagoon,
// 2026-10-06, anonymous 200), trimmed to two full records. The third
// record is constructed on the real object shape with every optional
// field stripped — it exercises the torrent_url fallback and the
// fail-soft meta (no seeders, no size).
//
// The provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animetosho/main.lua, the PR146
// Go→Lua migration — the torrent family's fifth Lua slot): these
// tests pin the script through the same contracts.Provider surface
// and the same verbatim live-capture discipline the compiled Go
// implementation was held to. The search-side contract is the
// script's; the episode/stream legs stay on the Go TorrentBase via
// the luaTorrent adapter (the engine consumes the surfaced links
// unchanged — the owner ruling), pinned at the roster level below.
//
// Documented API divergence (the migration's one honest swap): the
// compiled provider spoke the newznab XML dialect
// (/api?t=search&cat=5070&limit=30); the script speaks the JSON twin
// (/json?q=…, live-verified 2026-10-06) — the Lua contract has no XML
// parser. The JSON endpoint answers WITHOUT a server-side limit
// (verified: &limit= changes nothing), so the bounded-page rule
// (AnimeToshoSearchLimit, 30) moved client-side into the script.

// animeToshoServer serves a fixed JSON body on every path and counts
// hits (the request-count pins).
func animeToshoServer(t *testing.T, body string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAnimeToshoSearchParsesJSON(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "animetosho", animeToshoServer(t, string(fixture(t, "animetosho_search.json")), nil))
	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}

	batch := results[0]
	if batch.Title != "[Polarwindz] Black Lagoon + Roberta's Blood Trail (BD 1080p HEVC FLAC)" {
		t.Errorf("title = %q", batch.Title)
	}
	if batch.SourceID != "animetosho" {
		t.Errorf("source id = %q, want animetosho", batch.SourceID)
	}
	// The record's torrent_url is the direct .torrent on AT's own
	// storage — PR66 ingestion rides the preflighted bytes, never the
	// tracker-less magnet built from the info_hash (the DHT-only
	// metadata path that timed out the PR52 smoke; the live
	// magnet_uri is base32 and is not taken anyway).
	wantTorrent := "https://storage.animetosho.org/torrent/68e12263b9af297ead6633acc67a8c77f8f3ba2b/%5BPolarwindz%5D%20Black%20Lagoon%20%2B%20Roberta%27s%20Blood%20Trail%20%28BD%201080p%20HEVC%20FLAC%29.torrent"
	if batch.URL != wantTorrent {
		t.Errorf("url = %q, want the torrent_url %q", batch.URL, wantTorrent)
	}
	if batch.Meta[SearchMetaSize] != "75.8 GiB" {
		t.Errorf("size meta = %v, want 75.8 GiB (81377339586 bytes)", batch.Meta[SearchMetaSize])
	}
	if batch.Meta[SearchMetaSeeders] != "4" || batch.Meta[SearchMetaLeechers] != "3" {
		t.Errorf("seeders/leechers meta = %v/%v", batch.Meta[SearchMetaSeeders], batch.Meta[SearchMetaLeechers])
	}
	if batch.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", batch.Meta[SearchMetaQuality])
	}

	second := results[1]
	if second.Title != "[iiPython] Black Lagoon 01-29 + Specials [1080p BD Dual-Audio Multi-Sub OPUS AV1 v2]" {
		t.Errorf("second title = %q", second.Title)
	}
	wantSecond := "https://storage.animetosho.org/torrent/9f1d0bc73e54bd62dabcc1055b615a17ff9aa6b3/%5BiiPython%5D%20Black%20Lagoon%2001-24%20%2B%20OVA%20%2B%20Specials%20%5B1080p%20BD%20Dual-Audio%20Multi-Sub%20OPUS%20AV1%20v2%5D.torrent"
	if second.URL != wantSecond {
		t.Errorf("second url = %q, want the torrent_url %q", second.URL, wantSecond)
	}
	if second.Meta[SearchMetaSize] != "12.0 GiB" {
		t.Errorf("second size meta = %v, want 12.0 GiB (12843106391 bytes)", second.Meta[SearchMetaSize])
	}
	if second.Meta[SearchMetaSeeders] != "13" || second.Meta[SearchMetaLeechers] != "2" {
		t.Errorf("second seeders/leechers meta = %v/%v", second.Meta[SearchMetaSeeders], second.Meta[SearchMetaLeechers])
	}

	// No seed/size fields on the wire → the torrent_url is the link
	// and the meta fails soft (empty strings, never fabricated zeros).
	fallback := results[2]
	if fallback.URL != "https://storage.animetosho.org/torrent/abcdef0123456789abcdef0123456789abcdef01/%5BConstructed%5D%20No%20Attrs%20Show%20%281080p%29.torrent" {
		t.Errorf("fallback url = %q, want the torrent_url", fallback.URL)
	}
	if fallback.Meta[SearchMetaSize] != "" || fallback.Meta[SearchMetaSeeders] != "" {
		t.Errorf("fallback meta = %v/%v, want empty strings (fail-soft)", fallback.Meta[SearchMetaSize], fallback.Meta[SearchMetaSeeders])
	}
	if fallback.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("fallback quality meta = %v, want the PR35 badge from the title", fallback.Meta[SearchMetaQuality])
	}
}

// TestAnimeToshoSearchPrefersTorrentURLBytes pins the PR66 link
// preference: the torrent_url .torrent on AT's own storage wins — it
// is the preflightable, bytes-ingestible link (the PR53 pattern).
// Magnets only serve as the no-torrent_url fallback: a HEX magnet_uri
// rides verbatim (its tr= announces aid peer discovery), otherwise the
// magnet is built from the 40-hex info_hash. The live magnet_uri is
// base32 and must NOT be taken (the engine contract is hex; the
// torrent_url covers those items).
func TestAnimeToshoSearchPrefersTorrentURLBytes(t *testing.T) {
	t.Parallel()

	const body = `[
  {"title":"[Hex] Show - 01 (1080p).mkv",
   "torrent_url":"https://storage.animetosho.org/torrent/1/x.torrent",
   "info_hash":"1111111111111111111111111111111111111111",
   "magnet_uri":"magnet:?xt=urn:btih:1111111111111111111111111111111111111111&tr=udp://tracker.example.org:1337/announce",
   "seeders":10},
  {"title":"[B32] Show - 02 (720p).mkv",
   "torrent_url":"https://storage.animetosho.org/torrent/2/y.torrent",
   "info_hash":"2222222222222222222222222222222222222222",
   "magnet_uri":"magnet:?xt=urn:btih:KZLR34YVXTPNFGFXDDFZETUHGG67DUMD&tr=udp://tracker.example.org:1337/announce",
   "seeders":10},
  {"title":"[NoEnc] Show - 03 (1080p).mkv",
   "info_hash":"3333333333333333333333333333333333333333",
   "magnet_uri":"magnet:?xt=urn:btih:3333333333333333333333333333333333333333&tr=udp://tracker.example.org:1337/announce",
   "seeders":10},
  {"title":"[NoEncNoMagnet] Show - 04 (1080p).mkv",
   "info_hash":"4444444444444444444444444444444444444444",
   "seeders":10}
]`

	p := luaProvider(t, "animetosho", animeToshoServer(t, body, nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].URL != "https://storage.animetosho.org/torrent/1/x.torrent" {
		t.Errorf("hex magnet_uri item = %q, want the torrent_url (own storage, preflightable)", results[0].URL)
	}
	if results[1].URL != "https://storage.animetosho.org/torrent/2/y.torrent" {
		t.Errorf("base32 magnet_uri item = %q, want the torrent_url", results[1].URL)
	}
	if results[2].URL != "magnet:?xt=urn:btih:3333333333333333333333333333333333333333&tr=udp://tracker.example.org:1337/announce" {
		t.Errorf("no-torrent_url hex magnet_uri = %q, want it verbatim (trackers ride)", results[2].URL)
	}
	if results[3].URL != "magnet:?xt=urn:btih:4444444444444444444444444444444444444444&dn="+url.QueryEscape("[NoEncNoMagnet] Show - 04 (1080p).mkv") {
		t.Errorf("no-torrent_url no-magnet_uri = %q, want the info-hash-built magnet", results[3].URL)
	}
}

func TestAnimeToshoSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotRequests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequests = append(gotRequests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animetosho_search.json"))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "animetosho", srv.URL)

	if _, err := p.Search(context.Background(), "black lagoon"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(gotRequests) != 1 {
		t.Fatalf("requests = %v, want exactly one search request", gotRequests)
	}
	// The JSON API endpoint with the raw query; no other parameters —
	// the endpoint carries no category/limit/offset twins (live-verified
	// 2026-10-06), the bounded page is client-side.
	if gotRequests[0] != "/json?q=black+lagoon" {
		t.Errorf("request = %q, want /json?q=black+lagoon", gotRequests[0])
	}
}

// TestAnimeToshoSearchCapsResults pins the bounded-surface rule (the
// compiled AnimeToshoSearchLimit semantics): the JSON endpoint has no
// server-side limit parameter (live-verified 2026-10-06), so the
// script caps the parsed records client-side BEFORE the seedless
// filter — the Go adapter's preflight fan-out downstream can never
// spend unbounded fetches.
func TestAnimeToshoSearchCapsResults(t *testing.T) {
	t.Parallel()

	records := make([]map[string]any, 0, 35)
	for i := range 35 {
		records = append(records, map[string]any{
			"title":       fmt.Sprintf("[Cap] Show - %02d (1080p)", i+1),
			"torrent_url": fmt.Sprintf("https://storage.animetosho.org/torrent/%d/a.torrent", i+1),
			"seeders":     i + 1,
		})
	}
	broad, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal broad fixture: %v", err)
	}

	p := luaProvider(t, "animetosho", animeToshoServer(t, string(broad), nil))
	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 30 {
		t.Fatalf("results = %d, want the bounded page 30", len(results))
	}
}

func TestAnimeToshoSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := luaProvider(t, "animetosho", animeToshoServer(t, string(fixture(t, "animetosho_search.json")), &hits))
	for _, query := range []string{"", "   "} {
		_, err := p.Search(context.Background(), query)
		if err == nil {
			t.Fatalf("query %q: empty search must fail loud", query)
		}
		if !errors.Is(err, contracts.ErrInvalidInput) {
			t.Errorf("query %q: error = %v, want ErrInvalidInput", query, err)
		}
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (no network on the guard)", hits)
	}
}

// TestAnimeToshoSearchSkipsUnusableItems: a record without a title,
// or with a title but none of torrent_url/magnet_uri/info_hash, has
// nothing the engine could ingest — dropped, never a dead result.
func TestAnimeToshoSearchSkipsUnusableItems(t *testing.T) {
	t.Parallel()

	const body = `[
  {"title":"[Good] Show - 01 (1080p).mkv",
   "torrent_url":"https://storage.animetosho.org/torrent/3/z.torrent",
   "total_size":0,
   "seeders":7},
  {"title":"",
   "link":"https://animetosho.org/view/nameless.n2"},
  {"title":"[Broken] No link no attrs",
   "total_size":1024}
]`

	p := luaProvider(t, "animetosho", animeToshoServer(t, body, nil))
	results, err := p.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the usable record)", len(results))
	}
	if results[0].URL != "https://storage.animetosho.org/torrent/3/z.torrent" {
		t.Errorf("url = %q, want the torrent_url", results[0].URL)
	}
	// The strict zero-size pin (the humanBytesAttr unit table's "0"
	// row): a numeric zero converts to the TUI convention.
	if results[0].Meta[SearchMetaSize] != "0 B" {
		t.Errorf("size meta = %v, want 0 B", results[0].Meta[SearchMetaSize])
	}
}

func TestAnimeToshoSearchMalformedJSONTypedError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "animetosho", animeToshoServer(t, "<html>not json</html>", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed JSON must fail loud")
	}
	// The engine's non-marker raise path wraps with the provider/op
	// context (fmt %w, not a *ProviderError — the PR116 taxonomy; the
	// anilibria-torrent sibling pins the same containment shape).
	if !strings.Contains(err.Error(), "provider \"animetosho\" search:") {
		t.Errorf("error = %v, want the provider/op context", err)
	}
	if !strings.Contains(err.Error(), "invalid json") {
		t.Errorf("error = %v, want the invalid-json decode wall", err)
	}
}

func TestAnimeToshoSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "animetosho", srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	if !strings.Contains(err.Error(), "animetosho") {
		t.Errorf("error = %v, want the provider-tagged message", err)
	}
}

// TestAnimeToshoRosterCapabilityAndIdentity pins the hybrid roster
// shape through the real factory assembly: the bundled script serves
// search and identity, the Go TorrentBase keeps the torrent
// capability (IsTorrent/SetEngine/ingest legs). The latin
// name-preference declaration rides the script (the PR42 search
// fan-out routes animetosho the latin variants only).
func TestAnimeToshoRosterCapabilityAndIdentity(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var p contracts.Provider
	for _, item := range bare {
		if item.ID() == "animetosho" {
			p = item
			break
		}
	}
	if p == nil {
		t.Fatal("animetosho must stay in the roster (the bundled script pins its slot)")
	}
	tp, ok := p.(contracts.TorrentProvider)
	if !ok || !tp.IsTorrent() {
		t.Fatal("the Lua-served animetosho slot must keep the torrent capability (the smoke's torrent rule rides it)")
	}
	if _, ok := p.(interface{ SetEngine(*torrent.Engine) }); !ok {
		t.Error("the roster provider must accept the registry's engine injection")
	}
	if p.Name() != "AnimeTosho" {
		t.Errorf("name = %q, want the TUI display name", p.Name())
	}
	if p.BaseURL() != "https://feed.animetosho.org" {
		t.Errorf("base URL = %q, want the feed host the script pins", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	if lc, ok := p.(interface{ ContentLanguage() string }); !ok || lc.ContentLanguage() != "ja" {
		t.Errorf("content language = %v, want ja (JP audio with subs)", lc)
	}
	if np, ok := p.(contracts.NamePreferenceProvider); !ok || np.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want NamePrefLatin (the latin-only index)", np)
	}
}

// TestAnimeToshoEpisodesDelegateToTorrentBase pins the leg ownership:
// GetEpisodes rides the Go TorrentBase (ingest → metadata wait),
// never the script's stub — with no engine wired the call fails with
// the base's not-wired error, not a script answer.
func TestAnimeToshoEpisodesDelegateToTorrentBase(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var p contracts.Provider
	for _, item := range bare {
		if item.ID() == "animetosho" {
			p = item
			break
		}
	}
	if p == nil {
		t.Fatal("animetosho missing from the roster")
	}

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	_, err = p.GetEpisodes(context.Background(), dead)
	if err == nil {
		t.Fatal("GetEpisodes without a wired engine must fail loud")
	}
	if !strings.Contains(err.Error(), "engine is not wired") {
		t.Errorf("error = %v, want the TorrentBase not-wired failure (the Go leg owns episodes)", err)
	}
}

// TestAnimeToshoNotUnconfiguredByDefault pins the no-credentials
// convention: nothing to configure — never in the disabled table with
// default settings (the [torrent] gating is the disabled-table rule).
func TestAnimeToshoNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "animetosho" {
			t.Fatalf("animetosho must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestAnimeToshoDisabledWhenTorrentOff pins the disabled-table rule:
// without the [torrent] subsystem the provider
// cannot play anything, so it is not registered at all.
func TestAnimeToshoDisabledWhenTorrentOff(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false

	var found *DisabledProvider
	for _, d := range UnconfiguredProviders(cfg) {
		if d.ID == "animetosho" {
			dd := d
			found = &dd
		}
	}
	if found == nil {
		t.Fatal("animetosho must be unconfigured when [torrent] is disabled")
	}
	if !strings.Contains(found.Reason, "[torrent]") {
		t.Errorf("reason = %q, want the torrent-subsystem wording", found.Reason)
	}
}

// --- PR66: .torrent-bytes ingestion (the tokyotosho PR53 pattern),
// carried over byte-faithfully by the Go luaTorrent adapter. The
// search result link is the torrent_url .torrent on AT's own storage;
// a search-time preflight pre-fetches every result's bytes (bounded,
// short per-URL budget) and drops the dead ones BEFORE they surface
// (owner standing rule), handing the survivors' bytes to the engine —
// the tracker-less synthesized magnet (DHT-only metadata, the PR52
// smoke killer) is only the no-torrent_url fallback now. The pins run
// the factory hybrid (script search + adapter legs), the exact
// composition the roster serves.

// animeToshoHybrid loads the bundled script through the harness and
// wraps it in the search-in-Lua torrent hybrid the factory serves
// (the offline engine wired — the Go test built its provider the same
// way).
func animeToshoHybrid(t *testing.T, baseURL string) *luaTorrent {
	t.Helper()
	return newLuaTorrent(luaProvider(t, "animetosho", baseURL), nil, newOfflineTestEngine(t))
}

// TestAnimeToshoSearchPreflightDropsDeadHosts pins the PR66 owner
// ruling: every surfaced result's .torrent bytes are pre-fetched
// (bounded, short per-URL budget) BEFORE the result surfaces; a dead
// host drops the result. Seedless records are filtered first and
// never probed.
func TestAnimeToshoSearchPreflightDropsDeadHosts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
	var torrentBytes bytes.Buffer
	if err := mi.Write(&torrentBytes); err != nil {
		t.Fatalf("serialize metainfo: %v", err)
	}

	var fetches atomic.Int64
	live := torrentFixtureServer(t, torrentBytes.Bytes(), &fetches)
	dead := newDeadListener(t)

	body := fmt.Sprintf(`[
  {"title":"Show - 01","torrent_url":"%s/storage/1.torrent","seeders":10},
  {"title":"Show - dead","torrent_url":"http://%s/storage/2.torrent","seeders":10},
  {"title":"Show - seedless","torrent_url":"%s/storage/3.torrent","seeders":0},
  {"title":"Show - 03","torrent_url":"%s/storage/4.torrent","seeders":7}
]`, live.URL, dead.Addr().String(), live.URL, live.URL)

	p := animeToshoHybrid(t, animeToshoServer(t, body, nil))

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the dead host and the seedless item dropped)", len(results))
	}
	if results[0].URL != live.URL+"/storage/1.torrent" || results[1].URL != live.URL+"/storage/4.torrent" {
		t.Errorf("results = [%s, %s], want the two live links in feed order", results[0].URL, results[1].URL)
	}
	// The seedless record was filtered before the preflight: only the
	// two live results were fetched, each exactly once.
	if fetches.Load() != 2 {
		t.Errorf("preflight fetches = %d, want 2", fetches.Load())
	}
}

// TestAnimeToshoSearchPreflightFeedsIngestionNoRefetch: bytes that
// PASSED the preflight are handed to the engine right away, so the
// later GetEpisodes (the resolve leg) must NOT re-fetch the .torrent —
// the cache-reuse assertion of the ruling.
func TestAnimeToshoSearchPreflightFeedsIngestionNoRefetch(t *testing.T) {
	if testing.Short() {
		t.Skip("engine-based ingest in short mode")
	}
	t.Parallel()

	dir := t.TempDir()
	_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
	var torrentBytes bytes.Buffer
	if err := mi.Write(&torrentBytes); err != nil {
		t.Fatalf("serialize metainfo: %v", err)
	}

	var fetches atomic.Int64
	live := torrentFixtureServer(t, torrentBytes.Bytes(), &fetches)
	link := live.URL + "/storage/1.torrent"
	p := animeToshoHybrid(t, animeToshoServer(t,
		fmt.Sprintf(`[{"title":"Show - 01","torrent_url":"%s","seeders":10}]`, link), nil))

	if _, err := p.Search(context.Background(), "show"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("preflight fetches = %d, want 1", fetches.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eps, err := p.GetEpisodes(ctx, link)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) == 0 {
		t.Fatal("episodes = 0, want the release file (metadata was already ingested)")
	}
	if fetches.Load() != 1 {
		t.Errorf("fetches after GetEpisodes = %d, want 1 (ingestion must not double-fetch)", fetches.Load())
	}
}

// TestAnimeToshoSearchPreflightLogsTypedReason: drops are logged with
// the URL and the typed failure reason, never silent.
func TestAnimeToshoSearchPreflightLogsTypedReason(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String() + "/storage/1.torrent"
	p := animeToshoHybrid(t, animeToshoServer(t,
		fmt.Sprintf(`[{"title":"Show - dead","torrent_url":"%s","seeders":10}]`, deadURL), nil))

	var logBuf bytes.Buffer
	p.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))

	if _, err := p.Search(context.Background(), "show"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, deadURL) {
		t.Errorf("log = %q, want the dropped URL", logged)
	}
	if !strings.Contains(logged, "preflight") {
		t.Errorf("log = %q, want the typed preflight reason", logged)
	}
}

// TestAnimeToshoSearchNoEngineSkipsPreflight pins the nil-engine rule:
// without the [torrent] engine there is nothing to preflight or feed,
// so Search keeps the legacy behavior (no prefetch requests — this is
// also what keeps hand-built unit tests network-free). The bare
// harness provider IS the script surface alone (no adapter wrap).
func TestAnimeToshoSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	p := luaProvider(t, "animetosho", animeToshoServer(t, string(fixture(t, "animetosho_search.json")), &hits))
	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (nil engine: no preflight, nothing dropped)", len(results))
	}
	// Exactly ONE request happened: the JSON search itself. No
	// preflight attempted the fixture's storage.animetosho.org URLs.
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only)", hits)
	}
}
