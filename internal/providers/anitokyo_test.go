package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// Live-capture provenance: every anitokyo fixture below is a verbatim
// capture of anitokyo.tv taken 2026-09-25 (anonymous guest requests).
// PR116: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anitokyo/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to.

// TestAniTokyoSearch pins the catalog search against the real captured
// «дандадан» answer: DLE full-search rows (article.story.shortstory) in
// document order. The live answer surfaces exactly the 3 Dandadan
// releases; posters arrive as absolute atw.picmap.top URLs.
func TestAniTokyoSearch(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_search_dandadan.html")
	p := luaProvider(t, "anitokyo", srv.URL)
	results, err := p.Search(context.Background(), "дандадан")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (the captured cards)", len(results))
	}

	want := []contracts.SearchResult{
		{
			Title: "Дандадан [ТВ-3] / Dandadan 3rd Season",
			URL:   "https://anitokyo.tv/anime/9800-dandadan-tv-3-dandadan-3rd-season.html",
		},
		{
			Title: "Дандадан [ТВ-2] / Dandadan 2nd Season",
			URL:   "https://anitokyo.tv/anime/9328-dandadan-tv-2-dandadan-2nd-season.html",
		},
		{
			Title: "Дандадан [ТВ-1] / Dandadan [TV-1]",
			URL:   "https://anitokyo.tv/anime/8681-dandadan-tv-1-dandadan-tv-1.html",
		},
	}
	wantPosters := []string{
		"https://atw.picmap.top/uploads/thumbs/250x357/content/posters/2025-09/poster_1758229570069920.jpg",
		"https://atw.picmap.top/uploads/thumbs/250x357/content/posters/2024-12/poster_1735415677077730.jpg",
		"https://atw.picmap.top/uploads/thumbs/250x357/content/posters/2023-11/poster_1701140990559744.jpg",
	}
	for i, w := range want {
		got := results[i]
		if got.Title != w.Title {
			t.Errorf("results[%d].Title = %q, want %q", i, got.Title, w.Title)
		}
		if got.URL != w.URL {
			t.Errorf("results[%d].URL = %q, want %q", i, got.URL, w.URL)
		}
		if got.SourceID != "anitokyo" {
			t.Errorf("results[%d].SourceID = %q, want anitokyo", i, got.SourceID)
		}
		if got.Poster != wantPosters[i] {
			t.Errorf("results[%d].Poster = %q, want the captured poster %q", i, got.Poster, wantPosters[i])
		}
	}
}

// TestAniTokyoSearchOVASection pins the section filter: the search index
// mixes the playable release sections; the captured «твоё имя» answer
// carries one /ova/ card and it must surface (only /hentai/ and non-release
// links are dropped).
func TestAniTokyoSearchOVASection(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_search_ova.html")
	p := luaProvider(t, "anitokyo", srv.URL)
	results, err := p.Search(context.Background(), "твоё имя")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (the /ova/ card)", len(results))
	}
	if results[0].Title != "Если я назову твое имя / Kimi no Na o Yobeba" {
		t.Errorf("Title = %q", results[0].Title)
	}
	if results[0].URL != "https://anitokyo.tv/ova/3847-esli-ja-nazovu-tvoe-imja-kimi-no-na-o-yobeba.html" {
		t.Errorf("URL = %q, want the captured /ova/ release", results[0].URL)
	}
}

// TestAniTokyoSearchMiss pins the zero-result answer (the shared smoke
// probe «черная лагуна» captured live) — an empty result list, not an
// error (animemobi/anistar precedent).
func TestAniTokyoSearchMiss(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_search_miss.html")
	p := luaProvider(t, "anitokyo", srv.URL)
	results, err := p.Search(context.Background(), "черная лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

// TestAniTokyoSearchSendsDLEForm pins the load-bearing request shape: the
// DLE full-search is a POST form (do=search&subaction=search&story=…) —
// the same body the site's own <form method="post"> submits.
func TestAniTokyoSearchSendsDLEForm(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		_, _ = w.Write(fixture(t, "anitokyo_search_dandadan.html"))
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "anitokyo", srv.URL)
	if _, err := p.Search(context.Background(), "дандадан"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST (the DLE form method is load-bearing)", gotMethod)
	}
	for _, want := range []string{"do=search", "subaction=search", "story="} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body %q misses %q", gotBody, want)
		}
	}
}

// TestAniTokyoGetEpisodesTV pins the TV shape against the Dandadan TV-1
// capture: 12 episodes sorted by the item lssort, 60 dubs on episode 1,
// the AnimeVost dub carrying its /video.php?id=445222&cat=1 embed ref and
// the sibnet subtitle dub its own item id.
func TestAniTokyoGetEpisodesTV(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_anime_tv.html")
	p := luaProvider(t, "anitokyo", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/8681-dandadan-tv-1-dandadan-tv-1.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 12 {
		t.Fatalf("len(episodes) = %d, want 12 (live capture)", len(episodes))
	}
	first, last := episodes[0], episodes[11]
	if first.Num != "1" || last.Num != "12" {
		t.Errorf("nums = %q..%q, want \"1\"..\"12\" (lssort ascending)", first.Num, last.Num)
	}
	if first.Title != "1 серия" {
		t.Errorf("first.Title = %q, want the captured aname label", first.Title)
	}
	if len(first.RawEmbeds) != 60 {
		t.Fatalf("episode 1 dubs = %d, want 60 (live capture)", len(first.RawEmbeds))
	}
	ref, ok := first.RawEmbeds["AnimeVost"]
	if !ok || len(ref) != 1 || ref[0] != srv.URL+"/video.php?id=445222&cat=1" {
		t.Errorf("AnimeVost ref = %v, want [%s]", ref, srv.URL+"/video.php?id=445222&cat=1")
	}
	ref, ok = first.RawEmbeds["Субтитры „Sibnet“"]
	if !ok || len(ref) != 1 || ref[0] != srv.URL+"/video.php?id=453886&cat=1" {
		t.Errorf("sibnet dub ref = %v, want [%s]", ref, srv.URL+"/video.php?id=453886&cat=1")
	}
	// The raw_id state carrier must round-trip as JSON the streams
	// call can decode.
	if !strings.Contains(first.RawID, `"n":"1"`) || !strings.Contains(first.RawID, `"u"`) {
		t.Errorf("first.RawID = %q, want the {n,u} state JSON", first.RawID)
	}
}

// TestAniTokyoGetEpisodesMovie pins the movie shape (the SAO Unanswered
// Butterfly capture): one episode keyed 1, three dubs, embeds riding the
// movie cat=2 video.php flavor.
func TestAniTokyoGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_anime_movie.html")
	p := luaProvider(t, "anitokyo", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/movie/10143-mastera-mecha-onlajn-bezotvetnaja-babochka-sword-art-online-unanswered-butterfly.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("len(episodes) = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want \"1\"", ep.Num)
	}
	wantDubs := []string{"FumoDub", "AniStar", "Субтитры „Субтитры“"}
	for _, dub := range wantDubs {
		ref, ok := ep.RawEmbeds[dub]
		if !ok || len(ref) != 1 {
			t.Errorf("RawEmbeds[%q] = %v, want one video.php ref", dub, ref)
		}
	}
	if got := ep.RawEmbeds["FumoDub"][0]; got != srv.URL+"/video.php?id=559459&cat=2" {
		t.Errorf("FumoDub ref = %q, want the captured cat=2 URL", got)
	}
}

// TestAniTokyoGetEpisodesNoPlayer pins the typed miss: an announcement
// («Анонс») page carries no RalodePlayer data at all — that is
// contracts.ErrNotFound (the anicli.fail("not_found") flow), not an
// empty success.
func TestAniTokyoGetEpisodesNoPlayer(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_anime_anons.html")
	p := luaProvider(t, "anitokyo", srv.URL)
	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/9800-dandadan-tv-3-dandadan-3rd-season.html")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !isNotFoundErr(err) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAniTokyoSearchTransportError pins the transport failure path: a dead
// endpoint surfaces the error instead of an empty success.
func TestAniTokyoSearchTransportError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "anitokyo", "http://"+newDeadListener(t).Addr().String())
	if _, err := p.Search(context.Background(), "дандадан"); err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
}

// TestAniTokyoResolveStreamKodikRoundTrip covers the resolve branch: the
// video.php wrapper scrape feeds the shared extractor factory (through
// anicli.extract) and yields typed sources.
func TestAniTokyoResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		if r.URL.Path == "/video.php" {
			_, _ = fmt.Fprint(w, `<html><body><iframe src="/kodik/seria/1340414/e8652ad443b3ceb2057d17f8aa0b35d7/720p" allowfullscreen></iframe></body></html>`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/anime/") {
			// The release page re-parse (the fresh-sandbox streams()
			// re-fetches state.u): the (1, AnimeVost) row pointing at
			// the wrapper above.
			_, _ = fmt.Fprint(w, `<html><script>RalodePlayer.init({"A":{"name":"AnimeVost","items":{"1":{"aname":"1 серия","lssort":"1","scode":"<iframe src=\"/video.php?id=445222&cat=1\">"}}}},{})</script></html>`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "anitokyo", srv.URL)
	// raw_id state: the release page (served by the same fixture
	// server's default handler re-parses) plus the episode num.
	rawID, err := luaStateJSON(srv.URL+"/anime/8681-dandadan-tv-1-dandadan-tv-1.html", "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	episode := contracts.Episode{
		Num:   "1",
		RawID: rawID,
		RawEmbeds: map[string][]string{
			"AnimeVost": {srv.URL + "/video.php?id=445222&cat=1"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AnimeVost")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok || src.URL != "https://plain.example/x/720.m3u8" {
		t.Fatalf("Links[720] = %+v, ok=%v, want the kodik extractor result", src, ok)
	}
	if src.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8 (URL-shape labeling)", src.Type)
	}
}

// TestAniTokyoResolveStreamUnknownDub pins the typed dub miss.
func TestAniTokyoResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "anitokyo_anime_tv.html")
	p := luaProvider(t, "anitokyo", srv.URL)

	rawID, err := luaStateJSON(srv.URL+"/anime/8681-dandadan-tv-1-dandadan-tv-1.html", "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	episode := contracts.Episode{
		Num:   "1",
		RawID: rawID,
		RawEmbeds: map[string][]string{
			"AnimeVost": {srv.URL + "/video.php?id=1&cat=1"},
		},
	}

	_, err = p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !isNotFoundErr(err) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAniTokyoResolveStreamWrapperless pins the dead-wrapper path: a
// video.php answer without any player iframe is the typed extract wall,
// never a silent zero-link success.
func TestAniTokyoResolveStreamWrapperless(t *testing.T) {
	t.Parallel()

	var call int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch r.URL.Path {
		case "/video.php":
			_, _ = fmt.Fprint(w, "<html><body>ad shell, no iframe</body></html>")
		default:
			// the release page re-parse: a minimal valid blob with a
			// dub whose wrapper is the shell above
			_, _ = fmt.Fprint(w, `<html><script>RalodePlayer.init({"A":{"name":"AnimeVost","items":{"1":{"aname":"1 серия","lssort":"1","scode":"<iframe src=\"/video.php?id=1&cat=1\">"}}}},{})</script></html>`)
		}
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "anitokyo", srv.URL)
	rawID, err := luaStateJSON(srv.URL+"/anime/test.html", "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	episode := contracts.Episode{
		Num:   "1",
		RawID: rawID,
		RawEmbeds: map[string][]string{
			"AnimeVost": {srv.URL + "/video.php?id=1&cat=1"},
		},
	}

	_, err = p.ResolveStream(context.Background(), episode, "AnimeVost")
	if err == nil {
		t.Fatal("error = nil, want the typed extract failure")
	}
	if !isExtractFailedErr(err) {
		t.Errorf("error = %v, want contracts.ErrExtractFailed class", err)
	}
	_ = call
}

// TestAniTokyoProviderMeta pins the identity block: RU content language,
// SourceTypeBoth (RU voice-over with watchable video — the roster-wide
// semantics) and the roster id.
func TestAniTokyoProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anitokyo")
	if p.ID() != "anitokyo" || p.Name() != "AniTokyo" || p.BaseURL() != "https://anitokyo.tv" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok || lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %v, want ru", lc)
	}
}

// TestAniTokyoNamePreferenceRU pins the search routing (PR42 semantics):
// anitokyo.tv's DLE index matches the Cyrillic titles («дандадан» verified
// live 2026-09-25) — the provider must NOT declare the latin preference
// (animemobi/anistar precedent). The composite adapter exposes
// NamePreferenceProvider to every capability-declaring script, so the
// assertion is the REGISTRY value: NamePrefDefault (the PR116
// zero-value equivalence).
func TestAniTokyoNamePreferenceRU(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anitokyo")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefDefault {
		t.Errorf("NamePreference = %v, want NamePrefDefault (the RU group)", got)
	}
}

// TestAniTokyoSmokeQueryDeclared pins the declared live probe (PR51
// mechanism): the shared RU probe «черная лагуна» misses the catalog —
// live-verified 2026-09-25 (zero cards) — so the provider declares its
// own query, whose surface carries playable releases.
func TestAniTokyoSmokeQueryDeclared(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anitokyo")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("anitokyo must declare contracts.SmokeQueryProvider (the shared RU probe misses)")
	}
	if got := sq.SmokeQuery(); got != "дандадан" {
		t.Errorf("SmokeQuery = %q, want «дандадан»", got)
	}
}

// isExtractFailedErr reports whether err carries the
// contracts.ErrExtractFailed sentinel through the provider wrapper.
func isExtractFailedErr(err error) bool { return errors.Is(err, contracts.ErrExtractFailed) }
