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

// kickassanime (PR58) serves the kaa.lt catalog: a fuzzy JSON search,
// a paginated per-show episode API, and per-episode server lists whose
// media ids resolve onto the krussdomi HLS edge. All fixtures are
// verbatim live captures of 2026-09-18 (kaa.lt answered every probe
// anonymously — no challenge page).
//
// PR129: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/kickassanime/main.lua) — these tests
// pin the script through the same contracts.Provider surface and the
// same fixtures the compiled Go implementation was held to.
//
// Dubs-hydration delta vs the compiled provider: the Go provider
// exposed FetchDubs (contracts.DubsHydrator) and hydrated lazily per
// episode; the Lua provider contract has no such capability (the
// session resolves only from the listing's raw_embeds), so the script
// hydrates EAGERLY per episode in one bounded-parallel batch — the
// anikoto/animedia/yummy precedent. The empty-raw_embeds pins of the
// Go tests flip accordingly: here the embeds ride the listing.

// kaaRequest is one recorded request (method, path, query, headers,
// body) — the mutex-guarded log the resolution-chain tests read.
type kaaRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// kaaOverride replaces the fixture answer for every path carrying the
// prefix (the failure-shape tests).
type kaaOverride struct {
	body string
	code int
}

// kaaTestServer builds a dedicated mux server (NOT the shared
// fixtureServer recorder — the eager hydration runs parallel fetches,
// which race the single-slot recorder) answering the kickassanime
// fixtures, with per-test override answers.
type kaaTestServer struct {
	*httptest.Server
	t         *testing.T
	mux       *http.ServeMux
	mu        sync.Mutex
	reqs      []kaaRequest
	overrides map[string]kaaOverride
}

func newKaaTestServer(t *testing.T) *kaaTestServer {
	t.Helper()
	s := &kaaTestServer{t: t, mux: http.NewServeMux(), overrides: map[string]kaaOverride{}}

	// fsearch: capture the POST body (the JSON form recordedRequest
	// does not see — the kaaBodyCapture lesson, carried over).
	s.mux.HandleFunc("/api/fsearch", func(w http.ResponseWriter, r *http.Request) {
		s.answer(w, r, "kickassanime_search.json")
	})
	// show / episodes / servers: keyed by path shape.
	s.mux.HandleFunc("/api/show/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/episodes"):
			s.answer(w, r, "kickassanime_episodes.json")
		case strings.Contains(r.URL.Path, "/episode/"):
			s.answer(w, r, "kickassanime_servers.json")
		default:
			s.answer(w, r, "kickassanime_show.json")
		}
	})

	s.Server = httptest.NewServer(s.mux)
	t.Cleanup(s.Close)
	return s
}

// override replaces the fixture answer for paths carrying prefix (an
// override, not a re-registration — Go 1.22+ ServeMux panics on
// duplicate patterns).
func (s *kaaTestServer) override(prefix string, body string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides[prefix] = kaaOverride{body: body, code: code}
}

// answer records the request and serves the first matching override,
// else the fixture file.
func (s *kaaTestServer) answer(w http.ResponseWriter, r *http.Request, name string) {
	s.record(r)
	s.mu.Lock()
	var hit *kaaOverride
	for prefix, ov := range s.overrides {
		if strings.HasPrefix(r.URL.Path, prefix) {
			o := ov
			hit = &o
			break
		}
	}
	s.mu.Unlock()
	if hit != nil {
		w.WriteHeader(hit.code)
		_, _ = w.Write([]byte(hit.body))
		return
	}
	_, _ = w.Write(fixture(s.t, name))
}

func (s *kaaTestServer) record(r *http.Request) {
	buf := new(strings.Builder)
	if r.Body != nil {
		bufs := make([]byte, 4096)
		n, _ := r.Body.Read(bufs)
		buf.Write(bufs[:n])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, kaaRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Body:   buf.String(),
	})
}

func (s *kaaTestServer) log() []kaaRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kaaRequest(nil), s.reqs...)
}

func (s *kaaTestServer) countPath(segment string) int {
	n := 0
	for _, req := range s.log() {
		if strings.Contains(req.Path, segment) {
			n++
		}
	}
	return n
}

// kaaProvider loads the bundled kickassanime script against the test
// server (the Lua harness rewrites the production base literal).
func kaaProvider(t *testing.T, srv *kaaTestServer) contracts.Provider {
	t.Helper()
	return luaProvider(t, "kickassanime", srv.URL)
}

func TestKickassAnimeSearch(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	p := kaaProvider(t, srv)

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	reqs := srv.log()
	if len(reqs) == 0 {
		t.Fatal("no request recorded")
	}
	req := reqs[len(reqs)-1]
	if req.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", req.Method)
	}
	if req.Path != "/api/fsearch" {
		t.Errorf("request path = %q, want /api/fsearch", req.Path)
	}
	if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(req.Body, `"page":1`) || !strings.Contains(req.Body, `"query":"one piece"`) {
		t.Errorf("request body = %q, want {page:1, query:\"one piece\"}", req.Body)
	}

	// Fixture values are verbatim live captures (kaa.lt /api/fsearch
	// query "one piece", 2026-09-18).
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].Title != "One Piece" {
		t.Errorf("Title = %q, want title_en", results[0].Title)
	}
	// The live fuzzy index omits title_en on loosely-matched entries
	// (the real movie-01 capture): the JP title is the fallback.
	if results[1].Title != "One Piece Movie 01" {
		t.Errorf("Title = %q, want the title fallback (no title_en on the wire)", results[1].Title)
	}
	if results[0].URL != "one-piece-0948" {
		t.Errorf("URL = %q, want the show slug", results[0].URL)
	}
	if results[0].SourceID != "kickassanime" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
}

func TestKickassAnimeSearchEmpty(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	srv.override("/api/fsearch", `{"result":[],"maxPage":0}`, http.StatusOK)
	p := kaaProvider(t, srv)

	results, err := p.Search(context.Background(), "zzz-no-such-anime")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestKickassAnimeSearchMalformedJSON(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	srv.override("/api/fsearch", "<html>challenge page</html>", http.StatusOK)
	p := kaaProvider(t, srv)

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("error = nil, want a typed decode failure")
	}
	if !strings.Contains(err.Error(), "kickassanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

func TestKickassAnimeGetEpisodes(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	p := kaaProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The episode API request shape: ?ep=1 first page + lang=ja-JP.
	var epsReq *kaaRequest
	for i := range srv.log() {
		req := srv.log()[i]
		if strings.HasSuffix(req.Path, "/episodes") {
			epsReq = &req
		}
	}
	if epsReq == nil {
		t.Fatal("the episode API was never fetched")
	}
	if !strings.Contains(epsReq.Query, "ep=1") {
		t.Errorf("episodes query = %q, want the ep=1 first page", epsReq.Query)
	}
	if !strings.Contains(epsReq.Query, "lang=ja-JP") {
		t.Errorf("episodes query = %q, want lang=ja-JP", epsReq.Query)
	}

	// Fixture = verbatim live capture of the 12-episode Dandadan page.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12", len(episodes))
	}
	first, last := episodes[0], episodes[11]
	if first.Num != "1" || last.Num != "12" {
		t.Errorf("Nums = %q..%q, want 1..12 (wire order is ascending)", first.Num, last.Num)
	}
	if first.Title != "That's How Love Starts, Ya Know!" {
		t.Errorf("Title = %q, want the wire episode title", first.Title)
	}
	// RawID embeds the show slug: the servers endpoint is
	// /api/show/{show}/episode/{ep} and Episode carries no show field.
	if first.RawID != "dandadan-da3b/ep-1-b324b5" {
		t.Errorf("RawID = %q, want dandadan-da3b/ep-1-b324b5", first.RawID)
	}
	// Eager hydration (the DubsHydrator delta): the VidStreaming
	// manifest rides the listing; the dash-typed BirdStream is skipped
	// (its id answers 502 against the manifest path — two independent
	// ids probed live).
	links, ok := first.RawEmbeds["VidStreaming"]
	if !ok || len(links) != 1 {
		t.Fatalf("RawEmbeds[\"VidStreaming\"] = %v, want the one HLS mirror", first.RawEmbeds["VidStreaming"])
	}
	if links[0] != "https://hls.krussdomi.com/manifest/6713f500b97399e0e1ae2020/master.m3u8" {
		t.Errorf("mirror = %q, want the constructed krussdomi master manifest", links[0])
	}
	if _, ok := first.RawEmbeds["BirdStream"]; ok {
		t.Errorf("BirdStream slot present = %v, want skipped (type=dash)", first.RawEmbeds["BirdStream"])
	}
}

func TestKickassAnimeGetEpisodesMultiPage(t *testing.T) {
	t.Parallel()

	page1 := `{"current_page":1,"pages":[{"number":1,"from":"01","to":"02","eps":[1,2]},{"number":2,"from":"03","to":"04","eps":[3,4]}],"result":[` +
		`{"slug":"aaa111","title":"Ep One","episode_number":1,"episode_string":"1"},` +
		`{"slug":"bbb222","title":"Ep Two","episode_number":2,"episode_string":"2"}]}`
	page2 := `{"current_page":2,"pages":[],"result":[` +
		`{"slug":"ccc333","title":"Ep Three","episode_number":3,"episode_string":"3"},` +
		`{"slug":"ddd444","title":"Ep Four","episode_number":4,"episode_string":"4"}]}`
	show := `{"slug":"dandadan-da3b","type":"tv","locales":["ja-JP","en-US"]}`
	servers := `{"slug":"aaa111","show_slug":"dandadan-da3b","servers":[` +
		`{"name":"VidStreaming","shortName":"Vid","src":"https://krussdomi.com/cat-player/player?id=6713f500b97399e0e1ae2020&source=vidstream&ln=ja-JP"}]}`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/show/dandadan-da3b", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(show))
	})
	mux.HandleFunc("/api/show/dandadan-da3b/episodes", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("ep") {
		case "1":
			_, _ = w.Write([]byte(page1))
		case "3":
			// The follow-up page is fetched with the page's FIRST
			// episode number (the pg.eps[0] hop).
			_, _ = w.Write([]byte(page2))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/show/dandadan-da3b/episode/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(servers))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := luaProvider(t, "kickassanime", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want 4 (both pages merged)", len(episodes))
	}
	// Wire order is ascending; the concurrent page fan-out must not
	// scramble the concatenation.
	wantNums := []string{"1", "2", "3", "4"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	if episodes[3].RawID != "dandadan-da3b/ep-4-ddd444" {
		t.Errorf("RawID = %q, want dandadan-da3b/ep-4-ddd444", episodes[3].RawID)
	}
}

func TestKickassAnimeGetEpisodesSkipsInvalidNumbers(t *testing.T) {
	t.Parallel()

	show := `{"slug":"x","type":"tv","locales":["ja-JP"]}`
	page := `{"current_page":1,"pages":[],"result":[` +
		`{"slug":"good1","title":"ok","episode_number":1,"episode_string":"1"},` +
		`{"slug":"bad0","title":"zero","episode_number":0,"episode_string":"0"},` +
		`{"slug":"badj","title":"junk","episode_number":"x","episode_string":"x"}]}`
	servers := `{"servers":[]}`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/show/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(show))
	})
	mux.HandleFunc("/api/show/x/episodes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/api/show/x/episode/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(servers))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := luaProvider(t, "kickassanime", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "x")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want only the number-1 entry", episodes)
	}
}

func TestKickassAnimeGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	// Real live capture shape: the Mugen Train movie's watch_uri is
	// /demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62/ep-0-8d7564
	// — the movie renders as ONE episode numbered 1 (the wire number is
	// 0 and must not leak), RawID from the watch_uri tail.
	show := `{"slug":"demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62","type":"movie","locales":["ja-JP","en-US"],"watch_uri":"/demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62/ep-0-8d7564"}`
	servers := `{"servers":[{"name":"VidStreaming","shortName":"Vid","src":"https://krussdomi.com/cat-player/player?id=6713f500b97399e0e1ae2020&source=vidstream&ln=ja-JP"}]}`

	mux := http.NewServeMux()
	episodesRequested := false
	mux.HandleFunc("/api/show/demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(show))
	})
	mux.HandleFunc("/api/show/demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62/episodes", func(w http.ResponseWriter, _ *http.Request) {
		episodesRequested = true
		http.NotFound(w, nil)
	})
	mux.HandleFunc("/api/show/demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62/episode/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(servers))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := luaProvider(t, "kickassanime", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if episodesRequested {
		t.Errorf("the episode API was hit for a movie — movies resolve from watch_uri alone")
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	if episodes[0].Num != "1" {
		t.Errorf("Num = %q, want \"1\" (movie ruling)", episodes[0].Num)
	}
	if episodes[0].RawID != "demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62/ep-0-8d7564" {
		t.Errorf("RawID = %q, want the show-prefixed watch_uri tail", episodes[0].RawID)
	}
	// Eager hydration covers the movie's single episode too.
	if _, ok := episodes[0].RawEmbeds["VidStreaming"]; !ok {
		t.Errorf("RawEmbeds = %v, want the hydrated VidStreaming slot", episodes[0].RawEmbeds)
	}
}

func TestKickassAnimeGetEpisodesShow404(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	srv.override("/api/show/", "not found", http.StatusNotFound)
	p := kaaProvider(t, srv)

	_, err := p.GetEpisodes(context.Background(), "gone-show")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestKickassAnimeEpisodeEmbeds pins the eager hydration shape: every
// server with an id-bearing src becomes a constructed master-manifest
// embed under the server's name; dash-typed servers and id-less srcs
// are skipped (the compiled FetchDubs rules, now riding the listing).
func TestKickassAnimeEpisodeEmbeds(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	p := kaaProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The hydration batch hit the per-episode servers endpoint (12
	// episodes in the fixture).
	if got := srv.countPath("/episode/"); got < 12 {
		t.Errorf("server-list fetches = %d, want >= 12 (eager hydration)", got)
	}

	embeds := episodes[0].RawEmbeds
	if len(embeds) != 1 {
		t.Fatalf("RawEmbeds = %v, want 1 dub slot (VidStreaming; the type=dash BirdStream is skipped)", embeds)
	}
	urls, ok := embeds["VidStreaming"]
	if !ok || len(urls) != 1 {
		t.Fatalf("VidStreaming = %v, want the one HLS mirror", urls)
	}
	if urls[0] != "https://hls.krussdomi.com/manifest/6713f500b97399e0e1ae2020/master.m3u8" {
		t.Errorf("mirror = %q, want the constructed krussdomi master manifest", urls[0])
	}
}

// TestKickassAnimeEpisodeEmbedsSkipMissingID: a src without an ?id=
// query has nothing to build a manifest from — the server is skipped,
// the id-bearing sibling is kept.
func TestKickassAnimeEpisodeEmbedsSkipMissingID(t *testing.T) {
	t.Parallel()

	servers := `{"slug":"b324b5","show_slug":"dandadan-da3b","servers":[` +
		`{"name":"NoID","shortName":"N","src":"https://krussdomi.com/cat-player/player?source=vidstream"},` +
		`{"name":"VidStreaming","shortName":"Vid","src":"https://krussdomi.com/cat-player/player?id=6713f500b97399e0e1ae2020&source=vidstream&ln=ja-JP"}]}`
	srv := newKaaTestServer(t)
	srv.override("/api/show/dandadan-da3b/episode/", servers, http.StatusOK)
	p := kaaProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if _, ok := episodes[0].RawEmbeds["NoID"]; ok {
		t.Errorf("NoID slot present = %v, want skipped (no id to build a manifest from)", episodes[0].RawEmbeds["NoID"])
	}
	if len(episodes[0].RawEmbeds["VidStreaming"]) != 1 {
		t.Errorf("VidStreaming = %v, want the id-bearing mirror", episodes[0].RawEmbeds["VidStreaming"])
	}
}

func TestKickassAnimeResolveStream(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	p := kaaProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	before := srv.countPath("/episode/ep-1-b324b5")

	stream, err := p.ResolveStream(context.Background(), episodes[0], "VidStreaming")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	// The fresh-sandbox contract re-derives the server list at resolve
	// time (the anikoto/yummy re-fetch pattern).
	if after := srv.countPath("/episode/ep-1-b324b5"); after <= before {
		t.Errorf("server-list fetches for ep-1 = %d before, %d after — the resolve must re-fetch", before, after)
	}

	if stream.DubName != "VidStreaming" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["auto"]
	if !ok {
		t.Fatalf("Links = %v, want one \"auto\" entry (the master manifest carries all variants)", stream.Links)
	}
	if src.URL != "https://hls.krussdomi.com/manifest/6713f500b97399e0e1ae2020/master.m3u8" {
		t.Errorf("URL = %q", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8", src.Type)
	}
	if src.Quality != "auto" {
		t.Errorf("Quality = %q, want auto", src.Quality)
	}
	if src.Headers["Referer"] != "https://krussdomi.com/" {
		t.Errorf("Headers = %v, want the krussdomi Referer", src.Headers)
	}
}

func TestKickassAnimeResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv := newKaaTestServer(t)
	p := kaaProvider(t, srv)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	_, err = p.ResolveStream(context.Background(), episodes[0], "Nope")
	if err == nil || !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

// TestKickassAnimeResolveStreamBadRawID: a RawID without the
// {showSlug}/{epSlug} shape is a caller bug — typed invalid input, no
// fetch.
func TestKickassAnimeResolveStreamBadRawID(t *testing.T) {
	t.Parallel()

	p := kaaProvider(t, newKaaTestServer(t))
	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "no-slash-here"}, "VidStreaming")
	if err == nil || !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestKickassAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "kickassanime")
	if p.ID() != "kickassanime" || p.Name() != "KickassAnime" || p.BaseURL() != "https://kaa.lt" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// JA audio native, EN dub audio switchable inside the same master
	// manifest [LIVE-VERIFIED 2026-09-18: EXT-X-MEDIA NAME="English"].
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if lc, ok := p.(interface{ ContentLanguage() string }); !ok || lc.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %v, want ja", p)
	}
}

// TestKickassAnimeNamePreference pins the PR42 routing: the kaa.lt
// fuzzy index matches romaji/english titles only — Cyrillic queries
// there are guaranteed-zero.
func TestKickassAnimeNamePreference(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "kickassanime")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatalf("the kickassanime lua provider lost the NamePreference surface (%T)", p)
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
}

// TestKickassAnimeSmokeQuery pins the PR52 capability: the shared
// probes (черная лагуна / black lagoon) surface kaa.lt entries whose
// episode server lists are currently empty server-side — the chain
// dies at hydration through no provider-code fault. "dandadan" is the
// proven broad hit: two fresh-season search hits, both with populated
// per-episode servers.
func TestKickassAnimeSmokeQuery(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "kickassanime")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatalf("kickassanime does not declare SmokeQueryProvider")
	}
	if sq.SmokeQuery() != "dandadan" {
		t.Errorf("SmokeQuery = %q, want dandadan", sq.SmokeQuery())
	}
}
