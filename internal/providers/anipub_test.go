package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// AniPub offline fixture tests. Every anipub_* fixture in testdata is a
// REAL capture (2026-09-25, anonymous GET, apex host anipub.xyz — the
// www host 301s there, the api. subdomain is static GitHub Pages and
// serves no API):
//
//	anipub_search.json      GET /api/searchAll/cowboy%20bebop (3 hits)
//	anipub_search_miss.json GET /api/searchAll/zzzqqqxxx → {"found":false}
//	anipub_details_sub.json GET /v1/api/details/8270 (Cowboy Bebop, 25 eps, /sub links)
//	anipub_details_dub.json GET /v1/api/details/82 (Naruto, 219 eps, /dub links)
//	anipub_video_sub.html   GET /video/850/sub (player page, unquoted megaplay iframe)
//	anipub_video_dub.html   GET /video/12353/dub (www host in the ep link)
//	anipub_megaplay.html    GET megaplay.buzz/stream/s-2/12353/dub (data-id=104085)
//	anipub_sources_sub.json GET megaplay.buzz/stream/getSourcesNew?id=41014&type=sub&s=bcdn
//	anipub_sources_dub.json GET megaplay.buzz/stream/getSourcesNew?id=104085&type=dub&s=bcdn
//
// PR139: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anipub/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixture captures the compiled Go implementation was held to. The
// resolve-chain tests serve the real fixture bytes on a local server,
// rewriting the megaplay origin to it (the script derives the stream
// host from the video page's iframe — megaplay's own client fetches
// stream/getSourcesNew same-origin, so origin-following is the
// faithful shape). The enc fixtures still decrypt to the REAL CDN
// manifest URLs, pinning the AES-256-CBC leg against live bytes. The
// raw_id state channel carries the episode's catalog-flavor /video
// player URL (the sameband/anidub single-value precedent) so the
// fresh-sandbox streams(raw_id, dub) call re-derives the requested
// dub flavor through the site's own changeStreamType grammar.

// anipubRecorder captures the requests the chain issued, mutex-guarded
// (tests run in parallel; each test owns its server and recorder).
type anipubRecorder struct {
	mu       sync.Mutex
	seen     []string
	referers map[string]string // request URI → Referer header
}

func (r *anipubRecorder) add(method, path, referer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, method+" "+path)
	if r.referers == nil {
		r.referers = map[string]string{}
	}
	r.referers[path] = referer
}

func (r *anipubRecorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// refererFor reports the Referer the request to uri carried.
func (r *anipubRecorder) refererFor(uri string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.referers[uri]
}

// anipubTestEnv wires the bundled script against a local server serving
// the given path handlers. The harness rewrites the script's production
// base_url literal onto this server; the video-page fixtures' iframe
// origin rewrites to it at serve time, so the whole resolve chain stays
// offline and the playback Referer (derived from the iframe origin,
// megaplay's same-origin getSourcesNew fetch) asserts against origin.
func anipubTestEnv(t *testing.T, handle func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request)) (contracts.Provider, *anipubRecorder, string) {
	t.Helper()

	rec := &anipubRecorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Method, r.URL.RequestURI(), r.Referer())
		handle(rec, w, r)
	}))
	t.Cleanup(ts.Close)

	return luaProvider(t, "anipub", ts.URL), rec, ts.URL
}

// anipubServeFixture answers with a fixture body (optionally rewritten:
// the video pages swap the real megaplay origin for the test server's).
func anipubServeFixture(t *testing.T, w http.ResponseWriter, name string, rewrite map[string]string) {
	t.Helper()

	body := fixture(t, name)
	for from, to := range rewrite {
		body = []byte(strings.ReplaceAll(string(body), from, to))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// TestAniPubSearchParsesCatalog: the searchAll shape decodes into
// SearchResults carrying the numeric id in URL and the poster.
func TestAniPubSearchParsesCatalog(t *testing.T) {
	t.Parallel()

	p, rec, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/searchAll/cowboy bebop" || r.URL.Path == "/api/searchAll/cowboy%20bebop" {
			anipubServeFixture(t, w, "anipub_search.json", nil)
			return
		}
		http.NotFound(w, r)
	})

	results, err := p.Search(context.Background(), "cowboy bebop")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("Search = %d results, want 3", len(results))
	}
	wantIDs := []string{"1387", "7152", "8270"}
	wantTitles := []string{"Cowboy Bebop: The Movie", "Cowboy Bebop Session XX", "Cowboy Bebop"}
	for i, res := range results {
		if res.URL != wantIDs[i] {
			t.Errorf("result %d URL = %q, want id %q", i, res.URL, wantIDs[i])
		}
		if res.Title != wantTitles[i] {
			t.Errorf("result %d title = %q, want %q", i, res.Title, wantTitles[i])
		}
		if res.SourceID != "anipub" {
			t.Errorf("result %d SourceID = %q", i, res.SourceID)
		}
		if res.Poster == "" {
			t.Errorf("result %d carries no poster", i)
		}
	}
	if reqs := rec.requests(); len(reqs) != 1 || !strings.Contains(reqs[0], "/api/searchAll/") {
		t.Fatalf("unexpected requests: %v", reqs)
	}
}

// TestAniPubSearchMissIsEmpty: the {"found":false} no-hit body is a
// clean empty result, not an error.
func TestAniPubSearchMissIsEmpty(t *testing.T) {
	t.Parallel()

	p, _, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		anipubServeFixture(t, w, "anipub_search_miss.json", nil)
	})

	results, err := p.Search(context.Background(), "zzzqqqxxx")
	if err != nil {
		t.Fatalf("Search miss returned error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search miss = %d results, want 0", len(results))
	}
}

// TestAniPubSearchEncodesQuery: the query is one path segment — spaces
// percent-encode (the live server answers the %20 form).
func TestAniPubSearchEncodesQuery(t *testing.T) {
	t.Parallel()

	var gotPath string
	p, _, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		anipubServeFixture(t, w, "anipub_search_miss.json", nil)
	})

	if _, err := p.Search(context.Background(), "one piece"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := "/api/searchAll/one%20piece"; gotPath != want {
		t.Fatalf("request path = %q, want %q", gotPath, want)
	}
}

// TestAniPubGetEpisodesSub: the Cowboy Bebop details decode into 25
// episodes; the catalog flavor (/sub) lands on the Sub row and the
// complementary Dub row is the site's own changeStreamType rewrite.
// raw_id carries the catalog-flavor player URL (the fresh-sandbox
// state channel).
func TestAniPubGetEpisodesSub(t *testing.T) {
	t.Parallel()

	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/details/8270" {
			anipubServeFixture(t, w, "anipub_details_sub.json", nil)
			return
		}
		http.NotFound(w, r)
	})

	eps, err := p.GetEpisodes(context.Background(), "8270")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 25 {
		t.Fatalf("GetEpisodes = %d episodes, want 25", len(eps))
	}
	first, last := eps[0], eps[24]
	if first.Num != "1" || first.RawID != base+"/video/850/sub" {
		t.Errorf("first episode = (Num %q, RawID %q), want (1, %s/video/850/sub)", first.Num, first.RawID, base)
	}
	if last.Num != "25" {
		t.Errorf("last episode Num = %q, want 25", last.Num)
	}
	subs := first.RawEmbeds["Sub"]
	dubs := first.RawEmbeds["Dub"]
	if len(subs) != 1 || subs[0] != base+"/video/850/sub" {
		t.Errorf("Sub row = %v, want [%s/video/850/sub]", subs, base)
	}
	if len(dubs) != 1 || dubs[0] != base+"/video/850/dub" {
		t.Errorf("Dub row = %v, want [%s/video/850/dub]", dubs, base)
	}
}

// TestAniPubGetEpisodesDub: the Naruto details decode into 219 episodes
// whose catalog flavor is /dub (www-host link accepted, normalized onto
// the provider base).
func TestAniPubGetEpisodesDub(t *testing.T) {
	t.Parallel()

	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/details/82" {
			anipubServeFixture(t, w, "anipub_details_dub.json", nil)
			return
		}
		http.NotFound(w, r)
	})

	eps, err := p.GetEpisodes(context.Background(), "82")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 219 {
		t.Fatalf("GetEpisodes = %d episodes, want 219", len(eps))
	}
	first := eps[0]
	if first.RawID != base+"/video/12353/dub" {
		t.Errorf("first RawID = %q, want %s/video/12353/dub", first.RawID, base)
	}
	dubs := first.RawEmbeds["Dub"]
	if len(dubs) != 1 || dubs[0] != base+"/video/12353/dub" {
		t.Errorf("Dub row = %v, want [%s/video/12353/dub]", dubs, base)
	}
	if subs := first.RawEmbeds["Sub"]; len(subs) != 1 || subs[0] != base+"/video/12353/sub" {
		t.Errorf("Sub row = %v, want the complementary rewrite", subs)
	}
}

// TestAniPubGetEpisodesMovieDocLink: movie/special releases carry an
// EMPTY ep array with the stream on the doc-level link — the single-
// episode shape (live capture: Cowboy Bebop: The Movie, id 1387,
// /video/74019/sub).
func TestAniPubGetEpisodesMovieDocLink(t *testing.T) {
	t.Parallel()

	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/details/1387" {
			anipubServeFixture(t, w, "anipub_details_movie.json", nil)
			return
		}
		http.NotFound(w, r)
	})

	eps, err := p.GetEpisodes(context.Background(), "1387")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("GetEpisodes = %d episodes, want 1", len(eps))
	}
	first := eps[0]
	if first.Num != "1" || first.RawID != base+"/video/74019/sub" {
		t.Errorf("episode = (Num %q, RawID %q), want (1, %s/video/74019/sub)", first.Num, first.RawID, base)
	}
	subs := first.RawEmbeds["Sub"]
	if len(subs) != 1 || subs[0] != base+"/video/74019/sub" {
		t.Errorf("Sub row = %v, want [%s/video/74019/sub]", subs, base)
	}
}

// TestAniPubGetEpisodesNotFound: the API's 404 {"error":...} surfaces
// through the netclient status map as contracts.ErrNotFound.
func TestAniPubGetEpisodesNotFound(t *testing.T) {
	t.Parallel()

	p, _, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Anime not found"}`))
	})

	_, err := p.GetEpisodes(context.Background(), "99999999")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("GetEpisodes 404 err = %v, want contracts.ErrNotFound", err)
	}
}

// TestAniPubGetEpisodesNoEpisodes: a 200 body without parseable episode
// links fails typed, not silently empty.
func TestAniPubGetEpisodesNoEpisodes(t *testing.T) {
	t.Parallel()

	p, _, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"local":{"_id":7,"ep":[{"link":""}]}}`))
	})

	_, err := p.GetEpisodes(context.Background(), "7")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("GetEpisodes empty-body err = %v, want contracts.ErrNotFound", err)
	}
}

// TestAniPubResolveStreamSubChain: the full (video page → megaplay page
// → getSourcesNew → AES decrypt) chain resolves the REAL manifest URL
// pinned in the live enc capture, with the megaplay Referer on the
// source and the s=bcdn + type=sub query.
func TestAniPubResolveStreamSubChain(t *testing.T) {
	t.Parallel()

	rewrite := map[string]string{}
	p, rec, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/details/8270":
			anipubServeFixture(t, w, "anipub_details_sub.json", nil)
		case "/video/850/sub":
			anipubServeFixture(t, w, "anipub_video_sub.html", rewrite)
		case "/stream/s-2/850/sub":
			anipubServeFixture(t, w, "anipub_megaplay_sub.html", rewrite)
		case "/stream/getSourcesNew":
			if r.URL.Query().Get("id") != "41014" || r.URL.Query().Get("type") != "sub" ||
				r.URL.Query().Get("s") != "bcdn" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			anipubServeFixture(t, w, "anipub_sources_sub.json", nil)
		default:
			http.NotFound(w, r)
		}
	})
	rewrite["https://megaplay.buzz"] = base

	eps, err := p.GetEpisodes(context.Background(), "8270")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), eps[0], "Sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Sub" {
		t.Errorf("DubName = %q, want Sub", stream.DubName)
	}
	src, ok := stream.Links["auto"]
	if !ok {
		t.Fatalf("no auto quality link: %v", stream.Links)
	}
	const wantManifest = "https://fetch.nexabloom.top/anime/02e74f10e0327ad868d138f2b4fdd6f0/" +
		"d9a3d34b532ec6c522713c69e71774f3/master.m3u8"
	if src.URL != wantManifest {
		t.Errorf("manifest = %q, want %q", src.URL, wantManifest)
	}
	if src.Type != "m3u8" || src.Quality != "auto" {
		t.Errorf("source type/quality = %q/%q, want m3u8/auto", src.Type, src.Quality)
	}
	if src.Headers["Referer"] != base+"/" {
		t.Errorf("Referer = %q, want the iframe origin %s/", src.Headers["Referer"], base)
	}
	// Megaplay hotlink-gates the stream page on the embedding site's
	// Referer (live-verified 2026-09-25: no Referer → its Error page).
	if got := rec.refererFor("/stream/s-2/850/sub"); got != base+"/" {
		t.Errorf("stream page Referer = %q, want %s/", got, base)
	}
}

// TestAniPubResolveStreamDubChain: the dub flavor walks the same chain
// with type=dub and its own enc capture.
func TestAniPubResolveStreamDubChain(t *testing.T) {
	t.Parallel()

	rewrite := map[string]string{}
	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/api/details/82":
			anipubServeFixture(t, w, "anipub_details_dub.json", nil)
		case r.URL.Path == "/video/12353/dub":
			anipubServeFixture(t, w, "anipub_video_dub.html", rewrite)
		case r.URL.Path == "/stream/s-2/12353/dub":
			anipubServeFixture(t, w, "anipub_megaplay.html", rewrite)
		case r.URL.Path == "/stream/getSourcesNew" &&
			r.URL.Query().Get("type") == "dub" && r.URL.Query().Get("id") == "104085":
			anipubServeFixture(t, w, "anipub_sources_dub.json", nil)
		default:
			http.NotFound(w, r)
		}
	})
	rewrite["https://megaplay.buzz"] = base

	eps, err := p.GetEpisodes(context.Background(), "82")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), eps[0], "Dub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	const wantManifest = "https://fetch.nexabloom.top/anime/71a3cb155f8dc89bf3d0365288219936/" +
		"b6c6e323e9d5c75f53cd0e05afed4535/master.m3u8"
	if got := stream.Links["auto"].URL; got != wantManifest {
		t.Errorf("manifest = %q, want %q", got, wantManifest)
	}
}

// TestAniPubResolveStreamUnknownDub: a dub the episode does not carry is
// the typed not-found (the raw_id state is synthesized per the
// documented state channel).
func TestAniPubResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p, _, _ := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	episode := contracts.Episode{Num: "1", RawID: "https://anipub.xyz/video/850/sub",
		RawEmbeds: map[string][]string{"Sub": {"https://anipub.xyz/video/850/sub"}}}
	_, err := p.ResolveStream(context.Background(), episode, "AniLibria")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("unknown dub err = %v, want contracts.ErrNotFound", err)
	}
}

// TestAniPubResolveStreamNoIframe: a video page without its player
// iframe is the typed extract wall.
func TestAniPubResolveStreamNoIframe(t *testing.T) {
	t.Parallel()

	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>no player</body></html>"))
	})

	episode := contracts.Episode{Num: "1", RawID: base + "/video/850/sub",
		RawEmbeds: map[string][]string{"Sub": {base + "/video/850/sub"}}}
	_, err := p.ResolveStream(context.Background(), episode, "Sub")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("no-iframe err = %v, want contracts.ErrExtractFailed", err)
	}
}

// TestAniPubResolveStreamEncMissing: a getSourcesNew answer without the
// enc payload (wrong CDN selector, unknown type) fails typed.
func TestAniPubResolveStreamEncMissing(t *testing.T) {
	t.Parallel()

	rewrite := map[string]string{}
	p, _, base := anipubTestEnv(t, func(rec *anipubRecorder, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/details/8270":
			anipubServeFixture(t, w, "anipub_details_sub.json", nil)
		case "/video/850/sub":
			anipubServeFixture(t, w, "anipub_video_sub.html", rewrite)
		case "/stream/s-2/850/sub":
			anipubServeFixture(t, w, "anipub_megaplay_sub.html", rewrite)
		default:
			_, _ = w.Write([]byte(`{"tracks":[],"t":1}`))
		}
	})
	rewrite["https://megaplay.buzz"] = base

	eps, err := p.GetEpisodes(context.Background(), "8270")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	_, err = p.ResolveStream(context.Background(), eps[0], "Sub")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("enc-missing err = %v, want contracts.ErrExtractFailed", err)
	}
}

// TestAniPubDeclarations: the EN catalog speaks NamePrefLatin (the
// mongo Name index matches latin titles only) and declares its own
// smoke query — the shared RU probe misses this catalog entirely.
func TestAniPubDeclarations(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anipub")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the capability adapter must expose SmokeQuery")
	}
	if got := sq.SmokeQuery(); got != "cowboy bebop" {
		t.Errorf("SmokeQuery = %q, want %q", got, "cowboy bebop")
	}
}
