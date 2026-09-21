package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// --- allanime crypto material/chunk parser (build IDs 173/174) ---

var (
	benchAllanimeChunk173 = benchStringData("testdata/allanime/crypto_chunk_173_DhCxOiZl.js")
	benchAllanimeChunk174 = benchStringData("testdata/allanime/crypto_chunk_174_BYlv1dKC.js")
	benchAllanimeSinkMat  *aaCryptoProfile
	benchAllanimeSinkTbl  *aaChunkTables
)

// benchStringData loads a fixture as a string at package init.
func benchStringData(rel string) string {
	data, err := os.ReadFile(rel) //nolint:gosec // trusted testdata path
	if err != nil {
		panic("bench fixture " + rel + ": " + err.Error())
	}
	return string(data)
}

// BenchmarkAllanimeChunkMaterial173 parses the build-173 crypto chunk
// (the material tables the API query masking rides on).
func BenchmarkAllanimeChunkMaterial173(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		mat, err := aaParseChunkMaterial(benchAllanimeChunk173)
		if err != nil {
			b.Fatalf("chunk 173 material: %v", err)
		}
		benchAllanimeSinkMat = mat
	}
}

// BenchmarkAllanimeChunkMaterial174 — same parser, build-174 chunk.
func BenchmarkAllanimeChunkMaterial174(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		mat, err := aaParseChunkMaterial(benchAllanimeChunk174)
		if err != nil {
			b.Fatalf("chunk 174 material: %v", err)
		}
		benchAllanimeSinkMat = mat
	}
}

// BenchmarkAllanimeChunkTables173 parses the build-173 chunk tables
// (the per-build arithmetic fragment tables).
func BenchmarkAllanimeChunkTables173(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		tbl, err := aaParseChunkTables(benchAllanimeChunk173)
		if err != nil {
			b.Fatalf("chunk 173 tables: %v", err)
		}
		benchAllanimeSinkTbl = tbl
	}
}

// BenchmarkAllanimeChunkTables174 — same parser, build-174 chunk.
func BenchmarkAllanimeChunkTables174(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		tbl, err := aaParseChunkTables(benchAllanimeChunk174)
		if err != nil {
			b.Fatalf("chunk 174 tables: %v", err)
		}
		benchAllanimeSinkTbl = tbl
	}
}

// --- anizone Livewire payload decode ---

// BenchmarkAnizoneJSONArgDecode decodes a Livewire JSON.parse argument
// (escaped-unicode heavy) — the inner loop of every anizone page walk.
func BenchmarkAnizoneJSONArgDecode(b *testing.B) {
	b.ReportAllocs()
	raw := `{"t":"\u0427\u0451\u0440\u043d\u0430\u044f \u043b\u0430\u0433\u0443\u043d\u0430","e":"\ud83d\ude00","url":"https:\/\/anizone.to\/anime\/c05ffeb2-617d-4a52-af9f-19131a5c8b31","n":42}`
	var sink []byte
	for b.Loop() {
		out, err := azDecodeJSONArgument(raw)
		if err != nil {
			b.Fatalf("decode: %v", err)
		}
		sink = out
	}
	benchAllanimeSinkStr = string(sink)
}

var benchAllanimeSinkStr string

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

// --- animepahe play links ---

// BenchmarkAnimePahePlayLinks extracts the quality→kwik map from the
// live play-page capture.
func BenchmarkAnimePahePlayLinks(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animepahe_play.html")
	var sink map[string]string
	for b.Loop() {
		sink = animePahePlayLinks(body)
		if len(sink) == 0 {
			b.Fatal("no play links extracted")
		}
	}
	benchPaheSink = sink
}

var benchPaheSink map[string]string

// --- kickassanime episode page conversion ---

// BenchmarkKaaPageEpisodes converts one wire episodes page (live
// capture) into contracts.Episode values.
func BenchmarkKaaPageEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "kickassanime_episodes.json")
	var page kaaEpisodesResponse
	if err := json.Unmarshal(body, &page); err != nil {
		b.Fatalf("decode kaa episodes: %v", err)
	}
	var sink []any
	for b.Loop() {
		eps, err := pageEpisodes("one-piece", &page)
		if err != nil {
			b.Fatalf("pageEpisodes: %v", err)
		}
		sink = append(sink[:0], any(eps))
	}
	benchSinkResults = sink
}

// --- SequenceMatcher similarity (allanime sort + rehydrate matching) ---

// BenchmarkSimilarityRatio benchmarks the CPython SequenceMatcher
// ratio port on a realistic title pair — the hot comparator of
// allanime result ranking and history rehydration.
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
// list) over the live Black Lagoon capture.
func BenchmarkAnilibSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anilib_search_black_lagoon.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newAnilib(srv.URL, benchClient(b, "anilib"))
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
func BenchmarkAniZoneSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anizone_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := newAniZone(srv.URL, benchClient(b, "anizone"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("anizone search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkShizaSearch — shiza.info GraphQL search decode.
func BenchmarkShizaSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "shiza_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newShiza(srv.URL, benchClient(b, "shiza"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("shiza search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnimePaheSearch — animepahe /api search decode.
func BenchmarkAnimePaheSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animepahe_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newAnimePahe(srv.URL, benchClient(b, "animepahe"), nil)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("animepahe search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnimeGoSearch — animego.org HTML search parse (goquery).
func BenchmarkAnimeGoSearch(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animego_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := newAnimego(srv.URL, benchClient(b, "animego"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("animego search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkNyaaSearchRSS — nyaa.si RSS feed parse.
func BenchmarkNyaaSearchRSS(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "nyaa_search_rss.xml")
	srv := benchFixtureServer(b, body, "application/rss+xml")
	p := newNyaa(srv.URL, benchClient(b, "nyaa"), nil)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("nyaa search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnimeToshoSearchRSS — animetosho RSS/Atom feed parse.
func BenchmarkAnimeToshoSearchRSS(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "animetosho_search.xml")
	srv := benchFixtureServer(b, body, "application/xml")
	p := newAnimeTosho(srv.URL, benchClient(b, "animetosho"), nil)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("animetosho search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkTokyoToshoSearchRSS — tokyotosho RSS feed parse (incl. the
// empty-feed footer detection on the non-empty fixture).
func BenchmarkTokyoToshoSearchRSS(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "tokyotosho_search.xml")
	srv := benchFixtureServer(b, body, "application/xml")
	p := newTokyoTosho(srv.URL, benchClient(b, "tokyotosho"), nil)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("tokyotosho search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkYummySearchJSON — yummy.anime JSON search decode over the
// 46K live capture (the heaviest search payload).
func BenchmarkYummySearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "yummy_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newYummy(srv.URL, srv.URL, srv.URL, "bench-ua", benchClient(b, "yummy"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("yummy search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkHDRezkaSearchHTML — hdrezka HTML search parse.
func BenchmarkHDRezkaSearchHTML(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "hdrezka_search.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := newHDRezka(srv.URL, benchClient(b, "hdrezka"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("hdrezka search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkKickassanimeSearchJSON — kickassanime JSON search decode.
func BenchmarkKickassanimeSearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "kickassanime_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newKickassanime(srv.URL, benchClient(b, "kickassanime"), 4)
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "one piece")
		if err != nil {
			b.Fatalf("kickassanime search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// BenchmarkAnilibriaSearchJSON — anilibria (aniliberty.top) JSON search.
func BenchmarkAnilibriaSearchJSON(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anilibria_search.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newAnilibria(srv.URL, AniLibriaHost, benchClient(b, "anilibria"))
	ctx := context.Background()
	for b.Loop() {
		results, err := p.Search(ctx, "black lagoon")
		if err != nil {
			b.Fatalf("anilibria search: %v", err)
		}
		benchSinkN = len(results)
	}
}

// --- loopback GetEpisodes benches (the list-loader hot path) ---

// BenchmarkAnimePaheGetEpisodes2Pages walks the clamped One Piece
// pagination (2 round trips, 55 episodes) — the episode-loader shape.
func BenchmarkAnimePaheGetEpisodes2Pages(b *testing.B) {
	b.ReportAllocs()
	p1, err := clampReleaseLastPage(benchFixture(b, "animepahe_episodes_p1.json"), 2)
	if err != nil {
		b.Fatalf("clamp p1: %v", err)
	}
	p2 := benchFixture(b, "animepahe_episodes_p2.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(p1)
			return
		}
		_, _ = w.Write(p2)
	}))
	b.Cleanup(srv.Close)
	p := newAnimePahe(srv.URL, benchClient(b, "animepahe"), nil)
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "76d59a16-e57d-4ad1-7ec6-e88f0fe9469b")
		if err != nil {
			b.Fatalf("animepahe episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}

// BenchmarkShizaGetEpisodes — shiza release decode (dub/embed map).
func BenchmarkShizaGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "shiza_release.json")
	srv := benchFixtureServer(b, body, "application/json")
	p := newShiza(srv.URL, benchClient(b, "shiza"))
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
	p := newAnilib(srv.URL, benchClient(b, "anilib"))
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
// walk over the live series capture.
func BenchmarkAniZoneGetEpisodes(b *testing.B) {
	b.ReportAllocs()
	body := benchFixture(b, "anizone_series.html")
	srv := benchFixtureServer(b, body, "text/html; charset=utf-8")
	p := newAniZone(srv.URL, benchClient(b, "anizone"))
	ctx := context.Background()
	for b.Loop() {
		eps, err := p.GetEpisodes(ctx, "https://anizone.to/anime/c05ffeb2-617d-4a52-af9f-19131a5c8b31")
		if err != nil {
			b.Fatalf("anizone episodes: %v", err)
		}
		benchSinkN = len(eps)
	}
}
