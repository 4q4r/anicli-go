package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// requestLog is a recording test server serving exact-path bodies and
// capturing every request in order (fixtureServer only keeps the last
// one; the gogoanime resolve chain needs per-hop header assertions).
type requestLog struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []recordedRequest
}

func newRequestLog(t *testing.T, pages map[string]string) *requestLog {
	t.Helper()

	l := &requestLog{}
	mux := http.NewServeMux()
	for path, body := range pages {
		mux.HandleFunc(path, l.record(body))
	}
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

func (l *requestLog) get(path string) (recordedRequest, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, req := range l.requests {
		if req.Path == path {
			return req, true
		}
	}
	return recordedRequest{}, false
}

// record wraps a body-serving handler with request capture.
func (l *requestLog) record(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		l.mu.Lock()
		l.requests = append(l.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Form:   r.PostForm,
		})
		l.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}
}

// routeExtra registers an additional exact-path page after construction
// (Go's ServeMux guards its routing tree, late registration stays safe).
func (l *requestLog) routeExtra(t *testing.T, path, body string) {
	t.Helper()
	l.srv.Config.Handler.(*http.ServeMux).Handle(path, http.HandlerFunc(l.record(body)))
}

func TestGogoAnimeSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_search.html"))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// [LIVE-VERIFIED 2026-09-13] gogoanime.by is a WordPress/dramastream
	// site: search is GET /?s=<query>, not the legacy /search.html form.
	if rec.Path != "/" {
		t.Errorf("request path = %q, want /", rec.Path)
	}
	if want := "s=one+piece"; rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (first .listupd a.tip cards)", len(results))
	}
	if results[0].Title != "One Piece: Heroines" {
		t.Errorf("Title = %q, want anchor title attribute", results[0].Title)
	}
	// Result URLs stay the absolute /series/ hrefs the site emits.
	if results[0].URL != "https://gogoanime.by/series/one-piece-heroines/" {
		t.Errorf("URL = %q, want the absolute series href", results[0].URL)
	}
	if results[0].SourceID != "gogoanime" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != "https://i0.wp.com/gogoanime.by/wp-content/uploads/2026/07/one-piece-heroines.webp?resize=246,350" {
		t.Errorf("Poster = %q, want img src", results[0].Poster)
	}
	// The sidebar .leftseries card must not leak into search results.
	for _, r := range results {
		if strings.Contains(r.URL, "bleach-sennen") {
			t.Errorf("URL = %q leaked from the sidebar popular section", r.URL)
		}
	}
}

func TestGogoAnimeSearchSendsReferer(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html></html>")
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// PR5 task ruling kept: the embed-heavy site expects the Referer.
	if got := rec.Header.Get("Referer"); got != srv.URL {
		t.Errorf("Referer = %q, want the site root", got)
	}
}

func TestGogoAnimeGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/series/naruto-shippuuden/" {
			_, _ = w.Write(fixture(t, "gogoanime_anime.html"))
			return
		}
		http.NotFound(w, r)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/series/naruto-shippuuden/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 5 {
		t.Fatalf("episodes = %d, want 5", len(episodes))
	}

	// [LIVE-VERIFIED 2026-09-13] the dramastream series page renders ALL
	// episode-items server-side, newest-first (Episode 500 … 496); the
	// provider reverses to ascending like the legacy ajax list did.
	wantNums := []string{"496", "497", "498", "499", "500"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	if episodes[4].Title != "Episode 500" {
		t.Errorf("Title = %q, want the anchor text", episodes[4].Title)
	}
	if episodes[4].RawID != "https://gogoanime.by/naruto-shippuuden-episode-500-english-subbed/" {
		t.Errorf("RawID = %q, want the absolute episode href", episodes[4].RawID)
	}
	if len(episodes[0].RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (servers are fetched lazily)", episodes[0].RawEmbeds)
	}
}

func TestGogoAnimeGetEpisodesRelativeURL(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_anime.html"))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	if _, err := p.GetEpisodes(context.Background(), "/series/naruto-shippuuden/"); err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if rec.Path != "/series/naruto-shippuuden/" {
		t.Errorf("request path = %q, want the base-prefixed series path", rec.Path)
	}
}

func TestGogoAnimeGetEpisodesNoEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no episode list</body></html>")
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/series/none")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without .episodes-container", len(episodes))
	}
}

func TestGogoAnimeGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/series/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestGogoAnimeFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Host-swap the live-captured absolute URLs onto this server.
		_, _ = w.Write([]byte(strings.ReplaceAll( //nolint:gosec // test server: host-swapped fixture body
			string(fixture(t, "gogoanime_episode.html")), "https://gogoanime.by", "http://"+r.Host)))
	})
	p := newGogoAnime(srv.URL, testClient(t, "gogoanime"))

	episode := contracts.Episode{
		Num:   "500",
		RawID: srv.URL + "/naruto-shippuuden-episode-500-english-subbed/",
	}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if rec.Path != "/naruto-shippuuden-episode-500-english-subbed/" {
		t.Errorf("request path = %q, want the episode page", rec.Path)
	}

	embeds := got.RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("RawEmbeds = %v, want 2 servers (no-data-src skipped)", embeds)
	}
	mega, ok := embeds["Mega"]
	if !ok || len(mega) != 1 {
		t.Fatalf("Mega = %v, want the server's player URL", embeds["Mega"])
	}
	if !strings.HasPrefix(mega[0], srv.URL+"/player/?source=embed&url=") {
		t.Errorf("Mega data-src = %q, want the host-rewritten player URL", mega[0])
	}
	if _, ok := embeds["Broken"]; ok {
		t.Error("Broken (no data-src) must be skipped")
	}
}

// gogoMegaVidServers builds the live-verified megavid chain [LIVE-VERIFIED
// 2026-09-13]: the referer-gated /player/ page on the gogo host serves an
// iframe to a megavid embed whose #player-payload points at a JSON source
// endpoint. Hosts are swapped to the recording servers so the chain runs
// offline.
func gogoMegaVidServers(t *testing.T) (gogo, embed *requestLog) {
	t.Helper()

	embed = newRequestLog(t, map[string]string{
		"/mal/1735/500/sub":        string(fixture(t, "gogoanime_megavid.html")),
		"/mal/1735/500/sub/source": string(fixture(t, "gogoanime_megavid_source.json")),
	})
	playerBody := strings.Replace(
		string(fixture(t, "gogoanime_player.html")),
		"https://megavid.buzz/mal/1735/500/sub",
		embed.srv.URL+"/mal/1735/500/sub", 1)
	gogo = newRequestLog(t, map[string]string{"/player/": playerBody})
	return gogo, embed
}

// TestGogoAnimeResolveStreamMegaVidRoundTrip walks that chain end to end
// and pins the referer/accept contracts on every hop.
func TestGogoAnimeResolveStreamMegaVidRoundTrip(t *testing.T) {
	t.Parallel()

	gogo, embed := gogoMegaVidServers(t)

	episodeURL := gogo.srv.URL + "/naruto-shippuuden-episode-500-english-subbed/"
	p := newGogoAnime(gogo.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		Num:   "500",
		RawID: episodeURL,
		RawEmbeds: map[string][]string{
			"Mega": {gogo.srv.URL + "/player/?source=embed&url=V3oraS9OdVNOdFNoTUZuWm9qa1FrRWtnd3FZTmJlM3hXRWVzdmZqRVorZG9CYW15bFVMQWdCRHNxbzhRcTdNKw%3D%3D"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Mega")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Mega" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry", stream.Links)
	}
	if !strings.HasPrefix(src.URL, "https://megavid.buzz/vid/") {
		t.Errorf("URL = %q, want the megavid hls source", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8 (hls)", src.Type)
	}
	if src.Headers["Referer"] != embed.srv.URL+"/mal/1735/500/sub" {
		t.Errorf("Referer = %q, want the megavid embed page", src.Headers["Referer"])
	}

	// The /player/ proxy is referer-gated live (it redirects to the site
	// root without one) — pin the episode-page Referer on that hop.
	playerReq, ok := gogo.get("/player/")
	if !ok {
		t.Fatal("the player page was never fetched")
	}
	if got := playerReq.Header.Get("Referer"); got != episodeURL {
		t.Errorf("player fetch Referer = %q, want the episode URL", got)
	}

	// The source hop mirrors the embed bootstrap: Accept json + embed
	// Referer.
	srcReq, ok := embed.get("/mal/1735/500/sub/source")
	if !ok {
		t.Fatal("the megavid source endpoint was never fetched")
	}
	if got := srcReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("source fetch Accept = %q, want application/json", got)
	}
	if got := srcReq.Header.Get("Referer"); got != embed.srv.URL+"/mal/1735/500/sub" {
		t.Errorf("source fetch Referer = %q, want the embed page URL", got)
	}
}

// TestGogoAnimeResolveStreamMegaPlayRoundTrip covers the second live embed
// family [LIVE-VERIFIED 2026-09-13]: /player/ → megaplay embed with an
// inline jwplayer file URL.
func TestGogoAnimeResolveStreamMegaPlayRoundTrip(t *testing.T) {
	t.Parallel()

	embed := newRequestLog(t, map[string]string{
		"/embed.php": string(fixture(t, "gogoanime_megaplay.html")),
	})
	playerBody := strings.Replace(
		string(fixture(t, "gogoanime_player.html")),
		"https://megavid.buzz/mal/1735/500/sub",
		embed.srv.URL+"/embed.php?sid=x", 1)
	gogo := newRequestLog(t, map[string]string{"/player/": playerBody})

	p := newGogoAnime(gogo.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		Num:   "1178",
		RawID: gogo.srv.URL + "/one-piece-episode-1178-english-subbed/",
		RawEmbeds: map[string][]string{
			"HD": {gogo.srv.URL + "/player/?source=embed&url=x"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "HD")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry", stream.Links)
	}
	if src.URL != "https://megaplay.su/uploads/hls/a1p6UHRKOGFtNnpPQjlTMVBVeDFoSTBxNEpsVUhlY1kzMCtyanMreGUwcz0/index.m3u8?v=1789317979" {
		t.Errorf("URL = %q, want the inline megaplay hls file", src.URL)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8", src.Type)
	}

	embedReq, ok := embed.get("/embed.php")
	if !ok {
		t.Fatal("the megaplay embed page was never fetched")
	}
	// The browser sends the full /player/ page URL (query included) as
	// the embed-fetch Referer.
	if got := embedReq.Header.Get("Referer"); got != gogo.srv.URL+"/player/?source=embed&url=x" {
		t.Errorf("embed fetch Referer = %q, want the player page URL", got)
	}
}

// TestGogoAnimeResolveStreamLazyFetchesDubs keeps the Python behavior
// (gogoanime.py:124-125): an episode without embeds gets its server list
// fetched on demand inside resolve.
func TestGogoAnimeResolveStreamLazyFetchesDubs(t *testing.T) {
	t.Parallel()

	gogo, _ := gogoMegaVidServers(t)
	episodePage := strings.ReplaceAll(
		string(fixture(t, "gogoanime_episode.html")), "https://gogoanime.by", gogo.srv.URL)
	gogo.routeExtra(t, "/naruto-shippuuden-episode-500-english-subbed/", episodePage)

	p := newGogoAnime(gogo.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		Num:       "500",
		RawID:     gogo.srv.URL + "/naruto-shippuuden-episode-500-english-subbed/",
		RawEmbeds: map[string][]string{},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Mega")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if _, ok := stream.Links["720"]; !ok {
		t.Fatalf("Links = %v, want a 720 entry via the lazy dub fetch", stream.Links)
	}
}

// TestGogoAnimeResolveStreamFailsLoudWithoutSources pins the no-silent-
// failure rule: when no embed resolves anything, the chain error surfaces.
func TestGogoAnimeResolveStreamFailsLoudWithoutSources(t *testing.T) {
	t.Parallel()

	gogo := newRequestLog(t, map[string]string{"/player/": "<html><body>player maintenance</body></html>"})

	p := newGogoAnime(gogo.srv.URL, testClient(t, "gogoanime"))
	episode := contracts.Episode{
		RawID: gogo.srv.URL + "/episode-1/",
		RawEmbeds: map[string][]string{
			"Mega": {gogo.srv.URL + "/player/?source=embed&url=x"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Mega")
	if err == nil {
		t.Fatal("error = nil, want the extraction failure of the empty player page")
	}
	if !strings.Contains(err.Error(), "gogoanime") {
		t.Errorf("error = %v, want provider-tagged failure", err)
	}
}

func TestGogoAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := newGogoAnime(GogoAnimeBase, testClient(t, "gogoanime"))
	if p.ID() != "gogoanime" || p.Name() != "GogoAnime" || p.BaseURL() != GogoAnimeBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeVideo {
		t.Errorf("SourceType = %q, want video", p.SourceType())
	}
}
