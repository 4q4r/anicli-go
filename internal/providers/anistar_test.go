package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"golang.org/x/text/encoding/charmap"
)

// testAniStar builds the provider against one httptest fixture server
// standing in for the anistar.org catalog AND the /test/player2/
// player host (both legs ride the same origin live). Routes:
//
//	"/"                      (POST) → anistar_search.html (cp1251)
//	"/test/player2/..."            → anistar_player_series.html
//	"/10156-….html"                → anistar_player_movie.html page
//	"/5590-….html"                 → anistar_anime.html (cp1251)
//
// Handlers override the default mapping per test.
func testAniStar(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*AniStar, *recordedRequest) {
	t.Helper()
	srv, rec := fixtureServer(t, handle)
	return newAniStar(srv.URL, testClient(t, "anistar")), rec
}

// anistarServeFile is the fixture handler: serves one testdata file
// verbatim (the cp1251 fixtures keep their raw bytes).
func anistarServeFile(t *testing.T, name string) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}
}

// anistarFormGet reads one form field from the recorded POST body
// (rec.Form is the plain map the shared fixture server records).
func anistarFormGet(rec *recordedRequest, key string) string {
	if vals, ok := rec.Form[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// TestAniStarMeta pins the service-level identity: registration
// identity, BOTH content semantics (RU voice-overs over present
// video), the RU content language, and the capabilities the provider
// deliberately does NOT declare: the search index answers the shared
// RU smoke probe (verified live 2026-09-20) and the RU-default query
// routing needs no NamePreference (yummy precedent).
func TestAniStarMeta(t *testing.T) {
	t.Parallel()

	p := newAniStar(AniStarBase, testClient(t, "anistar"))
	if p.ID() != "anistar" || p.Name() != "AniStar" {
		t.Errorf("identity = %q/%q, want anistar/AniStar", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://anistar.org" {
		t.Errorf("BaseURL = %q, want the site root", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
	if _, ok := any(p).(contracts.SmokeQueryProvider); !ok {
		t.Errorf("SmokeQueryProvider not declared: the shared RU probe lands on the legacy vk/myvi embed generation no extractor covers")
	}
	if _, ok := any(p).(contracts.NamePreferenceProvider); ok {
		t.Errorf("NamePreferenceProvider declared: RU is the default routing")
	}
}

// TestAniStarSearch pins the catalog search against the real captured
// response page (testdata/anistar_search.html, POST / with
// do=search&subaction=search&story=наруто, captured live 2026-09-20):
// the query MUST ride cp1251 bytes (the UTF-8 form answers zero hits,
// live-verified), the results parse the .news cards, and the site-news
// / manga-reader cards the site mixes into the answer are dropped by
// the /anime/ category filter — 3 releases surface of 10 raw cards.
func TestAniStarSearch(t *testing.T) {
	t.Parallel()

	p, rec := testAniStar(t, anistarServeFile(t, "anistar_search.html"))

	results, err := p.Search(context.Background(), "наруто")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != http.MethodPost {
		t.Errorf("method = %q, want POST (the DLE full-search form)", rec.Method)
	}
	if rec.Path != "/" {
		t.Errorf("path = %q, want /", rec.Path)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form content type", ct)
	}
	if got := anistarFormGet(rec, "do"); got != "search" {
		t.Errorf("do = %q, want search", got)
	}
	if got := anistarFormGet(rec, "subaction"); got != "search" {
		t.Errorf("subaction = %q, want search", got)
	}
	// The story value reaches the wire as cp1251 bytes.
	wantStory, encErr := charmap.Windows1251.NewEncoder().Bytes([]byte("наруто"))
	if encErr != nil {
		t.Fatalf("encode fixture query: %v", encErr)
	}
	if got := anistarFormGet(rec, "story"); got != string(wantStory) {
		t.Errorf("story = %q bytes, want the cp1251 encoding of наруто (% x)", got, wantStory)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (the /anime/ releases; news and manga cards dropped)", len(results))
	}
	first := results[0]
	if first.Title != "Д — значит диджей 2 сезон / D4DJ: All Mix" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "https://anistar.org/9190-d-znachit-didzhey-2-sezon-d4dj-all-mix.html" {
		t.Errorf("URL = %q, want the release page URL", first.URL)
	}
	if first.Poster != p.BaseURL()+"/uploads/posters/9190/original.jpg" {
		t.Errorf("Poster = %q, want the absolutized poster URL", first.Poster)
	}
	if first.SourceID != "anistar" {
		t.Errorf("SourceID = %q, want anistar", first.SourceID)
	}
}

// TestAniStarSearchMiss pins the zero-result answer (captured live
// 2026-09-20 with a junk query): DLE answers HTTP 200 with no .news
// cards — zero results, no error.
func TestAniStarSearchMiss(t *testing.T) {
	t.Parallel()

	p, _ := testAniStar(t, anistarServeFile(t, "anistar_search_miss.html"))

	results, err := p.Search(context.Background(), "фырпрщхшт")
	if err != nil {
		t.Fatalf("Search miss must be zero results, got error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

// TestAniStarGetEpisodes pins the episode/dub listing against the real
// captures: the release page (anistar_anime.html) carries the p2p
// player iframe whose id+hash key the player page
// (anistar_player_series.html) — the var playlst JS array parsed into
// episodes grouped by number and dubs keyed by the title suffix (the
// suffix-less entries are the release's own AniStar voice-over).
func TestAniStarGetEpisodes(t *testing.T) {
	t.Parallel()

	routes := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			anistarServeFile(t, "anistar_anime.html")(w, r)
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php"):
			if got := r.Header.Get("Referer"); !strings.Contains(got, "/5590-detektiv-konan") {
				t.Errorf("player request Referer = %q, want the release page", got)
			}
			anistarServeFile(t, "anistar_player_series.html")(w, r)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	p, _ := testAniStar(t, routes)

	animeURL := p.BaseURL() + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want 4 (1, 2, 15, 16)", len(episodes))
	}
	wantNums := []string{"1", "2", "15", "16"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}

	ep1 := episodes[0]
	wantDubs := []string{"AniStar", "Многоголосая озвучка"}
	if len(ep1.RawEmbeds) != len(wantDubs) {
		t.Fatalf("episode 1 dubs = %v, want %v", ep1.RawEmbeds, wantDubs)
	}
	for _, dub := range wantDubs {
		if len(ep1.RawEmbeds[dub]) != 1 {
			t.Errorf("dub %q embeds = %d, want 1 (the player#media_id reference)", dub, len(ep1.RawEmbeds[dub]))
		}
	}
	// media_id 7562 is the verbatim «Серия 1» entry of the capture.
	if ref := ep1.RawEmbeds["AniStar"][0]; !strings.HasSuffix(ref, "#7562") {
		t.Errorf("AniStar embed ref = %q, want …#7562", ref)
	}
	if ref := ep1.RawEmbeds["Многоголосая озвучка"][0]; !strings.HasSuffix(ref, "#7565") {
		t.Errorf("Многоголосая озвучка embed ref = %q, want …#7565", ref)
	}

	// The one-off teams of episodes 15/16 keep their verbatim suffixes.
	ep15 := episodes[2]
	if _, ok := ep15.RawEmbeds["(Zendos)"]; !ok {
		t.Errorf("episode 15 dubs = %v, want the verbatim (Zendos) team", ep15.RawEmbeds)
	}
	if _, ok := ep15.RawEmbeds["(OVERLORDS)"]; !ok {
		t.Errorf("episode 15 dubs = %v, want the verbatim (OVERLORDS) team", ep15.RawEmbeds)
	}
}

// TestAniStarGetEpisodesMovie pins the movie shape: the single
// «Фильм» playlst entry becomes episode 1 (the movie numbering
// convention).
func TestAniStarGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	routes := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			anistarServeFile(t, "anistar_anime.html")(w, r)
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php"):
			anistarServeFile(t, "anistar_player_movie.html")(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	p, _ := testAniStar(t, routes)

	animeURL := p.BaseURL() + "/10156-detektiv-konan-pentagramma.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	if episodes[0].Num != "1" {
		t.Errorf("movie Num = %q, want 1", episodes[0].Num)
	}
	if dub, ok := episodes[0].RawEmbeds["AniStar"]; !ok || len(dub) != 1 || !strings.HasSuffix(dub[0], "#52940") {
		t.Errorf("movie RawEmbeds = %v, want AniStar → …#52940", episodes[0].RawEmbeds)
	}
}

// TestAniStarGetEpisodesNoPlayer pins the typed miss: a page without
// the p2p player iframe (site-news posts share the .html URL shape)
// fails loud with ErrNotFound.
func TestAniStarGetEpisodesNoPlayer(t *testing.T) {
	t.Parallel()

	p, _ := testAniStar(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>новость без плеера</body></html>"))
	})

	animeURL := p.BaseURL() + "/7748-skuchaesh-po-naruto.html"
	_, err := p.GetEpisodes(context.Background(), animeURL)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAniStarResolveStream pins the stream resolution: the media_id
// fragment keys the fresh player page fetch, the files[] HLS ladder
// wins the quality keys, files_mp4[] fills the gaps, every source
// carries the load-bearing Referer header (the an-media edge answers
// 403 without it — verified live 2026-09-20).
func TestAniStarResolveStream(t *testing.T) {
	t.Parallel()

	routes := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			anistarServeFile(t, "anistar_anime.html")(w, r)
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php"):
			anistarServeFile(t, "anistar_player_series.html")(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	p, _ := testAniStar(t, routes)

	animeURL := p.BaseURL() + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), episodes[0], "AniStar")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "AniStar" {
		t.Errorf("DubName = %q, want AniStar", stream.DubName)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("links = %d, want 2 (360, 720 — HLS wins the keys, mp4 fills none)", len(stream.Links))
	}
	for _, q := range []string{"360", "720"} {
		src, ok := stream.Links[q]
		if !ok {
			t.Errorf("quality %s missing", q)
			continue
		}
		if !strings.Contains(src.URL, "sf2.an-media.org") || !strings.HasSuffix(src.URL, "index.m3u8") {
			t.Errorf("quality %s URL = %q, want the sf2 HLS manifest (files[] wins)", q, src.URL)
		}
		if src.Type != "m3u8" {
			t.Errorf("quality %s Type = %q, want m3u8", q, src.Type)
		}
		if src.Quality != q {
			t.Errorf("quality %s Quality = %q", q, src.Quality)
		}
		if got := src.Headers["Referer"]; got != p.BaseURL()+"/" {
			t.Errorf("quality %s Referer = %q, want the site root (the edge 403s without it)", q, got)
		}
	}
}

// TestAniStarResolveStreamMovieMP4 pins the movie variant: files[]
// carries one m3u8 (360) and one edge-mp4 (720) entry — the mp4 fills
// nothing (both keys taken) and is labeled by its URL shape.
func TestAniStarResolveStreamMovieMP4(t *testing.T) {
	t.Parallel()

	routes := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			anistarServeFile(t, "anistar_anime.html")(w, r)
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php"):
			anistarServeFile(t, "anistar_player_movie.html")(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	p, _ := testAniStar(t, routes)

	animeURL := p.BaseURL() + "/10156-detektiv-konan-pentagramma.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), episodes[0], "AniStar")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("links = %d, want 2", len(stream.Links))
	}
	if src := stream.Links["360"]; src.Type != "m3u8" {
		t.Errorf("360 Type = %q, want m3u8", src.Type)
	}
	// The sfv keyed edge entry has no .m3u8 suffix — labeled mp4.
	if src := stream.Links["720"]; src.Type != "mp4" {
		t.Errorf("720 Type = %q, want mp4", src.Type)
	}
}

// TestAniStarResolveStreamUnknownDub pins the dub-miss: a dubID absent
// from RawEmbeds fails loud with ErrNotFound.
func TestAniStarResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p := newAniStar(AniStarBase, testClient(t, "anistar"))
	episode := contracts.Episode{Num: "1", RawEmbeds: map[string][]string{"AniStar": {"https://player#1"}}}

	_, err := p.ResolveStream(context.Background(), episode, "не-озвучка")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAniStarGetEpisodesLegacyPlayer pins the second player
// generation: older releases (Black Lagoon S2, captured live
// 2026-09-20) iframe /playlist_anistar2.php?link={slug}.html — a
// cp1251 page whose #PlayList spans carry the per-episode embed URLs
// in playX('…') onclick handlers. Each span becomes one episode with
// the suffix-less AniStar dub key; the embed URL itself is the
// RawEmbeds value (no player#media_id indirection).
func TestAniStarGetEpisodesLegacyPlayer(t *testing.T) {
	t.Parallel()

	routes := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html") && strings.HasPrefix(r.URL.Path, "/2516-"):
			anistarServeFile(t, "anistar_anime_legacy.html")(w, r)
		case strings.Contains(r.URL.Path, "playlist_anistar2.php"):
			anistarServeFile(t, "anistar_playlist_legacy.html")(w, r)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	p, _ := testAniStar(t, routes)

	episodes, err := p.GetEpisodes(context.Background(),
		p.BaseURL()+"/2516-piraty-chernoy-laguny-vtoroy-sezon-black-lagoon-2nd-season.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12", len(episodes))
	}
	for i, want := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"} {
		if episodes[i].Num != want {
			t.Fatalf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	ep1 := episodes[0]
	dubs := ep1.RawEmbeds["AniStar"]
	if len(dubs) != 1 || !strings.HasPrefix(dubs[0], "https://myvi.ru/player/embed/html/") {
		t.Errorf("episode 1 AniStar embeds = %v, want the myvi embed URL", ep1.RawEmbeds)
	}
	if ep2 := episodes[1].RawEmbeds["AniStar"]; len(ep2) == 1 && !strings.HasPrefix(ep2[0], "https://vk.com/video_ext.php") {
		t.Errorf("episode 2 AniStar embeds = %v, want the vk embed URL", ep2)
	}
}

// TestAniStarResolveStreamLegacyDirect pins the legacy resolve path:
// embed URLs ride the shared extractor factory, whose direct fallback
// answers bare media URLs (here: a synthetic .mp4 embed on episode 3's
// shape — the factory settles it at quality 720 without a fetch).
func TestAniStarResolveStreamLegacyDirect(t *testing.T) {
	t.Parallel()

	p := newAniStar(AniStarBase, testClient(t, "anistar"))
	episode := contracts.Episode{Num: "1", RawEmbeds: map[string][]string{
		"AniStar": {"https://media.example/video/ep1.m3u8"},
	}}

	stream, err := p.ResolveStream(context.Background(), episode, "AniStar")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok || src.URL != "https://media.example/video/ep1.m3u8" {
		t.Fatalf("links = %v, want the direct 720 source", stream.Links)
	}
}

// TestAniStarResolveStreamLegacyUnsupportedHost pins the loud failure
// for embed hosts the shared extractor factory does not cover (vk.com,
// myvi.ru — the bulk of the legacy playlists): empty resolution
// surfaces as ErrExtractFailed, never a silent zero.
func TestAniStarResolveStreamLegacyUnsupportedHost(t *testing.T) {
	t.Parallel()

	p := newAniStar(AniStarBase, testClient(t, "anistar"))
	episode := contracts.Episode{Num: "2", RawEmbeds: map[string][]string{
		"AniStar": {"https://vk.com/video_ext.php?oid=119777155&id=159189162&hash=34f8071c7354a301&hd=3"},
	}}

	_, err := p.ResolveStream(context.Background(), episode, "AniStar")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "vk.com") {
		t.Errorf("err = %v, want the unsupported host named", err)
	}
}

// TestAniStarSearchTransportError pins the transport failure
// passthrough (dead listener — no network egress).
func TestAniStarSearchTransportError(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	p := newAniStar("http://"+dead.Addr().String(), testClient(t, "anistar"))

	if _, err := p.Search(context.Background(), "конан"); err == nil {
		t.Fatal("Search on a dead transport must fail, got nil")
	}
}
