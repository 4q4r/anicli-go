package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// subsplease (PR144): the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/subsplease/main.lua) — the roster's
// first torrent Go→Lua migration. These tests pin the script through
// the same contracts.Provider surface and the same fixtures the
// compiled Go implementation was held to (the luaProductionBases
// harness); the torrent ENGINE legs stay Go (the PR144 owner ruling),
// so the ingest/episodes assertions drive the search-in-Lua hybrid
// the factory serves (luatorrent.go): the script surfaces the
// magnets, TorrentBase consumes them.

// spSearchFixtureResults is the number of results the trimmed live
// search fixture yields: two releases × three resolutions, wire order.
const spSearchFixtureResults = 6

// The script's surfaced-surface caps (the compiled provider's
// constants, pinned here through the cap fixtures): 18 episode
// results (the newest releases × resolutions) plus 6 batch results.
const (
	subspleaseEpisodeCap = 18
	subspleaseBatchCap   = 6
)

// spMagnet builds a well-formed tracker-rich test magnet (the API
// shape: base32 btih, dn title, xl byte length, one tr announce).
func spMagnet(dn string, xl int64) string {
	return "magnet:?xt=urn:btih:M5TKWR3KC5D2MAQKVGEDXINMGKQ7GRXE" +
		"&dn=" + url.QueryEscape(dn) +
		"&xl=" + strconv.FormatInt(xl, 10) +
		"&tr=" + url.QueryEscape("udp://tracker.opentrackr.org:1337/announce")
}

// spQuote JSON-encodes one string value.
func spQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// spReleaseJSON renders one KEYED f=search payload entry (the key is
// the release display name; the downloads carry the given magnet
// list; res is the test-wide 720p default).
func spReleaseJSON(key, show, episode, page string, magnets ...string) string {
	var b strings.Builder
	b.WriteString(spQuote(key) + `:{"show":` + spQuote(show) + `,"episode":` + spQuote(episode))
	if page != "" {
		b.WriteString(`,"page":` + spQuote(page))
	}
	b.WriteString(`,"downloads":[`)
	for i, m := range magnets {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"res":"720","magnet":` + spQuote(m) + `}`)
	}
	b.WriteString("]}")
	return b.String()
}

// spSearchPayload wraps release entries into the f=search object,
// preserving the given order (the wire order is meaningful).
func spSearchPayload(entries ...string) string {
	return "{" + strings.Join(entries, ",") + "}"
}

// spShowPayload wraps the f=show batch/episode object.
func spShowPayload(batches []string) string {
	return `{"batch":{` + strings.Join(batches, ",") + `},"episode":{}}`
}

// spServer serves fixture files by path prefix; hits, when non-nil,
// counts requests.
func spServer(t *testing.T, routes map[string]string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		for prefix, name := range routes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture(t, name))
				return
			}
		}
		// Un-routed show pages answer the no-sid shape: the batch hop
		// fails soft and the episode surface stays intact.
		if strings.HasPrefix(r.URL.Path, "/shows/") {
			_, _ = w.Write([]byte("<html><body>no table here</body></html>"))
			return
		}
		t.Errorf("unexpected request %s", r.URL)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// spFixtureRelease mirrors one search fixture release (test-side
// decode only).
type spFixtureRelease struct {
	Downloads []struct {
		Res    string `json:"res"`
		Magnet string `json:"magnet"`
	} `json:"downloads"`
}

func fixtureJSON(t *testing.T, name string) map[string]spFixtureRelease {
	t.Helper()
	var out map[string]spFixtureRelease
	if err := json.Unmarshal(fixture(t, name), &out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return out
}

// newSubsPleaseHybrid loads the bundled script through the harness
// and wraps it in the search-in-Lua torrent hybrid the factory serves
// (the engine stays nil — SetEngine injects the offline one).
func newSubsPleaseHybrid(t *testing.T, baseURL string) *luaTorrent {
	t.Helper()
	return newLuaTorrent(luaProvider(t, "subsplease", baseURL), nil, nil)
}

func TestSubsPleaseSearchParsesReleases(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "subsplease", spServer(t, map[string]string{
		"/api/": "subsplease_api_search_rezero.json",
	}, nil))
	results, err := p.Search(context.Background(), "re:zero")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != spSearchFixtureResults {
		t.Fatalf("results = %d, want %d (2 releases × 3 resolutions)", len(results), spSearchFixtureResults)
	}

	// Wire order is meaningful (newest release first, resolutions in
	// the download-array order 480/720/1080).
	first := results[0]
	if first.Title != "[SubsPlease] Re Zero kara Hajimeru Isekai Seikatsu - 84 (480p) [4B99F832].mkv" {
		t.Errorf("title = %q, want the magnet dn release name", first.Title)
	}
	if first.SourceID != "subsplease" {
		t.Errorf("source id = %q, want subsplease", first.SourceID)
	}
	if !strings.HasPrefix(first.URL, "magnet:?xt=urn:btih:") {
		t.Errorf("url = %q, want the API magnet verbatim", first.URL)
	}
	if got := first.Meta[SearchMetaSize]; got != "369.2 MiB" {
		t.Errorf("size meta = %v, want 369.2 MiB (the xl= byte length)", got)
	}
	if got := first.Meta[SearchMetaQuality]; got != "480p" {
		t.Errorf("quality meta = %v, want the PR35 badge 480p", got)
	}
	if got := results[2].Meta[SearchMetaQuality]; got != "1080p" {
		t.Errorf("1080p quality meta = %v", got)
	}
	if got := results[2].Meta[SearchMetaSize]; got != "1.3 GiB" {
		t.Errorf("1080p size meta = %v, want 1.3 GiB", got)
	}
	if results[3].Title != "[SubsPlease] Re Zero kara Hajimeru Isekai Seikatsu - 55 (480p) [6DA15600].mkv" {
		t.Errorf("second release title = %q, want the - 55 release", results[3].Title)
	}
	// Magnets are passed through unmodified: the engine owns them.
	raw := fixtureJSON(t, "subsplease_api_search_rezero.json")
	want := raw["Re Zero kara Hajimeru Isekai Seikatsu - 84"].Downloads[2].Magnet
	if results[2].URL != want {
		t.Errorf("1080p url = %q, want the fixture magnet verbatim", results[2].URL)
	}
}

func TestSubsPleaseSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	if _, err := p.Search(context.Background(), "re:zero"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/api/" {
		t.Errorf("path = %q, want /api/", gotPath)
	}
	if gotQuery.Get("f") != "search" {
		t.Errorf("f = %q, want search", gotQuery.Get("f"))
	}
	if gotQuery.Get("s") != "re:zero" {
		t.Errorf("s = %q, want the raw query", gotQuery.Get("s"))
	}
	// tz is REQUIRED — the endpoint answers empty without it
	// (live-verified 2026-09-23 and again 2026-10-06); any fixed value
	// works.
	if gotQuery.Get("tz") != "0" {
		t.Errorf("tz = %q, want 0", gotQuery.Get("tz"))
	}
}

func TestSubsPleaseSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := luaProvider(t, "subsplease", spServer(t, map[string]string{
		"/api/": "subsplease_api_search_rezero.json",
	}, &hits))
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

// TestSubsPleaseSearchEmptyResultSet: the no-match answer is the
// literal "[]" (live capture, verbatim fixture: f=search&s=black
// lagoon — the show is outside the SubsPlease seasonal catalog; the
// PHP empty-assoc quirk serializes an empty result as an array). It
// settles as zero results, nil error — and NO batch-expansion hop.
func TestSubsPleaseSearchEmptyResultSet(t *testing.T) {
	t.Parallel()

	hits := 0
	p := luaProvider(t, "subsplease", spServer(t, map[string]string{
		"/api/": "subsplease_api_search_empty.json",
	}, &hits))
	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (search only, no show hop)", hits)
	}
}

// TestSubsPleaseSearchMalformedBodyFailsLoud pins the broken-body
// wall. The Lua classification carries the provider id and the op in
// the error message (the typed ProviderError shape rides the marker
// kinds — the compiled provider's decode wall surfaced the same
// context through contracts.WrapProvider).
func TestSubsPleaseSearchMalformedBodyFailsLoud(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not json at all"))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed body must fail loud")
	}
	if !strings.Contains(err.Error(), "subsplease") || !strings.Contains(err.Error(), "search") {
		t.Errorf("error = %v, want the subsplease search context", err)
	}
}

func TestSubsPleaseSearchHTTPErrorFailsLoud(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	if !strings.Contains(err.Error(), "subsplease") {
		t.Errorf("error = %v, want the subsplease-tagged failure", err)
	}
}

// TestSubsPleaseSearchSkipsUnusableDownloads: downloads without a
// magnet, and magnets the engine cannot parse (no usable btih), are
// dropped; a release left with zero usable downloads disappears.
func TestSubsPleaseSearchSkipsUnusableDownloads(t *testing.T) {
	t.Parallel()

	good := spMagnet("[SubsPlease] Show - 01 (720p) [AAAA].mkv", 387157507)
	body := spSearchPayload(
		spReleaseJSON("Show - 01", "Show", "01", "show",
			"",                   // no magnet at all
			"magnet:?xt=garbage", // unparseable btih
			good,
		),
		spReleaseJSON("Show - 02", "Dead", "02", "", "magnet:?dn=no-hash"),
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (unusable downloads and the magnetless release dropped)", len(results))
	}
	if results[0].URL != good {
		t.Errorf("url = %q, want the one usable magnet", results[0].URL)
	}
}

// TestSubsPleaseSearchSynthesizesTitleWithoutDn: the release title
// rides the magnet dn=; a magnet without one synthesizes the title
// from the API's show/episode/res fields instead of surfacing an
// empty name.
func TestSubsPleaseSearchSynthesizesTitleWithoutDn(t *testing.T) {
	t.Parallel()

	body := spSearchPayload(spReleaseJSON("Show - 5", "Show", "5", "show",
		"magnet:?xt=urn:btih:M5TKWR3KC5D2MAQKVGEDXINMGKQ7GRXE&xl=742095133"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Title != "Show - 5 (720p)" {
		t.Errorf("title = %q, want the synthesized \"Show - 5 (720p)\"", results[0].Title)
	}
}

// TestSubsPleaseSearchExpandsTopShowBatch pins the back-catalog flow
// (live-verified 2026-09-23): the top matched show's page carries the
// sid attribute, and /api/?f=show&sid=… returns the batch releases —
// surfaced AFTER the episode results. The batch magnets carry the
// same tracker-rich shape.
func TestSubsPleaseSearchExpandsTopShowBatch(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/" && r.URL.Query().Get("f") == "search":
			_, _ = w.Write(fixture(t, "subsplease_api_search_rezero.json"))
		case r.URL.Path == "/shows/re-zero-kara-hajimeru-isekai-seikatsu/":
			_, _ = w.Write(fixture(t, "subsplease_show_page.html"))
		case r.URL.Path == "/api/" && r.URL.Query().Get("f") == "show":
			if got := r.URL.Query().Get("sid"); got != "87" {
				t.Errorf("show sid = %q, want 87 (the show-page attribute)", got)
			}
			_, _ = w.Write(fixture(t, "subsplease_api_show87.json"))
		default:
			t.Errorf("unexpected request %s", r.URL)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "re:zero")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 2 episode releases × 3 res + 2 batches × 3 res.
	if len(results) != 12 {
		t.Fatalf("results = %d, want 12 (6 episode + 6 batch results)", len(results))
	}
	if !strings.Contains(results[6].Title, "(26-50)") {
		t.Errorf("first batch title = %q, want the - 26-50 batch (wire order)", results[6].Title)
	}
	if !strings.Contains(results[6].Title, "[Batch]") {
		t.Errorf("first batch title = %q, want the [Batch] marker", results[6].Title)
	}
	if !strings.HasPrefix(results[6].URL, "magnet:?xt=urn:btih:") {
		t.Errorf("batch url = %q, want the batch magnet", results[6].URL)
	}
	if got := results[8].Meta[SearchMetaQuality]; got != "1080p" {
		t.Errorf("batch 1080p quality meta = %v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 {
		t.Fatalf("requests = %v, want exactly 3 (search → show page → show API)", paths)
	}
	if !strings.HasPrefix(paths[1], "/shows/re-zero-kara-hajimeru-isekai-seikatsu/") {
		t.Errorf("second request = %q, want the show page", paths[1])
	}
	if !strings.Contains(paths[2], "f=show") || !strings.Contains(paths[2], "sid=87") {
		t.Errorf("third request = %q, want the f=show&sid=87 payload", paths[2])
	}
}

// TestSubsPleaseSearchBatchExpansionFailSoft: the batch hop is an
// enhancement — a show page without a usable sid (an edge layout, an
// error page) must not fail the search or lose the episode results.
func TestSubsPleaseSearchBatchExpansionFailSoft(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/":
			_, _ = w.Write(fixture(t, "subsplease_api_search_rezero.json"))
		default:
			// The show page answers garbage: no sid anywhere.
			_, _ = w.Write([]byte("<html><body>no table here</body></html>"))
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "re:zero")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != spSearchFixtureResults {
		t.Fatalf("results = %d, want %d (episode results intact, no batch rows)", len(results), spSearchFixtureResults)
	}
}

// TestSubsPleaseSearchEpisodeCap pins the surfaced-surface bound: the
// episode results stop at the wire-order head (the newest releases
// survive, the tail is cut).
func TestSubsPleaseSearchEpisodeCap(t *testing.T) {
	t.Parallel()

	entries := make([]string, 0, 8)
	for i := range 8 {
		m := spMagnet("[SubsPlease] Show - "+strconv.Itoa(i+1)+" (720p) [AAAA].mkv", 387157507)
		entries = append(entries, spReleaseJSON("Show - "+strconv.Itoa(i+1), "Show", strconv.Itoa(i+1), "", m, m, m))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(spSearchPayload(entries...)))
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != subspleaseEpisodeCap {
		t.Fatalf("results = %d, want the cap %d", len(results), subspleaseEpisodeCap)
	}
	if results[0].Title != "[SubsPlease] Show - 1 (720p) [AAAA].mkv" {
		t.Errorf("first result = %q, want the wire-order head", results[0].Title)
	}
}

// TestSubsPleaseSearchBatchCap pins the batch surface bound: a show
// payload with many batches stops at the batch cap.
func TestSubsPleaseSearchBatchCap(t *testing.T) {
	t.Parallel()

	batches := make([]string, 0, 8)
	for i := range 8 {
		lo, hi := i*12+1, (i+1)*12
		dn := "[SubsPlease] Show (" + strconv.Itoa(lo) + "-" + strconv.Itoa(hi) + ") (720p) [Batch]"
		key := "Show - " + strconv.Itoa(lo) + "-" + strconv.Itoa(hi)
		batches = append(batches, spReleaseJSON(key, "Show", key, "", spMagnet(dn, 1449305708)))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/" && r.URL.Query().Get("f") == "search":
			_, _ = w.Write([]byte(spSearchPayload(spReleaseJSON("Show - 1", "Show", "1", "show",
				spMagnet("[SubsPlease] Show - 1 (720p) [AAAA].mkv", 1)))))
		case r.URL.Path == "/shows/show/":
			_, _ = w.Write([]byte(`<table id="show-release-table" sid="9"></table>`))
		default:
			if r.URL.Path == "/api/" && r.URL.Query().Get("f") == "show" {
				_, _ = w.Write([]byte(spShowPayload(batches)))
				return
			}
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "subsplease", srv.URL)

	results, err := p.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 1 episode result + the batch cap.
	if len(results) != 1+subspleaseBatchCap {
		t.Fatalf("results = %d, want 1 episode + batch cap %d", len(results), subspleaseBatchCap)
	}
}

// TestSubsPleaseLinkIsEngineIngestable pins the ingestion contract on
// the REAL magnet shape through the hybrid the factory serves: the
// API's base32 btih magnets must go through TorrentBase.Ingest
// without error (anacrolix parses 32-char base32 xt= values — the
// animetosho "40-hex" note covers tracker-list files, not magnet
// URIs).
func TestSubsPleaseLinkIsEngineIngestable(t *testing.T) {
	if testing.Short() {
		t.Skip("engine-based ingest in short mode")
	}
	t.Parallel()

	p := newSubsPleaseHybrid(t, spServer(t, map[string]string{
		"/api/": "subsplease_api_search_rezero.json",
	}, nil))
	results, err := p.Search(context.Background(), "re:zero")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("results = 0, want the fixture surface")
	}

	p.SetEngine(newOfflineTestEngine(t))
	ih, err := p.Ingest(context.Background(), results[0].URL)
	if err != nil {
		t.Fatalf("Ingest(api magnet): %v", err)
	}
	if ih == (torrent.InfoHash{}) {
		t.Fatal("infohash = zero, want the parsed base32 btih")
	}
}

// TestSubsPleaseCapabilityAndRoster pins the hybrid's capability
// surfaces: the torrent capability plus the script-declared
// declarations the registry and the parity smoke probe.
func TestSubsPleaseCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newSubsPleaseHybrid(t, spServer(t, map[string]string{
		"/api/": "subsplease_api_search_rezero.json",
	}, nil))
	if !p.IsTorrent() {
		t.Error("subsplease must carry the torrent capability")
	}
	if p.ID() != "subsplease" || p.Name() != "SubsPlease" {
		t.Errorf("id/name = %q/%q", p.ID(), p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, EN subs)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP audio with EN subs)", got)
	}
	if p.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("name preference = %v, want latin (EN/romaji release names)", p.NamePreference())
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the site root, not empty")
	}
	// The declared smoke query (PR51): the shared "black lagoon" probe
	// is outside the SubsPlease seasonal catalog entirely — the
	// provider speaks for itself.
	if got := p.SmokeQuery(); got == "" || got == "black lagoon" {
		t.Errorf("smoke query = %q, want a declared catalog answer", got)
	}
}

// TestSubsPleaseNotUnconfiguredByDefault pins the no-credentials
// convention: subsplease has nothing to configure, so it must never
// appear in the disabled table with default settings.
func TestSubsPleaseNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "subsplease" {
			t.Fatalf("subsplease must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// TestSubsPleaseGetEpisodesDelegatesToEpisodesWait: the hybrid's
// GetEpisodes path rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func TestSubsPleaseGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	p := newSubsPleaseHybrid(t, "http://127.0.0.1:1")
	p.SetEngine(newOfflineTestEngine(t))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	eps, err := p.GetEpisodes(ctx, dead)
	if err == nil {
		t.Fatal("GetEpisodes on unreachable metadata must fail loud")
	}
	if len(eps) != 0 {
		t.Errorf("episodes = %v, want none", eps)
	}
}

// TestSubsPleaseBatchHopFailureIsLogged: the fail-soft batch skip is
// logged with the typed reason, never silent (PR62 #4).
func TestSubsPleaseBatchHopFailureIsLogged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/" {
			_, _ = w.Write(fixture(t, "subsplease_api_search_rezero.json"))
			return
		}
		_, _ = w.Write([]byte("<html>garbage</html>"))
	}))
	t.Cleanup(srv.Close)

	var logBuf bytes.Buffer
	p := luaProviderWithLogger(t, "subsplease", srv.URL, slog.New(slog.NewTextHandler(&logBuf, nil)))

	if _, err := p.Search(context.Background(), "re:zero"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(logBuf.String(), "batch") {
		t.Errorf("log = %q, want the typed batch-skip reason", logBuf.String())
	}
}

// TestSubsPleaseTorrentOffDropsTheHybrid pins the kodik-parity rule
// for the hybrid slot: with [torrent] disabled the script still loads
// (its search is plain HTTP), but the slot must NOT register — its
// results resolve through the engine, and the disabled set must name
// the reason (the compiled torrent factories drop the same way).
func TestSubsPleaseTorrentOffDropsTheHybrid(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Torrent.Enabled = false
	bare, disabled, err := allWithCFDisabled(cfg, nil, nil, discardLogger)
	if err != nil {
		t.Fatalf("allWithCFDisabled: %v", err)
	}
	for _, p := range bare {
		if p.ID() == "subsplease" {
			t.Fatalf("subsplease registered with [torrent] disabled (it cannot resolve without the engine)")
		}
	}
	for _, d := range disabled {
		if d.ID == "subsplease" {
			if d.Reason == "" {
				t.Error("disabled reason must be user-facing (RU), got empty")
			}
			return
		}
	}
	t.Errorf("subsplease missing from the disabled set: %v", disabled)
}
