package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// kaaBodyCapture records the raw request body of the last POST (the
// fsearch endpoint speaks JSON, which recordedRequest.Form does not
// see).
type kaaBodyCapture struct {
	mu   sync.Mutex
	body []byte
}

func (c *kaaBodyCapture) read(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.body = b
	c.mu.Unlock()
}

func (c *kaaBodyCapture) last() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

func TestKickassAnimeSearch(t *testing.T) {
	t.Parallel()

	cap := &kaaBodyCapture{}
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		cap.read(r)
		if r.URL.Path == "/api/fsearch" {
			_, _ = w.Write(fixture(t, "kickassanime_search.json"))
			return
		}
		http.NotFound(w, r)
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/api/fsearch" {
		t.Errorf("request path = %q, want /api/fsearch", rec.Path)
	}
	if got := rec.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Page  int    `json:"page"`
		Query string `json:"query"`
	}
	if err := json.Unmarshal(cap.last(), &body); err != nil {
		t.Fatalf("request body %q is not JSON: %v", cap.last(), err)
	}
	if body.Page != 1 || body.Query != "one piece" {
		t.Errorf("request body = %q, want {page:1, query:\"one piece\"}", cap.last())
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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":[],"maxPage":0}`))
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>challenge page</html>"))
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show/dandadan-da3b":
			_, _ = w.Write(fixture(t, "kickassanime_show.json"))
		case "/api/show/dandadan-da3b/episodes":
			if got := r.URL.Query().Get("lang"); got != "ja-JP" {
				t.Errorf("episodes lang = %q, want ja-JP", got)
			}
			_, _ = w.Write(fixture(t, "kickassanime_episodes.json"))
		default:
			http.NotFound(w, r)
		}
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	episodes, err := p.GetEpisodes(context.Background(), "dandadan-da3b")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
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
	if len(first.RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (servers hydrate per episode)", first.RawEmbeds)
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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show/dandadan-da3b":
			_, _ = w.Write([]byte(show))
		case "/api/show/dandadan-da3b/episodes":
			switch r.URL.Query().Get("ep") {
			case "1":
				_, _ = w.Write([]byte(page1))
			case "3":
				// The follow-up page is fetched with the page's FIRST
				// episode number (the Anivexa recipe's pg.eps[0] hop).
				_, _ = w.Write([]byte(page2))
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

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
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show/x":
			_, _ = w.Write([]byte(show))
		case "/api/show/x/episodes":
			_, _ = w.Write([]byte(page))
		default:
			http.NotFound(w, r)
		}
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

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
	episodesRequested := false
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/show/demon-slayer-kimetsu-no-yaiba-the-movie-mugen-train-cc62":
			_, _ = w.Write([]byte(show))
		case strings.HasSuffix(r.URL.Path, "/episodes"):
			episodesRequested = true
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

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
}

func TestKickassAnimeGetEpisodesShow404(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	_, err := p.GetEpisodes(context.Background(), "gone-show")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestKickassAnimeFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show/dandadan-da3b/episode/ep-1-b324b5" {
			_, _ = w.Write(fixture(t, "kickassanime_servers.json"))
			return
		}
		http.NotFound(w, r)
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	episode := contracts.Episode{
		Num:       "1",
		RawID:     "dandadan-da3b/ep-1-b324b5",
		RawEmbeds: map[string][]string{},
	}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if rec.Path != "/api/show/dandadan-da3b/episode/ep-1-b324b5" {
		t.Errorf("request path = %q, want the episode servers endpoint", rec.Path)
	}

	embeds := got.RawEmbeds
	// [LIVE-VERIFIED 2026-09-18] the Dandadan ep-1 server list carries
	// VidStreaming (HLS id) + BirdStream (type=dash); the dash id
	// answers 502 against the krussdomi manifest path (two independent
	// ids probed), so dash-typed servers are skipped — an embed slot
	// that cannot resolve is dead weight in the picker.
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

func TestKickassAnimeFetchDubsSkipsMissingID(t *testing.T) {
	t.Parallel()

	servers := `{"slug":"b324b5","show_slug":"dandadan-da3b","servers":[` +
		`{"name":"NoID","shortName":"N","src":"https://krussdomi.com/cat-player/player?source=vidstream"},` +
		`{"name":"VidStreaming","shortName":"Vid","src":"https://krussdomi.com/cat-player/player?id=6713f500b97399e0e1ae2020&source=vidstream&ln=ja-JP"}]}`
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show/dandadan-da3b/episode/ep-1-b324b5" {
			_, _ = w.Write([]byte(servers))
			return
		}
		http.NotFound(w, r)
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	episode := contracts.Episode{
		Num:       "1",
		RawID:     "dandadan-da3b/ep-1-b324b5",
		RawEmbeds: map[string][]string{},
	}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if _, ok := got.RawEmbeds["NoID"]; ok {
		t.Errorf("NoID slot present = %v, want skipped (no id to build a manifest from)", got.RawEmbeds["NoID"])
	}
	if len(got.RawEmbeds["VidStreaming"]) != 1 {
		t.Errorf("VidStreaming = %v, want the id-bearing mirror", got.RawEmbeds["VidStreaming"])
	}
}

func TestKickassAnimeResolveStream(t *testing.T) {
	t.Parallel()

	p := newKickassanime(KickassAnimeBase, testClient(t, "kickassanime"), 4)
	episode := contracts.Episode{
		Num:   "1",
		RawID: "dandadan-da3b/ep-1-b324b5",
		RawEmbeds: map[string][]string{
			"VidStreaming": {"https://hls.krussdomi.com/manifest/6713f500b97399e0e1ae2020/master.m3u8"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "VidStreaming")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
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

// TestKickassAnimeResolveStreamLazyFetchesDubs keeps the gogoanime
// behavior: an episode without embeds gets its server list fetched on
// demand inside resolve.
func TestKickassAnimeResolveStreamLazyFetchesDubs(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show/dandadan-da3b/episode/ep-1-b324b5" {
			_, _ = w.Write(fixture(t, "kickassanime_servers.json"))
			return
		}
		http.NotFound(w, r)
	})
	p := newKickassanime(srv.URL, testClient(t, "kickassanime"), 4)

	episode := contracts.Episode{
		Num:       "1",
		RawID:     "dandadan-da3b/ep-1-b324b5",
		RawEmbeds: map[string][]string{},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "VidStreaming")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if _, ok := stream.Links["auto"]; !ok {
		t.Fatalf("Links = %v, want the lazy-fetched auto source", stream.Links)
	}
}

func TestKickassAnimeResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p := newKickassanime(KickassAnimeBase, testClient(t, "kickassanime"), 4)
	episode := contracts.Episode{
		Num:   "1",
		RawID: "dandadan-da3b/ep-1-b324b5",
		RawEmbeds: map[string][]string{
			"VidStreaming": {"https://hls.krussdomi.com/manifest/6713f500b97399e0e1ae2020/master.m3u8"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Nope")
	if err == nil {
		t.Fatal("error = nil, want a typed unknown-dub failure")
	}
	if !strings.Contains(err.Error(), "kickassanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

func TestKickassAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := newKickassanime(KickassAnimeBase, testClient(t, "kickassanime"), 4)
	if p.ID() != "kickassanime" || p.Name() != "KickassAnime" || p.BaseURL() != KickassAnimeBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if KickassAnimeBase != "https://kaa.lt" {
		t.Errorf("KickassAnimeBase = %q, want the live kaa.lt domain", KickassAnimeBase)
	}
	// JA audio native, EN dub audio switchable inside the same master
	// manifest [LIVE-VERIFIED 2026-09-18: EXT-X-MEDIA NAME="English"].
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %q, want ja", p.ContentLanguage())
	}
}

// TestKickassAnimeNamePreference pins the PR42 routing: the kaa.lt
// fuzzy index matches romaji/english titles only — Cyrillic queries
// there are guaranteed-zero.
func TestKickassAnimeNamePreference(t *testing.T) {
	t.Parallel()

	p := newKickassanime(KickassAnimeBase, testClient(t, "kickassanime"), 4)
	np, ok := any(p).(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatalf("KickassAnime must implement contracts.NamePreferenceProvider")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
}

// TestKickassAnimeSmokeQuery pins the PR52 capability: the shared
// probes (черная лагуна / black lagoon) hit kaa.lt catalog entries
// whose episode server lists are currently EMPTY server-side
// [LIVE-VERIFIED 2026-09-18: black-lagoon-ac06 ep-1 answers servers:[]
// in both ja-JP and en-US], so the provider declares its own
// end-to-end-proven broad hit instead.
func TestKickassAnimeSmokeQuery(t *testing.T) {
	t.Parallel()

	p := newKickassanime(KickassAnimeBase, testClient(t, "kickassanime"), 4)
	sq, ok := contracts.Provider(p).(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatalf("KickassAnime does not declare SmokeQueryProvider")
	}
	if sq.SmokeQuery() == "" {
		t.Fatalf("SmokeQuery = \"\", want a provider-specific probe")
	}
}
