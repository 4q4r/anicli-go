package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// PR81 offline benchmarks for the provider hot paths. Every benchmark
// is fixture-driven: the HTTP providers run against a loopback
// httptest server replaying a live capture (no external network), the
// separable parsers are called directly on fixture bytes. Inputs are
// built BEFORE `for b.Loop()` (auto-excluded from timing); results
// sink into package-level vars so the compiler cannot dead-code the
// loop body (go.dev testing.B.Loop contract + sinks).

// benchClient is the *testing.B twin of the package testClient helper.
func benchClient(b *testing.B, providerID string) *netclient.Client {
	b.Helper()

	cfg := config.Default().Network
	cfg.ProxyURL = ""
	c, err := netclient.New(cfg, netclient.WithProvider(providerID))
	if err != nil {
		b.Fatalf("netclient.New(%s): %v", providerID, err)
	}
	return c
}

// benchFixture loads a live-capture fixture from testdata.
func benchFixture(b *testing.B, name string) []byte {
	b.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // trusted testdata path
	if err != nil {
		b.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// benchFixtureServer serves one fixture body on EVERY path/method —
// the benchmark does not assert protocol details, only parse cost.
func benchFixtureServer(b *testing.B, body []byte, contentType string) *httptest.Server {
	b.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// benchSinkResults keeps Search/GetEpisodes results alive.
var benchSinkResults []any

// --- hdrezka page + anubis PoW ---

// BenchmarkHDRezkaPageParse parses the live series-page capture (the
// episode/dub roster extraction).
func BenchmarkHDRezkaPageParse(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "hdrezka_anime_series.html")
	var sink *hdrezkaPage
	for b.Loop() {
		page, err := parseHDRezkaPage(body)
		if err != nil {
			b.Fatalf("parse page: %v", err)
		}
		sink = page
	}
	benchHDRezkaSink = sink
}

var benchHDRezkaSink *hdrezkaPage

// buildBenchAnubisChallenge extracts the challenge JSON from the live
// capture and clamps the difficulty to 2 (bounded: ~256 sha256 hashes
// on average — microsecond scale, deterministic enough for a bench).
func buildBenchAnubisChallenge(b *testing.B, difficulty int) hdrezkaAnubisChallenge {
	b.Helper()

	m := hdrezkaAnubisChallengeRe.FindSubmatch(benchFixture(b, "hdrezka_anubis_challenge.html"))
	if m == nil {
		b.Fatal("fixture does not carry an anubis_challenge script")
	}
	var ch hdrezkaAnubisChallenge
	if err := json.Unmarshal(m[1], &ch); err != nil {
		b.Fatalf("decode anubis challenge: %v", err)
	}
	ch.Rules.Algorithm = "fast"
	ch.Rules.Difficulty = difficulty
	return ch
}

// BenchmarkHDRezkaAnubisPoWD2 solves a difficulty-2 anubis proof of
// work — the per-page-fetch cost when the gate engages.
func BenchmarkHDRezkaAnubisPoWD2(b *testing.B) {
	b.ReportAllocs()
	ch := buildBenchAnubisChallenge(b, 2)
	for b.Loop() {
		nonce, digest, err := solveHDRezkaAnubis(ch)
		if err != nil {
			b.Fatalf("solve: %v", err)
		}
		benchSinkPoW = nonce + len(digest)
	}
}

var benchSinkPoW int

// --- kickassanime episode walk ---

// BenchmarkKaaGetEpisodes drives the bundled Lua script's episode walk
// (show routing, first page, follow-up fan-out and the eager
// per-episode hydration) over the live captures. PR129: the bundled
// Lua script is the production path — the bench drives it (the shiza
// precedent), the compiled pageEpisodes conversion bench is gone with
// the Go file.
func BenchmarkKaaGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	show := benchFixture(b, "kickassanime_show.json")
	episodes := benchFixture(b, "kickassanime_episodes.json")
	servers := benchFixture(b, "kickassanime_servers.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/episodes"):
			_, _ = w.Write(episodes)
		case strings.Contains(r.URL.Path, "/episode/"):
			_, _ = w.Write(servers)
		default:
			_, _ = w.Write(show)
		}
	}))
	b.Cleanup(srv.Close)
	p := luaProvider(b, "kickassanime", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "dandadan-da3b")
		if err != nil {
			b.Fatalf("kaa episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// --- SequenceMatcher similarity (rehydrate matching) ---

// BenchmarkSimilarityRatio benchmarks the CPython SequenceMatcher
// ratio port on a realistic title pair — the hot comparator of the
// TUI's history rehydration matching.
func BenchmarkSimilarityRatio(b *testing.B) {
	b.ReportAllocs()
	a, c := "ван пис", "one piece wan pisu tv"
	var sink float64
	for b.Loop() {
		sink = SimilarityRatio(a, c)
	}
	benchSinkRatio = sink
}

var benchSinkRatio float64

// --- loopback Search benches (full provider parse path, no TLS/site) ---

// BenchmarkAnilibSearch — anilib (anilibria.tv JSON API search card
// list) over the live Black Lagoon capture. PR122: the bundled Lua
// script is the production path — the bench drives it (the sequential
// contentless preflight rides the same stub).
func BenchmarkAnilibSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anilib_search_black_lagoon.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "anilib", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("anilib search: %v", err)
		}
		benchSinkN = len(results)
	}
}

var benchSinkN int

// BenchmarkAniZoneSearch — anizone.to Livewire HTML search parse.
// PR130: the bundled Lua script is the production path — the bench
// drives it (the shiza precedent).
func BenchmarkAniZoneSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anizone_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "anizone", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("anizone search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkShizaSearch — shiza GraphQL search decode. PR125: the
// bundled Lua script is the production path — the bench drives it.
func BenchmarkShizaSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "shiza_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "shiza", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "черная лагуна")
		if err != nil {
			b.Fatalf("shiza search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- loopback GetEpisodes benches (the list-loader hot path) ---

// BenchmarkShizaGetEpisodes — shiza release decode (dub/embed map).
func BenchmarkShizaGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "shiza_release.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := luaProvider(b, "shiza", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, srv.URL+"/releases/black-lagoon-tv")
		if err != nil {
			b.Fatalf("shiza episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// BenchmarkAnilibGetEpisodes — anilib episodes+players decode.
func BenchmarkAnilibGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	search := benchFixture(b, "anilib_search.json")
	episodes := benchFixture(b, "anilib_episodes.json")
	players := benchFixture(b, "anilib_episode_players.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Query().Get("franchise") != "" || r.URL.Path == "/api/anime":
			_, _ = w.Write(search)
		case r.URL.Query().Get("id") != "" && r.URL.Path == "/api/episodes":
			_, _ = w.Write(episodes)
		default:
			_, _ = w.Write(players)
		}
	}))
	b.Cleanup(srv.Close)
	p := luaProvider(b, "anilib", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "https://anilib.me/ru/anime/1-black-lagoon")
		if err != nil {
			b.Fatalf("anilib episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// BenchmarkAniZoneGetEpisodes — anizone series-page Livewire episode
// walk over the live series capture (the bundled Lua script, PR130).
func BenchmarkAniZoneGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anizone_series.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := luaProvider(b, "anizone", srv.URL)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "a8vfumal")
		if err != nil {
			b.Fatalf("anizone episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}
