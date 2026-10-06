package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PR81 review #1: the ten roster providers missing from the first
// benchmark round. Same contract as providers_bench_test.go: loopback
// fixture servers replaying live captures, inputs prebuilt before
// `for b.Loop()`, results sunk into package vars.

// benchRouter serves per-path bodies (for providers whose Search or
// GetEpisodes fans out over several endpoints); unmatched paths get
// the fallback body.
func benchRouter(b *testing.B, fallback []byte, routes map[string][]byte) *httptest.Server {
	b.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for prefix, body := range routes {
			if len(r.URL.Path) >= len(prefix) && r.URL.Path[:len(prefix)] == prefix {
				_, _ = w.Write(body)
				return
			}
		}
		_, _ = w.Write(fallback)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// --- animevost ---

// BenchmarkAnimevostSearchJSON — animevost POST /search decode (the
// Lua script, the PR119 migration's animedia benchmark pattern).
func BenchmarkAnimevostSearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animevost_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "animevost", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("animevost search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnimevostGetEpisodes — the playlist decode.
func BenchmarkAnimevostGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animevost_playlist.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "animevost", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "326")
		if err != nil {
			b.Fatalf("animevost episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// --- gogoanime ---

// BenchmarkGogoanimeSearchJSON — gogoanime search decode (the PR119
// migration's benchmark pattern: the bundled script drives the same
// fixture through the SDK).
func BenchmarkGogoanimeSearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "gogoanime_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "gogoanime", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("gogoanime search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkGogoanimeGetEpisodesHTML — the series-page walk plus the
// eager per-episode mirror hydration (episode roster extraction with
// reverse-descending numbers like 1178; the batch legs ride the
// router's episode body).
func BenchmarkGogoanimeGetEpisodesHTML(b *testing.B) {
	b.ReportAllocs()
	series := benchFixture(b, "gogoanime_series.html")
	episode := benchFixture(b, "gogoanime_episode.html")
	srv := benchRouter(b, episode, map[string][]byte{"/series/": series})
	p := luaProvider(b, "gogoanime", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, srv.URL+"/series/one-piece/")
		if err != nil {
			b.Fatalf("gogoanime episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// --- sameband ---

// BenchmarkSamebandSearchHTML — sameband HTML search parse (the Lua
// script's search leg; PR131 harness).
func BenchmarkSamebandSearchHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "sameband_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "sameband", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("sameband search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- kodik ---

// BenchmarkKodikSearchJSON — kodik token'd search decode (the Lua
// script's search leg; the PR140 harness with the settings seam —
// the script fails loud without a token).
func BenchmarkKodikSearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "kodik_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProviderWithSettings(b, "kodik", srv.URL, map[string]string{"token": "bench-token"})
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "naruto")
		if err != nil {
			b.Fatalf("kodik search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- anidub ---

// BenchmarkAnidubSearchHTML — anidub HTML search parse (the Lua
// script's search leg; PR132 harness).
func BenchmarkAnidubSearchHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anidub_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "anidub", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "блич")
		if err != nil {
			b.Fatalf("anidub search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnidubGetEpisodesHTML — the anime-page episode/dub roster
// (the Lua script's episodes leg; PR132 harness).
func BenchmarkAnidubGetEpisodesHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anidub_anime.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "anidub", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, srv.URL+"/12254-blich.html")
		if err != nil {
			b.Fatalf("anidub episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// --- animedia ---

// BenchmarkAniMediaSearchHTML — animedia HTML search parse (the Lua
// script's search leg; PR116 harness).
func BenchmarkAniMediaSearchHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animedia_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "animedia", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("animedia search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- anistar ---

// BenchmarkAniStarSearchHTML — the 53K DLE full-search page (the
// heaviest HTML search payload on the roster; the Lua script's search
// leg, PR138 harness).
func BenchmarkAniStarSearchHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anistar_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "anistar", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "наруто")
		if err != nil {
			b.Fatalf("anistar search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- anilibria-torrent ---

// newBenchAnilibriaTorrent — the b-variant of the package fixture
// constructor (engine nil: search never touches it).
func newBenchAnilibriaTorrent(b *testing.B, baseURL string) *AniLibriaTorrent {
	b.Helper()
	return newAnilibriaTorrent(baseURL, benchClient(b, "anilibria-torrent"), nil)
}

// BenchmarkAnilibriaTorrentSearch — the release-list expansion search:
// search JSON, then per-release torrent lists (the two-step loader).
func BenchmarkAnilibriaTorrentSearch(b *testing.B) {
	b.ReportAllocs()
	search := benchFixture(b, "anilibria_search.json")
	release := benchFixture(b, "anilibria-torrent_release.json")
	srv := benchRouter(b, []byte(`[]`), map[string][]byte{
		"/app/search/releases":         search,
		"/anime/torrents/release/9789": release,
	})
	p := newBenchAnilibriaTorrent(b, srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "dandadan")
		if err != nil {
			b.Fatalf("anilibria-torrent search: %v", err)
		}
		benchSinkN = len(results)
	}
}
