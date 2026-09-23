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

// Live-capture provenance: every animemobi fixture below is a verbatim
// capture of animemobi.com taken 2026-09-23 (anonymous guest requests,
// mobile-safari User-Agent). The search fixtures ride the DLE full-search
// POST (do=search&subaction=search); the release fixtures cover the three
// observed page shapes: per-episode seria anchors (TV), a single movie
// anchor (Фильм) and a whole-season «Смотреть» anchor.

// TestAnimeMobiSearch pins the catalog search against the real captured
// "black lagoon" answer: DLE full-search rows (div.shortstory) filtered to
// the anime sections. The live answer surfaces 10 rows — 3 anime releases
// plus 7 AMV/cover/news cards the fan-out must drop (anistar's news-filter
// precedent).
func TestAnimeMobiSearch(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_search.html")
	p := newAnimeMobi(srv.URL, testClient(t, "animemobi"))
	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (the anime rows; AMV/cover cards filtered)", len(results))
	}

	want := []contracts.SearchResult{
		{
			Title: "Black Lagoon: Roberta's Blood Trail / Пираты «Чёрной лагуны»: Кровавая тропа Роберты (RUS)",
			URL:   AnimeMobiBase + "/anime-rus/ova-rus/5857-black-lagoon-robertas-blood-trail-piraty-chernoj-laguny-krovavaja-tropa-roberty-rus.html",
		},
		{
			Title: "Black Lagoon: The Second Barrage / Пираты «Черной лагуны» [ТВ-2] (RUS)",
			URL:   AnimeMobiBase + "/anime-rus/tv-rus/5856-black-lagoon-the-second-barrage-piraty-chernoj-laguny-tv-2-rus.html",
		},
		{
			Title: "Black Lagoon / Пираты «Черной лагуны» [ТВ-1] (RUS)",
			URL:   AnimeMobiBase + "/anime-rus/tv-rus/5855-black-lagoon-piraty-chernoj-laguny-tv-1-rus.html",
		},
	}
	wantPosters := []string{
		"/uploads/posts/2022-09/thumbs/1664477502_black-lagoon-robertas-blood-trail.jpg",
		"/uploads/posts/2022-09/thumbs/1664476903_black-lagoon-the-second-barrage.jpg",
		"/uploads/posts/2022-09/thumbs/1664476476_black-lagoon.jpg",
	}
	for i, w := range want {
		got := results[i]
		if got.Title != w.Title {
			t.Errorf("results[%d].Title = %q, want %q", i, got.Title, w.Title)
		}
		if got.URL != w.URL {
			t.Errorf("results[%d].URL = %q, want %q", i, got.URL, w.URL)
		}
		if got.SourceID != "animemobi" {
			t.Errorf("results[%d].SourceID = %q, want animemobi", i, got.SourceID)
		}
		if !strings.HasSuffix(got.Poster, wantPosters[i]) {
			t.Errorf("results[%d].Poster = %q, want the absolutized captured thumbnail %q", i, got.Poster, wantPosters[i])
		}
	}
}

// TestAnimeMobiSearchDesktopSkin pins the desktop-skin search answer
// (testdata/animemobi_search_desktop.html, captured live 2026-09-23 with
// the roster's default desktop User-Agent): the operator's UA decides
// which DLE skin the site renders, and the desktop skin marks result rows
// div.base/div.bheading (h1.heading) where the smartphone skin used
// div.shortstory (h2.title). Same query, same 17 answers — only the
// markup differs; the parser must surface both. Posters on this skin
// arrive as absolute URLs.
func TestAnimeMobiSearchDesktopSkin(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_search_desktop.html")
	p := newAnimeMobi(srv.URL, testClient(t, "animemobi"))

	results, err := p.Search(context.Background(), "боруто")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 9 {
		t.Fatalf("len(results) = %d, want 9 (the anime rows; the non-anime row filtered)", len(results))
	}
	first := results[0]
	if first.Title != "Boruto: Naruto Next Generations / Боруто: Следующее поколение Наруто (RUS)" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != AnimeMobiBase+"/anime-rus/tv-rus/2421-boruto-naruto-next-generations-boruto-sleduyuschee-pokolenie-naruto-rus.html" {
		t.Errorf("URL = %q", first.URL)
	}
	if !strings.HasSuffix(first.Poster, "/uploads/posts/2017-06/thumbs/1496681240_boruto-naruto-next-generations.jpg") {
		t.Errorf("Poster = %q, want the captured desktop-skin poster thumbnail", first.Poster)
	}
}

// TestAnimeMobiSearchMiss pins the zero-result answer (captured live
// 2026-09-23 with a junk query): DLE answers HTTP 200 with its «поиск не
// дал никаких результатов» banner and no rows — an empty result list, not
// an error (anistar precedent).
func TestAnimeMobiSearchMiss(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_search_miss.html")
	p := newAnimeMobi(srv.URL, testClient(t, "animemobi"))
	results, err := p.Search(context.Background(), "zzzqqqxxx")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

// TestAnimeMobiGetEpisodesTV pins the per-episode release shape against
// the Black Lagoon TV-1 capture: 12 «Серия NN» anchors, one dub
// («Многоголосый» from the «Озвучка:» field), zero-padded episode numbers
// normalized to canonical decimals, embed refs kept in page order.
func TestAnimeMobiGetEpisodesTV(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_anime_tv.html")
	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime-rus/tv-rus/5855-black-lagoon-piraty-chernoj-laguny-tv-1-rus.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 12 {
		t.Fatalf("len(episodes) = %d, want 12 (live capture)", len(episodes))
	}
	first, last := episodes[0], episodes[11]
	if first.Num != "1" || last.Num != "12" {
		t.Errorf("nums = %q..%q, want \"1\"..\"12\" (zero padding normalized)", first.Num, last.Num)
	}
	if first.Title != "Серия 01" {
		t.Errorf("first.Title = %q, want the captured anchor label", first.Title)
	}
	if len(first.RawEmbeds) != 1 {
		t.Fatalf("first.RawEmbeds = %v, want exactly one dub", first.RawEmbeds)
	}
	refs, ok := first.RawEmbeds["Многоголосый"]
	if !ok {
		t.Fatalf("dub key = %v, want «Многоголосый» (the release's «Озвучка:» value)", first.RawEmbeds)
	}
	wantRef := "https://kodikplayer.com/seria/321237/4e37e52abb25ebf86aedf4ad768a95ef/720p"
	if len(refs) != 1 || refs[0] != wantRef {
		t.Errorf("refs = %v, want [%s]", refs, wantRef)
	}
	if last.RawEmbeds["Многоголосый"][0] != "https://kodikplayer.com/seria/321248/8b7ba285675afdc821f1599810f0ac41/720p" {
		t.Errorf("last ref = %v, want the captured Серия 12 anchor", last.RawEmbeds["Многоголосый"])
	}
}

// TestAnimeMobiGetEpisodesMovie pins the movie shape (Naruto film 3
// capture): one «Фильм 01» anchor counted as episode 1, the dub name
// entity-decoded from the «Озвучка:» field ([TimaMan &amp; Lem0nka]).
func TestAnimeMobiGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_anime_movie.html")
	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime-rus/movie-rus/5619-naruto-film-3.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("len(episodes) = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want \"1\" (movies count as episode 1)", ep.Num)
	}
	if ep.Title != "Фильм 01" {
		t.Errorf("Title = %q, want the captured anchor label", ep.Title)
	}
	refs, ok := ep.RawEmbeds["TimaMan & Lem0nka"]
	if !ok || len(refs) != 1 || refs[0] != "https://aniqit.com/video/47759/e952c4d2ac0189696079700fd60dff6f/720p" {
		t.Errorf("RawEmbeds = %v, want the entity-decoded dub with the captured aniqit video ref", ep.RawEmbeds)
	}
}

// TestAnimeMobiGetEpisodesSeason pins the whole-season shape (Zhe Tian
// capture): a single «Смотреть» anchor whose href is a kodik /season/
// link covering every episode — one episode entry keyed "1", the dub
// entity-decoded ([Shoker &amp; Alice]).
func TestAnimeMobiGetEpisodesSeason(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animemobi_anime_season.html")
	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime-rus/web-rus/6292-zhe-tian.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("len(episodes) = %d, want 1 (the season anchor)", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want \"1\" (the label-free «Смотреть» anchor)", ep.Num)
	}
	refs, ok := ep.RawEmbeds["Shoker & Alice"]
	if !ok || len(refs) != 1 || refs[0] != "https://aniqit.com/season/90888/771e2fe28ae5e4cfc49cdf2cda9648b4/720p" {
		t.Errorf("RawEmbeds = %v, want the decoded dub with the captured season ref", ep.RawEmbeds)
	}
}

// TestAnimeMobiGetEpisodesNoPlayer pins the typed miss: a release page
// carrying no a.onlinevideo anchors (site news share the .html URL shape)
// is contracts.ErrNotFound, not an empty success.
func TestAnimeMobiGetEpisodesNoPlayer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body><h1>site news</h1></body></html>")
	}))
	t.Cleanup(srv.Close)

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	_, err := p.GetEpisodes(context.Background(), srv.URL+"/main/1-post1.html")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !strings.Contains(err.Error(), "no onlinevideo anchors") {
		t.Errorf("error = %v, want the anchor-miss context", err)
	}
	if !isNotFoundErr(err) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAnimeMobiSearchTransportError pins the transport failure path: a
// dead endpoint surfaces the error instead of an empty success.
func TestAnimeMobiSearchTransportError(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi("http://"+newDeadListener(t).Addr().String(), testClient(t, "animemobi"))
	if _, err := p.Search(context.Background(), "black lagoon"); err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
}

// TestAnimeMobiResolveStreamKodikRoundTrip covers the resolve branch: the
// stored kodik-family embed ref runs through the shared extractor factory
// (kodik extractor Matches aniqit AND kodikplayer hosts) and yields the
// /ftor sources, typed by URL shape.
func TestAnimeMobiResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	episode := contracts.Episode{
		Num: "1",
		RawEmbeds: map[string][]string{
			"Многоголосый": {srv.URL + "/kodik/seria/12345/xyz/720p"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Многоголосый")
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

// TestAnimeMobiResolveStreamUnknownDub pins the typed dub miss.
func TestAnimeMobiResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	episode := contracts.Episode{
		Num:       "1",
		RawEmbeds: map[string][]string{"Многоголосый": {"https://kodikplayer.com/seria/1/h/720p"}},
	}

	_, err := p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !isNotFoundErr(err) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAnimeMobiResolveStreamTransportFailsLoud pins the dead-embed path:
// an unresolvable embed surfaces the extractor-tagged transport failure
// wrapped in the provider context, never a silent zero-link answer.
func TestAnimeMobiResolveStreamTransportFailsLoud(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	episode := contracts.Episode{
		Num: "1",
		RawEmbeds: map[string][]string{
			"Многоголосый": {"//" + newDeadListener(t).Addr().String() + "/kodik/e/9"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Многоголосый")
	if err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
	if !strings.Contains(err.Error(), "extractor:kodik") {
		t.Errorf("error = %v, want extractor:kodik context on the transport failure", err)
	}
}

// TestAnimeMobiProviderMeta pins the identity block: RU content language,
// SourceTypeBoth (RU voice-over with watchable video — the roster-wide
// semantics), and the roster id.
func TestAnimeMobiProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	if p.ID() != "animemobi" || p.Name() != "AnimeMobi" || p.BaseURL() != AnimeMobiBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// TestAnimeMobiNamePreferenceRU pins the search routing (PR42 semantics):
// animemobi.com's DLE index matches the Cyrillic fragments of its
// composite titles («наруто» verified live 2026-09-23) — the provider
// stays in the RU group and must NOT declare the latin-only preference
// (anilibria-torrent precedent).
func TestAnimeMobiNamePreferenceRU(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	if _, declares := any(p).(contracts.NamePreferenceProvider); declares {
		t.Error("animemobi must stay in the RU group (no latin preference declaration)")
	}
}

// TestAnimeMobiSmokeQueryDeclared pins the declared live probe (PR51
// mechanism): the shared RU probe «черная лагуна» misses the catalog —
// the DLE word-prefix search never matches the inflected site titles
// («Пираты «Черной лагуны»», verified live 2026-09-23) — so the provider
// declares its own query, whose first surfaced result rides the
// reachable kodikplayer.com embed host.
func TestAnimeMobiSmokeQueryDeclared(t *testing.T) {
	t.Parallel()

	p := newAnimeMobi(AnimeMobiBase, testClient(t, "animemobi"))
	if _, ok := any(p).(contracts.SmokeQueryProvider); !ok {
		t.Fatal("animemobi must declare contracts.SmokeQueryProvider (the shared RU probe misses)")
	}
	if got := p.SmokeQuery(); got != "боруто" {
		t.Errorf("SmokeQuery = %q, want «боруто»", got)
	}
}

// isNotFoundErr reports whether err carries the contracts.ErrNotFound
// sentinel through the provider wrapper.
func isNotFoundErr(err error) bool {
	return errors.Is(err, contracts.ErrNotFound)
}

// serveFixture serves one testdata capture over httptest so the page
// parsing runs against the REAL bytes (the fixture host replaces
// animemobi.com in the fetch only).
func serveFixture(t *testing.T, name string) *httptest.Server {
	t.Helper()

	data := fixture(t, name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}
