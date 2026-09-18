package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestAnilibSearch(t *testing.T) {
	t.Parallel()

	// The PR53 preflight rides /episodes after the search; only the
	// /anime request is the search itself, so the recording happens
	// inside the /anime leg and the mux serves the preflight chain
	// (episode lists of the four fixture releases + their sorted-first
	// episode id 13's players detail — all with dubs, all kept).
	var rec *recordedRequest
	var recMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/anime" {
			recMu.Lock()
			rec = &recordedRequest{
				Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
				Header: r.Header.Clone(), Form: r.PostForm,
			}
			recMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "anilib_search.json"))
			return
		}
		switch r.URL.Path {
		case "/episodes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "anilib_episodes.json"))
		case "/episodes/13":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "anilib_episode_players.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec == nil || rec.Path != "/anime" {
		t.Fatalf("search request not recorded as /anime: %+v", rec)
	}
	// [LIVE-VERIFIED 2026-09-13] the browser-shaped parameter list the API
	// accepts: q, limit=20 and site_id=5. The legacy fields[] entries and
	// the site_id[] array form are REJECTED with HTTP 422 ("The selected
	// value for fields.N is incorrect") — verified against
	// api.cdnlibs.org/api/anime.
	params, err := url.ParseQuery(rec.Query)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", rec.Query, err)
	}
	if got := params["q"]; len(got) != 1 || got[0] != "naruto" {
		t.Errorf("q = %v, got", got)
	}
	if got := params["limit"]; len(got) != 1 || got[0] != "20" {
		t.Errorf("limit = %v, want 20", got)
	}
	if got := params["site_id"]; len(got) != 1 || got[0] != "5" {
		t.Errorf("site_id = %v, want [5]", got)
	}
	if _, ok := params["site_id[]"]; ok {
		t.Error("site_id[] must not be sent (API rejects the array form)")
	}
	if _, ok := params["fields[]"]; ok {
		t.Error("fields[] must not be sent (API rejects the field selector)")
	}

	// Fixture: three real entries from GET /anime?q=naruto&limit=20&site_id=5
	// [LIVE-VERIFIED 2026-09-13] plus one modeled null-chain entry.
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].Title != "Наруто" {
		t.Errorf("Title = %q, want rus_name preference", results[0].Title)
	}
	if results[0].URL != "11--naruto-anime" {
		t.Errorf("URL = %q, want slug_url", results[0].URL)
	}
	if results[0].SourceID != "anilib" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != "https://cover.cdnlibs.org/uploads/anime/11/cover/bbf77bf3-26e9-40ad-b21b-57a6b90a4b0d.jpg" {
		t.Errorf("Poster = %q, want live cover.default", results[0].Poster)
	}
	if results[1].Title != "Наруто: Ураганные хроники" {
		t.Errorf("Title = %q, want second live rus_name", results[1].Title)
	}
	if results[3].Title != "Eng Only Title" {
		t.Errorf("Title = %q, want eng_name fallback", results[3].Title)
	}
	if results[3].Poster != "" {
		t.Errorf("Poster = %q, want empty when cover.default missing", results[3].Poster)
	}
}

// anilibMuxFixtureServer routes the three live API shapes the search
// preflight touches: /anime (search answers), /episodes?anime_id=N
// (episode lists), /episodes/{id} (episode detail with players).
// Every hit is appended to *paths so tests can pin the preflight's
// exact request sequence; the append is mutex-guarded because the
// preflight fans out concurrently.
func anilibMuxFixtureServer(t *testing.T, mux map[string][]byte, paths *[]string) string {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			mu.Lock()
			*paths = append(*paths, r.URL.Path+"?"+r.URL.RawQuery)
			mu.Unlock()
		}
		body, ok := mux[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			body = mux[r.URL.Path]
		}
		if body == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAnilibSearchFiltersContentlessReleases pins the PR53 owner
// ruling: contentless catalog entries (the CM/«Реклама» placeholders
// whose episode detail carries an EMPTY players list) are dropped from
// Search, never surfaced as dead results. The filter keys on the API's
// own data property — first episode's players[] — exactly like the
// PR44 release-dub model hydrates them, never on a title string.
//
// Fixture chain (all [LIVE-VERIFIED 2026-09-18]): the real 5-entry
// black lagoon search; release 25322 «…Реклама» resolves to episode
// 141970 whose players list is empty (dropped); the four real
// releases resolve to the shared episodes fixture whose sorted-first
// episode (id 13, the null-numbered one — pythonFloatKey parity with
// GetEpisodes) carries players (kept).
func TestAnilibSearchFiltersContentlessReleases(t *testing.T) {
	t.Parallel()

	var paths []string
	base := anilibMuxFixtureServer(t, map[string][]byte{
		"/anime":                   fixture(t, "anilib_search_black_lagoon.json"),
		"/episodes?anime_id=25322": fixture(t, "anilib_episodes_cm.json"),
		"/episodes/141970":         fixture(t, "anilib_episode_players_cm.json"),
		"/episodes?anime_id=805":   fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=1343":  fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=3864":  fixture(t, "anilib_episodes.json"),
		"/episodes?anime_id=5317":  fixture(t, "anilib_episodes.json"),
		"/episodes/13":             fixture(t, "anilib_episode_players.json"),
	}, &paths)
	p := newAnilib(base, testClient(t, "anilib"))

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// 5 live entries → 4 kept: the CM placeholder (empty players) is
	// gone, feed order preserved.
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (the CM entry dropped)", len(results))
	}
	for _, r := range results {
		if strings.Contains(r.Title, "Реклама") {
			t.Errorf("result %q surfaced: the contentless CM entry must be dropped", r.Title)
		}
	}
	if results[0].Title != "Пираты «Чёрной лагуны»" {
		t.Errorf("first title = %q, want feed order preserved", results[0].Title)
	}

	// The preflight rode the same endpoints the PR44 dub model uses:
	// episode list of the release, then the sorted-first episode's
	// detail. Spot-check the CM release's request pair.
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/episodes?anime_id=25322") {
		t.Errorf("preflight never listed release 25322: %v", paths)
	}
	if !strings.Contains(joined, "/episodes/141970") {
		t.Errorf("preflight never fetched the first episode's players: %v", paths)
	}
}

// TestAnilibSearchPreflightFailureFailsOpen: a preflight probe that
// FAILS (transport, non-200) is absence of evidence, not evidence of a
// contentless release — the result is kept. The filter drops only on a
// positively-empty players list; a flaky API must not empty the search.
func TestAnilibSearchPreflightFailureFailsOpen(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anime" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "anilib_search_black_lagoon.json"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("results = %d, want all 5 kept (probe errors fail open)", len(results))
	}
}

func TestAnilibSearchQueryUnquoteSemantics(t *testing.T) {
	t.Parallel()

	// Python sends ("q", unquote(query)) (anilib.py:50): the query is
	// percent-DECODED first, then re-encoded by the request layer.
	// unquote leaves a literal "+" untouched, so requests puts q=foo%2Bbar
	// on the wire. The Go twin of unquote is url.PathUnescape (Query-
	// Unescape would decode "+" to a space).
	tests := []struct {
		name  string
		query string
		wantQ string
	}{
		{name: "literal plus stays plus", query: "foo+bar", wantQ: "foo+bar"},
		{name: "percent-encoded plus decodes to literal plus", query: "bleach%2Bmovie", wantQ: "bleach+movie"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, `{"data": []}`)
			})
			p := newAnilib(srv.URL, testClient(t, "anilib"))

			if _, err := p.Search(context.Background(), tt.query); err != nil {
				t.Fatalf("Search: %v", err)
			}

			params, err := url.ParseQuery(rec.Query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", rec.Query, err)
			}
			if got := params["q"]; len(got) != 1 || got[0] != tt.wantQ {
				t.Errorf("server-side q = %v, want [%q]", got, tt.wantQ)
			}
		})
	}
}

func TestAnilibSearchSendsSiteHeaders(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data": []}`)
	})
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The load-bearing header set from anilib.py:27-41.
	for header, want := range map[string]string{
		"Authority":          "api.cdnlibs.org",
		"Origin":             "https://animelib.me",
		"Referer":            "https://animelib.me/",
		"Accept":             "application/json, text/plain, */*",
		"Sec-Ch-Ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "cross-site",
	} {
		if got := rec.Header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
}

func TestAnilibSearchHTTPErrorReturnsEmpty(t *testing.T) {
	t.Parallel()

	// Python wraps the whole search call in `except Exception: return []`
	// (anilib.py:66-70): HTTP failures surface as an empty result set,
	// not an error. Ported verbatim and documented as a Python quirk.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search on HTTP error = %v, want nil (Python returns [])", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAnilibGetEpisodes(t *testing.T) {
	t.Parallel()

	// Route-aware stub: the episodes list, then the PR44 tier-1 dub-
	// list fetch of the FIRST episode's players.
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paths = append(paths, r.URL.RequestURI())
		switch {
		case strings.HasPrefix(r.URL.Path, "/episodes/"):
			_, _ = w.Write(fixture(t, "anilib_episode_players.json"))
		default:
			_, _ = w.Write(fixture(t, "anilib_episodes.json"))
		}
	}))
	t.Cleanup(srv.Close)
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	episodes, err := p.GetEpisodes(context.Background(), "16488--bleach-sennen-kessen-hen")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The request sequence proves the numeric id was extracted from
	// the slug prefix (anilib.py:91): the list rode ?anime_id=16488,
	// the tier-1 dub-list fetch hit the first episode's detail.
	if len(paths) != 2 || paths[0] != "/episodes?anime_id=16488" || paths[1] != "/episodes/13" {
		t.Errorf("requests = %v, want the list fetch then the tier-1 /episodes/13", paths)
	}

	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want 4", len(episodes))
	}
	// Numeric ascending sort with the Python quirks (anilib.py:114):
	// the null-numbered episode renders as "None" and sorts with key 0 —
	// verified against the Python oracle: ['None', '0.5', '1', '2'].
	if episodes[0].Num != "None" || episodes[0].RawID != "13" {
		t.Errorf("episodes[0] = %q/%q, want None/13 (key 0 sorts first)", episodes[0].Num, episodes[0].RawID)
	}
	if episodes[1].Num != "0.5" {
		t.Errorf("episodes[1].Num = %q, want 0.5", episodes[1].Num)
	}
	if episodes[2].Num != "1" || episodes[3].Num != "2" {
		t.Errorf("episode order = %q, %q", episodes[2].Num, episodes[3].Num)
	}
	if episodes[1].Title != "Episode" {
		t.Errorf("episodes[1].Title = %q, want Episode fallback for null name", episodes[1].Title)
	}
	if episodes[2].RawID != "11" {
		t.Errorf("episodes[2].RawID = %q", episodes[2].RawID)
	}
	// The release's dub list (tier-1) rides every episode; episode one
	// keeps its real links.
	if len(episodes[1].RawEmbeds) != 2 {
		t.Errorf("RawEmbeds = %v, want the release dub keys", episodes[1].RawEmbeds)
	}
	if links := episodes[1].RawEmbeds["AniLib (AnimeLib)"]; links == nil || len(links) != 0 {
		t.Errorf("episode 0.5 AniLib links = %v, want an empty list (on-demand resolve)", links)
	}
	if len(episodes[0].RawEmbeds["AniLib (AnimeLib)"]) == 0 {
		t.Errorf("episode one embeds = %v, want the real links from the tier-1 fetch", episodes[0].RawEmbeds)
	}
}

func TestAnilibFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilib_episode_players.json"))
	})
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	episode := contracts.Episode{Num: "1", RawID: "11", RawEmbeds: map[string][]string{}}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if got != &episode {
		t.Fatal("FetchDubs must return the same episode pointer")
	}
	if rec.Path != "/episodes/11" {
		t.Errorf("request path = %q, want /episodes/11", rec.Path)
	}

	embeds := episode.RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("embeds = %v, want 2 dubs", embeds)
	}
	internal, ok := embeds["AniLib (AnimeLib)"]
	if !ok {
		t.Fatalf("embeds = %v, want key 'AniLib (AnimeLib)' (team + player)", embeds)
	}
	if len(internal) != 1 {
		t.Fatalf("internal embed = %v, want one payload", internal)
	}
	if !strings.HasPrefix(internal[0], "internal:") {
		t.Errorf("internal embed = %q, want internal: prefix", internal[0])
	}
	kodik, ok := embeds["Studio Band (Kodik)"]
	if !ok || len(kodik) != 1 || kodik[0] != "//kodik.info/serial/12345/xyz/720p" {
		t.Errorf("kodik embed = %v", embeds["Studio Band (Kodik)"])
	}
}

func TestAnilibResolveStreamInternal(t *testing.T) {
	t.Parallel()

	p := newAnilib(AnilibAPIBase, testClient(t, "anilib"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "11",
		RawEmbeds: map[string][]string{
			"AniLib (AnimeLib)": {`internal:{"quality":[{"href":"bleach/ep1_1080.m3u8","quality":1080},{"href":"bleach/ep1_720.m3u8","quality":720},{"href":"bleach/ep1_default.m3u8"}]}`},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AniLib (AnimeLib)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "AniLib (AnimeLib)" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	// Python dict semantics: the third entry (missing quality defaults to
	// 1080, anilib.py:151) OVERWRITES the first 1080 entry, leaving two
	// distinct keys.
	if len(stream.Links) != 2 {
		t.Fatalf("Links = %v, want 2 (default-1080 overwrites the first 1080)", stream.Links)
	}

	sd, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want 720", stream.Links)
	}
	// The video1.cdnlibs.org path construction quirk: the literal
	// "/.%D0%B0s/" segment from anilib.py:153.
	if sd.URL != "https://video1.cdnlibs.org/.%D0%B0s/bleach/ep1_720.m3u8" {
		t.Errorf("720 URL = %q", sd.URL)
	}
	if sd.Quality != "720" {
		t.Errorf("720 Quality = %q", sd.Quality)
	}
	if sd.Headers["Referer"] != "https://v3.animelib.org" {
		t.Errorf("Referer = %q, want https://v3.animelib.org", sd.Headers["Referer"])
	}

	// The overwritten 1080 entry carries the default-1080 URL.
	hd, ok := stream.Links["1080"]
	if !ok || hd.URL != "https://video1.cdnlibs.org/.%D0%B0s/bleach/ep1_default.m3u8" {
		t.Errorf("1080 entry = %+v, want the default-1080 overwrite", hd)
	}
}

// TestAnilibResolveStreamKodikRoundTrip covers the Kodik-player branch of
// anilib resolve (anilib.py:160-162) with the ported kodik extractor:
// the embed URL runs through the factory and yields the /ftor sources.
func TestAnilibResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	p := newAnilib(AnilibAPIBase, testClient(t, "anilib"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"Studio Band (Kodik)": {srv.URL + "/kodik/serial/12345/xyz/720p"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Studio Band (Kodik)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if src, ok := stream.Links["720"]; !ok || src.URL != "https://plain.example/x/720.m3u8" {
		t.Errorf("720 = %+v, ok=%v, want the kodik extractor result", src, ok)
	}
}

// TestAnilibResolveStreamProtocolRelativeKodik pins the "//" prefix
// normalization (anilib.py:161) against a dead endpoint: the absolutized
// https URL fails transport-side and the extractor-tagged error surfaces.
func TestAnilibResolveStreamProtocolRelativeKodikFailsLoud(t *testing.T) {
	t.Parallel()

	p := newAnilib(AnilibAPIBase, testClient(t, "anilib"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"D (Kodik)": {"//" + newDeadListener(t).Addr().String() + "/kodik/e/9"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "D (Kodik)")
	if err == nil {
		t.Fatal("error = nil, want the transport failure of the absolutized https URL")
	}
	if !strings.Contains(err.Error(), "extractor:kodik") {
		t.Errorf("error = %v, want extractor:kodik context on the transport failure", err)
	}
}

func TestAnilibResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newAnilib(AnilibAPIBase, testClient(t, "anilib"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"AniLib (AnimeLib)": {`internal:{"quality":[]}`},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty", stream.Links)
	}
}

func TestAnilibProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnilib(AnilibAPIBase, testClient(t, "anilib"))
	if p.ID() != "anilib" || p.Name() != "AnimeLib" || p.BaseURL() != AnilibAPIBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
