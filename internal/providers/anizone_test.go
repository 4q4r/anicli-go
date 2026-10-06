package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// The fixtures are real captures of anizone.to (2026-09-18, see
// testdata/README.md): the Livewire search/series pages and the
// vidstackPlayer watch page. Page chrome is trimmed; payload values are
// verbatim.
//
// PR130: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anizone/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to.

// azDub is the fixed single dub name of the sub-only catalog (the
// script emits it as the raw_embeds key; the streams resolution keys
// on it).
const azDub = "Original (AniZone)"

// azProvider loads the bundled anizone script against the test server
// (the Lua harness rewrites the production base literal).
func azProvider(t *testing.T, srvURL string) contracts.Provider {
	t.Helper()
	return luaProvider(t, "anizone", srvURL)
}

// rewritePlayerSrc points the captured vidstackPlayer src at a test
// m3u8 server. The watch fixture keeps the verbatim production URL in
// its RAW double-encoded form (the payload string literal carries
// `\\\/` for every slash — JSON-unescape leaves `\/`, the recipe's
// normalizeUrl then collapses it onto `/`), so the rewrite swaps the
// raw form for the raw form of the test URL. Everything else rides
// along verbatim — the same clamp-and-re-encode pattern the PR78
// pagination test uses.
func rewritePlayerSrc(t *testing.T, body []byte, m3u8URL string) []byte {
	t.Helper()

	const captured = `https:\\\/\\\/seiryuu.vid-cdn.xyz\\\/41b2995c-9811-4353-a3f5-a2815e45887e\\\/master.m3u8`
	replacement := strings.ReplaceAll(m3u8URL, "/", `\\\/`)
	out := bytes.Replace(body, []byte(captured), []byte(replacement), 1)
	if bytes.Equal(out, body) {
		t.Fatal("rewritePlayerSrc: captured src not found in watch fixture")
	}
	return out
}

func TestAniZoneSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write(fixture(t, "anizone_search.html"))
	})
	p := azProvider(t, srv.URL)

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != http.MethodGet || rec.Path != "/anime" {
		t.Errorf("request = %s %s, want GET /anime", rec.Method, rec.Path)
	}
	got, err := url.ParseQuery(rec.Query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if got.Get("search") != "black lagoon" {
		t.Errorf("query = %q, want search=\"black lagoon\"", rec.Query)
	}

	// Real capture (anizone.to /anime?search=black lagoon): three hits.
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	first := results[0]
	// pickTitle order is title_list["1"] first (recipe pickTitle:
	// "1" → "5" → "8" → first value).
	if first.Title != "Black Lagoon" {
		t.Errorf("Title = %q, want the title_list[\"1\"] value", first.Title)
	}
	if first.URL != "a8vfumal" {
		t.Errorf("URL = %q, want the slug (GetEpisodes consumes it)", first.URL)
	}
	if first.SourceID != "anizone" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster != "https://anizone.to/images/anime/c05ffeb2-617d-4a52-af9f-19131a5c8b31.jpg" {
		t.Errorf("Poster = %q, want the cover URL", first.Poster)
	}
	// The Lua adapter decodes meta numbers as json.Number (the
	// script passes the payload's start_year through).
	if year, ok := first.Meta["year"].(json.Number); !ok || year.String() != "2006" {
		t.Errorf("Meta[year] = %#v, want 2006", first.Meta["year"])
	}
	if first.Meta["type"] != "TV Series" {
		t.Errorf("Meta[type] = %v, want the site format string", first.Meta["type"])
	}
	if results[1].Title != "Black Lagoon: Roberta`s Blood Trail" {
		t.Errorf("results[1].Title = %q", results[1].Title)
	}
	if results[2].URL != "dwhx8kxv" {
		t.Errorf("results[2].URL = %q, want the slug", results[2].URL)
	}
}

func TestAniZoneSearchSendsHeaders(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html></html>")
	})
	p := azProvider(t, srv.URL)

	if _, err := p.Search(context.Background(), "q"); err == nil {
		t.Fatal("Search on a payload-less page must surface the typed error")
	}
	if got := rec.Header.Get("Referer"); got != srv.URL+"/" {
		t.Errorf("Referer = %q, want the site root (recipe fetchPage)", got)
	}
	if got := rec.Header.Get("Accept"); !strings.Contains(got, "text/html") {
		t.Errorf("Accept = %q, want the html document accept", got)
	}
	if got := rec.Header.Get("User-Agent"); got == "" {
		t.Error("User-Agent = empty, want the netclient default")
	}
}

func TestAniZoneSearchTypedErrors(t *testing.T) {
	t.Parallel()

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		p := azProvider(t, "http://"+newDeadListener(t).Addr().String())
		results, err := p.Search(context.Background(), "q")
		if err == nil {
			t.Fatal("Search err = nil, want the transport error")
		}
		if results != nil {
			t.Errorf("results = %v, want nil", results)
		}
	})

	t.Run("payload not found", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "<html><body>no payload here</body></html>")
		})
		p := azProvider(t, srv.URL)
		_, err := p.Search(context.Background(), "q")
		if err == nil {
			t.Fatal("Search err = nil, want the payload error")
		}
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Fatalf("err = %v, want ErrExtractFailed", err)
		}
		var pe *contracts.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a *contracts.ProviderError", err)
		}
		if pe.Provider != "anizone" || pe.Op != contracts.OpSearch {
			t.Errorf("ProviderError = %+v, want provider=anizone op=search", pe)
		}
	})

	t.Run("empty items is a clean miss", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<script>items: JSON.parse('[]')</script>`)
		})
		p := azProvider(t, srv.URL)
		results, err := p.Search(context.Background(), "q")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("results = %d, want 0", len(results))
		}
	})
}

// TestAniZoneSearchNoResultsPage pins the PR78 clean-miss semantics: the
// site's legit no-results answer (verbatim capture, cyrillic query —
// testdata/anizone_search_empty.html) is the FULL Anime Index Livewire
// page with an empty result block and NO items payload script at all.
// That page must settle as zero results, not the typed extract failure —
// the fan-out reaches this provider with cyrillic-only variant sets
// (enrichment off), and a miss is a normal search outcome (PR78:
// «Ателье колдовских колпаков» row failed with "search payload not
// found" live). A page without the index chrome still fails loud (the
// "payload not found" subtest above).
func TestAniZoneSearchNoResultsPage(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write(fixture(t, "anizone_search_empty.html"))
	})
	p := azProvider(t, srv.URL)

	results, err := p.Search(context.Background(), "Ателье колдовских колпаков")
	if err != nil {
		t.Fatalf("Search on the no-results page must be a clean miss, got: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAniZoneGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anime/a8vfumal" {
			_, _ = w.Write(fixture(t, "anizone_series.html"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	p := azProvider(t, srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "a8vfumal")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/anime/a8vfumal" {
		t.Errorf("request path = %q", rec.Path)
	}
	// Real capture: twelve regular episodes plus four specials. The
	// recipe drops the non-numeric slugs (s1…s4) — the watch URL and
	// the numbering both assume episode numbers.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12 (specials dropped)", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" || first.RawID != luaStateJSONOf(srv.URL+"/anime/a8vfumal/1", "1") {
		t.Errorf("Num/RawID = %q/%q, want 1 and the {n,u} state JSON", first.Num, first.RawID)
	}
	if first.Title != "The Black Lagoon" {
		t.Errorf("Title = %q, want the title_list[\"1\"] value", first.Title)
	}
	embeds := first.RawEmbeds[azDub]
	if len(embeds) != 1 || embeds[0] != srv.URL+"/anime/a8vfumal/1" {
		t.Errorf("RawEmbeds = %v, want the single watch URL", first.RawEmbeds)
	}
	if episodes[11].Num != "12" {
		t.Errorf("episodes[11].Num = %q, want 12 (sorted)", episodes[11].Num)
	}
}

// luaStateJSONOf builds the {n, u} state JSON the script encodes into
// raw_id (the fresh-sandbox streams() state carrier — the
// animevost/anilib precedent, key order "n" then "u").
func luaStateJSONOf(pageURL, num string) string {
	b, err := json.Marshal(map[string]string{"n": num, "u": pageURL})
	if err != nil {
		return ""
	}
	return string(b)
}

// The page-one fixture is the live One Piece capture (hasMore: true +
// nextCursor), the continuation is the live /livewire/update response
// (items 25–48). Together they pin the two-leg walk: the POST must
// carry the decoded snapshot, the cursor and the csrf of the initial
// page, and the session cookie the netclient jar replayed.
func TestAniZoneGetEpisodesPagination(t *testing.T) {
	t.Parallel()

	type hit struct {
		method string
		path   string
		header http.Header
		body   []byte
	}
	var mu sync.Mutex
	var hits []hit
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := hit{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
		if r.Method == http.MethodPost {
			h.body, _ = readAllLimit(r.Body)
		}
		mu.Lock()
		hits = append(hits, h)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			// The live flow stores session cookies on the page fetch;
			// the continuation must replay them from the client jar.
			http.SetCookie(w, &http.Cookie{ //nolint:gosec // test double for the live session cookie, never a real credential
				Name: "anizone_session", Value: "az-jar-check", Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			_, _ = w.Write(fixture(t, "anizone_series_paged.html"))
		case r.Method == http.MethodPost && r.URL.Path == "/livewire/update":
			// The captured page-two response carries hasMore: true; the
			// walk terminates on the next (terminal) continuation.
			w.Header().Set("Content-Type", "application/json")
			posts++
			if posts == 1 {
				_, _ = w.Write(fixture(t, "anizone_livewire_page2.json"))
				return
			}
			_, _ = w.Write([]byte(`{"components":[{"snapshot":"terminal","effects":{"dispatches":[{"name":"items-loaded","params":{"items":[],"nextCursor":null,"hasMore":false}}]}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p := azProvider(t, srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "uyyyn4kf")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(hits) != 3 {
		t.Fatalf("requests = %d, want the page fetch + captured continuation + terminal continuation", len(hits))
	}
	post := hits[1]
	if post.method != http.MethodPost || post.path != "/livewire/update" {
		t.Fatalf("continuation = %s %s, want POST /livewire/update", post.method, post.path)
	}
	if got := post.header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want json", got)
	}
	// The csrf travels verbatim from the captured page meta.
	if got := post.header.Get("X-CSRF-TOKEN"); got != "didEEhClXbQU8zvGuRLWub28LrIPAB2nerOgr6bt" {
		t.Errorf("X-CSRF-TOKEN = %q, want the captured page token", got)
	}
	if got := post.header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q", got)
	}
	if got := post.header.Get("Origin"); got != srv.URL {
		t.Errorf("Origin = %q, want the site root", got)
	}
	if got := post.header.Get("Referer"); got != srv.URL+"/anime/uyyyn4kf" {
		t.Errorf("Referer = %q, want the series page", got)
	}
	if got := post.header.Get("Cookie"); !strings.Contains(got, "anizone_session=az-jar-check") {
		t.Errorf("Cookie = %q, want the jar-replayed session cookie", got)
	}

	// Body: the decoded page-one snapshot, the cursor and the loadPage
	// call — the Livewire wire format (updates is the empty OBJECT the
	// wire format carries).
	var body struct {
		Components []struct {
			Snapshot string         `json:"snapshot"`
			Updates  map[string]any `json:"updates"`
			Calls    []struct {
				Path   string   `json:"path"`
				Method string   `json:"method"`
				Params []string `json:"params"`
			} `json:"calls"`
		} `json:"components"`
	}
	if err := json.Unmarshal(post.body, &body); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if len(body.Components) != 1 {
		t.Fatalf("components = %d, want 1", len(body.Components))
	}
	comp := body.Components[0]
	if comp.Updates == nil {
		t.Error("updates = null, want the empty object the wire format carries")
	}
	// The expected snapshot: the fixture's entity-encoded attribute,
	// HTML-decoded (recipe decodeEntities).
	pageFixture := fixture(t, "anizone_series_paged.html")
	wantSnapshot := azFixtureSnapshot(string(pageFixture))
	if comp.Snapshot != wantSnapshot {
		t.Errorf("posted snapshot = %d bytes, want the decoded fixture snapshot (%d bytes)",
			len(comp.Snapshot), len(wantSnapshot))
	}
	if len(comp.Calls) != 1 || comp.Calls[0].Method != "loadPage" || comp.Calls[0].Path != "" {
		t.Fatalf("calls = %+v, want one loadPage call at path \"\"", comp.Calls)
	}
	if got := comp.Calls[0].Params; len(got) != 1 ||
		got[0] != "eyJzb3J0IjoyNCwiaWQiOjEwNDEsIl9wb2ludHNUb05leHRJdGVtcyI6dHJ1ZX0" {
		t.Errorf("call params = %v, want the captured nextCursor", got)
	}

	// The walk: 24 episodes from page one plus 24 from the captured
	// continuation, contiguous and sorted.
	if len(episodes) != 48 {
		t.Fatalf("episodes = %d, want 48", len(episodes))
	}
	if episodes[23].Num != "24" || episodes[24].Num != "25" {
		t.Errorf("page boundary = %q..%q, want 24..25", episodes[23].Num, episodes[24].Num)
	}
}

func readAllLimit(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

func TestAniZoneGetEpisodesTypedErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing payload", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "<html>challenge page</html>")
		})
		p := azProvider(t, srv.URL)
		_, err := p.GetEpisodes(context.Background(), "x")
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Fatalf("err = %v, want ErrExtractFailed", err)
		}
	})

	t.Run("items without snapshot or csrf", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<script>items: JSON.parse('[]')</script>`)
		})
		p := azProvider(t, srv.URL)
		_, err := p.GetEpisodes(context.Background(), "x")
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Fatalf("err = %v, want ErrExtractFailed (payload requires items+snapshot+csrf)", err)
		}
	})

	t.Run("garbage continuation", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write(fixture(t, "anizone_series_paged.html"))
				return
			}
			_, _ = fmt.Fprint(w, "not json")
		}))
		t.Cleanup(srv.Close)
		p := azProvider(t, srv.URL)
		_, err := p.GetEpisodes(context.Background(), "uyyyn4kf")
		if err == nil {
			t.Fatal("GetEpisodes err = nil, want the continuation error")
		}
		var pe *contracts.ProviderError
		if !errors.As(err, &pe) || pe.Op != contracts.OpGetEpisodes {
			t.Fatalf("err = %v, want a ProviderError op=get_episodes", err)
		}
	})

	t.Run("continuation without dispatch", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write(fixture(t, "anizone_series_paged.html"))
				return
			}
			_, _ = fmt.Fprint(w, `{"components":[{"snapshot":"x","effects":{"dispatches":[]}}]}`)
		}))
		t.Cleanup(srv.Close)
		p := azProvider(t, srv.URL)
		_, err := p.GetEpisodes(context.Background(), "uyyyn4kf")
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Fatalf("err = %v, want ErrExtractFailed", err)
		}
	})
}

func TestAniZoneResolveStream(t *testing.T) {
	t.Parallel()

	m3u8Srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write(fixture(t, "anizone_master.m3u8"))
	})
	watchSrv, watchRec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(rewritePlayerSrc(t, fixture(t, "anizone_watch.html"), m3u8Srv.URL+"/master.m3u8"))
	})
	// The watch page and the playlist hang off DIFFERENT fixture
	// servers here; the script resolves the playlist URL from the
	// decoded payload, so only the watch URL rides the provider base.
	// The playlist fetch leaves the base — point the captured src at
	// the m3u8 server (rewritePlayerSrc already did).
	p := azProvider(t, watchSrv.URL)

	episode := contracts.Episode{
		Num:   "1",
		RawID: luaStateJSONOf(watchSrv.URL+"/anime/a8vfumal/1", "1"),
		RawEmbeds: map[string][]string{
			azDub: {watchSrv.URL + "/anime/a8vfumal/1"},
		},
	}
	stream, err := p.ResolveStream(context.Background(), episode, azDub)
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	if watchRec.Path != "/anime/a8vfumal/1" {
		t.Errorf("watch path = %q", watchRec.Path)
	}
	// The master playlist was fetched and split into its variants.
	if rec.Path != "/master.m3u8" {
		t.Errorf("playlist path = %q", rec.Path)
	}
	if got := rec.Header.Get("Referer"); got != watchSrv.URL {
		t.Errorf("playlist Referer = %q, want the site root", got)
	}

	if stream.DubName != azDub {
		t.Errorf("DubName = %q", stream.DubName)
	}
	for _, height := range []string{"360", "720", "1080"} {
		src, ok := stream.Links[height]
		if !ok {
			t.Errorf("Links[%s] missing (real capture carries it)", height)
			continue
		}
		if src.URL != m3u8Srv.URL+"/video/"+height+"/playlist.m3u8" {
			t.Errorf("Links[%s].URL = %q, want the resolved variant", height, src.URL)
		}
		if src.Quality != height || src.Type != "m3u8" {
			t.Errorf("Links[%s] quality/type = %q/%q", height, src.Quality, src.Type)
		}
		if src.Headers["Referer"] != watchSrv.URL {
			t.Errorf("Links[%s].Headers[Referer] = %v, want the site root", height, src.Headers)
		}
	}
	if len(stream.Links) != 3 {
		t.Errorf("Links = %d entries, want the three captured variants", len(stream.Links))
	}
}

// TestAniZoneResolveStreamFromRawID pins the fresh-sandbox state
// contract: streams() receives raw_id and the dub ONLY — the watch URL
// rides the {n,u} JSON state (the animevost/anilib precedent), so the
// resolution works with empty RawEmbeds too.
func TestAniZoneResolveStreamFromRawID(t *testing.T) {
	t.Parallel()

	m3u8Srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write(fixture(t, "anizone_master.m3u8"))
	})
	watchSrv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(rewritePlayerSrc(t, fixture(t, "anizone_watch.html"), m3u8Srv.URL+"/master.m3u8"))
	})
	p := azProvider(t, watchSrv.URL)

	episode := contracts.Episode{
		Num:   "1",
		RawID: luaStateJSONOf(watchSrv.URL+"/anime/a8vfumal/1", "1"),
	}
	stream, err := p.ResolveStream(context.Background(), episode, azDub)
	if err != nil {
		t.Fatalf("ResolveStream from raw_id alone: %v", err)
	}
	if len(stream.Links) != 3 {
		t.Errorf("Links = %d entries, want the three captured variants", len(stream.Links))
	}
}

func TestAniZoneResolveStreamErrors(t *testing.T) {
	t.Parallel()

	t.Run("no player payload", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "<html>no player</html>")
		})
		p := azProvider(t, srv.URL)
		episode := contracts.Episode{RawID: luaStateJSONOf(srv.URL+"/anime/x/1", "1")}
		_, err := p.ResolveStream(context.Background(), episode, azDub)
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Fatalf("err = %v, want ErrExtractFailed", err)
		}
	})

	t.Run("playlist fetch fails", func(t *testing.T) {
		t.Parallel()
		dead := "http://" + newDeadListener(t).Addr().String() + "/master.m3u8"
		watchSrv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(rewritePlayerSrc(t, fixture(t, "anizone_watch.html"), dead))
		})
		p := azProvider(t, watchSrv.URL)
		episode := contracts.Episode{RawID: luaStateJSONOf(watchSrv.URL+"/anime/x/1", "1")}
		_, err := p.ResolveStream(context.Background(), episode, azDub)
		if err == nil {
			t.Fatal("err = nil, want the playlist transport error")
		}
		var pe *contracts.ProviderError
		if !errors.As(err, &pe) || pe.Op != contracts.OpResolveStream {
			t.Fatalf("err = %v, want a ProviderError op=resolve_stream", err)
		}
	})

	t.Run("empty embeds resolve to an empty stream", func(t *testing.T) {
		t.Parallel()
		// The compiled provider's semantics: no resolvable watch URL →
		// an empty stream, no error (the session skips the row).
		p := azProvider(t, "https://anizone.to")
		stream, err := p.ResolveStream(context.Background(), contracts.Episode{}, azDub)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(stream.Links) != 0 {
			t.Errorf("Links = %v, want empty", stream.Links)
		}
	})
}

func TestAniZoneNamePreference(t *testing.T) {
	t.Parallel()

	p := azProvider(t, "https://anizone.to")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("anizone must implement contracts.NamePreferenceProvider (latin-only index)")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
}

// azFixtureSnapshot extracts the entity-encoded pages.anime-detail
// wire:snapshot attribute from a captured page (test-side mirror of
// the script's own extraction, without the decode).
func azFixtureSnapshot(page string) string {
	const marker = `wire:snapshot="`
	for _, candidate := range strings.Split(page, marker)[1:] {
		end := strings.Index(candidate, `"`)
		if end < 0 {
			continue
		}
		attr := candidate[:end]
		if strings.Contains(attr, "pages.anime-detail") {
			return html.UnescapeString(attr)
		}
	}
	return ""
}
