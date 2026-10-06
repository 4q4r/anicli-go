package providers

// The fixtures below are REAL captures of anistar.org (2026-09-20,
// plain curl with a browser UA — the edge clears browser fingerprints):
//
//	anistar_search.html          POST / with do=search&subaction=
//	                             search&story=наруто (cp1251 bytes;
//	                             10 raw cards, 3 /anime/ releases)
//	anistar_search_miss.html     the junk-query zero-result shell
//	anistar_anime.html           the #movie_video p2p player block
//	                             (Detective Conan TV, id=5590)
//	anistar_player_series.html   the var playlst array (8 entries:
//	                             «Серия 1/2» + «Многоголосая
//	                             озвучка» siblings + the one-off
//	                             (Zendos)/(OVERLORDS) teams)
//	anistar_player_movie.html    the single «Фильм» entry (Conan
//	                             Pentagramma, media_id 52940)
//	anistar_anime_legacy.html    the legacy playlist_anistar2 iframe
//	                             (Black Lagoon S2)
//	anistar_playlist_legacy.html the #PlayList spans (12 myvi/vk
//	                             embed URLs)
//
// The cp1251 fixtures keep their raw bytes; the fixture server serves
// them verbatim and the SCRIPT decodes (anicli.iconv) — the wire shape
// the production site serves.
//
// PR138: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anistar/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shifts
// forced by the fresh-sandbox Lua adapter (the sameband/anifilm
// precedent), documented here rather than hidden:
//
//   - streams(raw_id, dub) receives no RawEmbeds map, so the resolve
//     state (player URL + per-dub media_id references for the p2p
//     generation, the per-dub embed URLs for the legacy one) rides the
//     RawID state JSON the episodes() leg builds; RawEmbeds keeps
//     carrying the player#media_id / embed references for consumers.
//   - the internal helpers (anistarParsePlaylst, anistarSplitTitle,
//     anistarStreamHeaders, anistarStreamType) were Go-internal units;
//     their observable behavior — first-seen episode order, the
//     suffix→dub rule with the AniStar fallback, the load-bearing
//     site-root Referer on every source, the .m3u8→m3u8/mp4 label
//     rule, the files[]-wins precedence — stays pinned below through
//     the public surface.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"golang.org/x/text/encoding/charmap"
)

// anistarFormGet reads one form field from the recorded POST body
// (rec.Form is the plain map the shared fixture server records).
func anistarFormGet(rec *recordedRequest, key string) string {
	if vals, ok := rec.Form[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// anistarRoute is the shared fixture router: the release-page and
// player-page shapes both ride the one test origin live, so the test
// routes on the path suffix. Overrides via the map (fixture name per
// route key: ".html" and the player script paths).
func anistarRoute(t *testing.T, pages map[string]string) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html") && pages[".html"] != "":
			_, _ = w.Write(fixture(t, pages[".html"]))
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php") && pages["videoas_p2p_new.php"] != "":
			_, _ = w.Write(fixture(t, pages["videoas_p2p_new.php"]))
		case strings.Contains(r.URL.Path, "playlist_anistar2.php") && pages["playlist_anistar2.php"] != "":
			_, _ = w.Write(fixture(t, pages["playlist_anistar2.php"]))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestAniStarProviderMeta pins the service-level identity: registration
// identity, BOTH content semantics (RU voice-overs over present
// video), the RU content language, the declared live probe, and the
// capability the provider deliberately does NOT declare: the RU-default
// query routing needs no NamePreference (yummy precedent).
func TestAniStarProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anistar")
	if p.ID() != "anistar" || p.Name() != "AniStar" {
		t.Errorf("identity = %q/%q, want anistar/AniStar", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://anistar.org" {
		t.Errorf("BaseURL = %q, want the site root", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	cl, ok := p.(interface{ ContentLanguage() string })
	if !ok || cl.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage declared=%v, want the ru declaration", ok)
	}
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatalf("SmokeQueryProvider not declared: the shared RU probe lands on the legacy vk/myvi embed generation no extractor covers")
	}
	if got := sq.SmokeQuery(); got != "боруто" {
		t.Errorf("SmokeQuery = %q, want боруто (the p2p-generation probe)", got)
	}
	// The Lua capability composite carries the NamePreference method
	// on every adapted provider (caps.go: the zero value is
	// observationally identical to not-implemented at every consumer),
	// so the pin is the VALUE: RU is the default routing, the script
	// declares no name_preference.
	if np, ok := p.(contracts.NamePreferenceProvider); ok {
		if got := np.NamePreference(); got != contracts.NamePrefDefault {
			t.Errorf("NamePreference = %v, want the default (RU is the default routing)", got)
		}
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

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anistar_search.html"))
	})
	p := luaProvider(t, "anistar", srv.URL)

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
	if !strings.HasSuffix(first.Poster, "/uploads/posters/9190/original.jpg") {
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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anistar_search_miss.html"))
	})
	p := luaProvider(t, "anistar", srv.URL)

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

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":               "anistar_anime.html",
		"videoas_p2p_new.php": "anistar_player_series.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
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

// TestAniStarGetEpisodesPlayerReferer pins the player fetch's Referer:
// the player page rides the release page URL (the iframe context the
// browser sends).
func TestAniStarGetEpisodesPlayerReferer(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			_, _ = w.Write(fixture(t, "anistar_anime.html"))
		case strings.Contains(r.URL.Path, "videoas_p2p_new.php"):
			if got := r.Header.Get("Referer"); !strings.Contains(got, "/5590-detektiv-konan") {
				t.Errorf("player request Referer = %q, want the release page", got)
			}
			_, _ = w.Write(fixture(t, "anistar_player_series.html"))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
	if _, err := p.GetEpisodes(context.Background(), animeURL); err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
}

// TestAniStarGetEpisodesMovie pins the movie shape: the single
// «Фильм» playlst entry becomes episode 1 (the movie numbering
// convention).
func TestAniStarGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":               "anistar_anime.html",
		"videoas_p2p_new.php": "anistar_player_movie.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/10156-detektiv-konan-pentagramma.html"
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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>новость без плеера</body></html>"))
	})
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/7748-skuchaesh-po-naruto.html"
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

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":               "anistar_anime.html",
		"videoas_p2p_new.php": "anistar_player_series.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
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

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":               "anistar_anime.html",
		"videoas_p2p_new.php": "anistar_player_movie.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/10156-detektiv-konan-pentagramma.html"
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
// from the episode's references fails loud with ErrNotFound.
func TestAniStarResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":               "anistar_anime.html",
		"videoas_p2p_new.php": "anistar_player_series.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	animeURL := srv.URL + "/5590-detektiv-konan-detective-conan-meitantei-conan.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	_, err = p.ResolveStream(context.Background(), episodes[0], "не-озвучка")
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

	srv, _ := fixtureServer(t, anistarRoute(t, map[string]string{
		".html":                 "anistar_anime_legacy.html",
		"playlist_anistar2.php": "anistar_playlist_legacy.html",
	}))
	p := luaProvider(t, "anistar", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(),
		srv.URL+"/2516-piraty-chernoy-laguny-vtoroy-sezon-black-lagoon-2nd-season.html")
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

// TestAniStarLegacySpanRefsUnescaped is the PR82 review-nit #9 proof:
// legacy playlist spans carry HTML-entity-encoded URLs (&amp; for & in
// vk query strings). The ref must be unescaped BEFORE it rides into
// RawEmbeds — the entity-encoded form mis-resolves at the extractor
// factory (mangled query keys). Plain-& URLs are untouched.
func TestAniStarLegacySpanRefsUnescaped(t *testing.T) {
	t.Parallel()

	// «Серия» in cp1251 (D1 E5 F0 E8 FF) — the fixture bodies ride the
	// production cp1251 decoder, ASCII passes through untouched.
	serija := string([]byte{0xD1, 0xE5, 0xF0, 0xE8, 0xFF})

	animePage := `<html><body><div id="movie_video"><iframe src="/playlist_anistar2.php?link=testslug.html"></iframe></div></body></html>`
	playlistPage := `<html><body><div id="PlayList">` +
		`<span id="link" onclick="playvk('https://vk.com/video_ext.php?oid=119777155&amp;id=159189162&amp;hash=34f8071c' , this)">` + serija + ` 1</span>` +
		`<span id="link" onclick="playvk('https://vk.com/video_ext.php?oid=119777155&id=159189163&hash=abc' , this)">` + serija + ` 2</span>` +
		`</div></body></html>`

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			_, _ = w.Write([]byte(animePage))
		case strings.Contains(r.URL.Path, "playlist_anistar2.php"):
			_, _ = w.Write([]byte(playlistPage))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	p := luaProvider(t, "anistar", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/testslug.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	want := "https://vk.com/video_ext.php?oid=119777155&id=159189162&hash=34f8071c"
	if got := episodes[0].RawEmbeds["AniStar"][0]; got != want {
		t.Fatalf("episode 1 embed = %q, want the &-decoded %q", got, want)
	}
	// Plain-& URLs pass through unchanged (idempotent unescape).
	want2 := "https://vk.com/video_ext.php?oid=119777155&id=159189163&hash=abc"
	if got := episodes[1].RawEmbeds["AniStar"][0]; got != want2 {
		t.Fatalf("episode 2 embed = %q, want %q", got, want2)
	}
}

// TestAniStarResolveStreamLegacyDirect pins the legacy resolve path:
// embed URLs ride the shared extractor factory, whose direct fallback
// answers bare media URLs (here: a synthetic .mp4 embed on episode 3's
// shape — the factory settles it at quality 720 without a fetch).
func TestAniStarResolveStreamLegacyDirect(t *testing.T) {
	t.Parallel()

	serija := string([]byte{0xD1, 0xE5, 0xF0, 0xE8, 0xFF})
	animePage := `<html><body><div id="movie_video"><iframe src="/playlist_anistar2.php?link=testslug.html"></iframe></div></body></html>`
	playlistPage := `<html><body><div id="PlayList">` +
		`<span id="link" onclick="playvk('https://media.example/video/ep1.m3u8' , this)">` + serija + ` 1</span>` +
		`</div></body></html>`

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			_, _ = w.Write([]byte(animePage))
		case strings.Contains(r.URL.Path, "playlist_anistar2.php"):
			_, _ = w.Write([]byte(playlistPage))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	p := luaProvider(t, "anistar", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/testslug.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), episodes[0], "AniStar")
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

	serija := string([]byte{0xD1, 0xE5, 0xF0, 0xE8, 0xFF})
	animePage := `<html><body><div id="movie_video"><iframe src="/playlist_anistar2.php?link=testslug.html"></iframe></div></body></html>`
	playlistPage := `<html><body><div id="PlayList">` +
		`<span id="link" onclick="playvk('https://vk.com/video_ext.php?oid=119777155&id=159189162&hash=34f8071c7354a301&hd=3' , this)">` + serija + ` 2</span>` +
		`</div></body></html>`

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".html"):
			_, _ = w.Write([]byte(animePage))
		case strings.Contains(r.URL.Path, "playlist_anistar2.php"):
			_, _ = w.Write([]byte(playlistPage))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	p := luaProvider(t, "anistar", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/testslug.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	_, err = p.ResolveStream(context.Background(), episodes[0], "AniStar")
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

	p := luaProvider(t, "anistar", "http://"+newDeadListener(t).Addr().String())

	if _, err := p.Search(context.Background(), "конан"); err == nil {
		t.Fatal("Search on a dead transport must fail, got nil")
	}
}
