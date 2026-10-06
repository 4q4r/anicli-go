package providers

// Live-capture provenance: every animemobi fixture below is a verbatim
// capture of animemobi.com taken 2026-09-23 (anonymous guest requests,
// mobile-safari User-Agent). The search fixtures ride the DLE full-search
// POST (do=search&subaction=search); the release fixtures cover the three
// observed page shapes: per-episode seria anchors (TV), a single movie
// anchor (Фильм) and a whole-season «Смотреть» anchor.
//
// PR137: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animemobi/main.lua) — these tests
// pin the script through the same contracts.Provider surface and the
// same fixtures the compiled Go implementation was held to. Contract
// shift forced by the fresh-sandbox Lua adapter (the anikado/anifilm
// precedent), documented here rather than hidden:
//
//   - the episode's embed ref rides episode RawID alone (the {n, d, r}
//     state JSON — the only state channel into the per-invocation
//     streams(raw_id, dub) call); RawEmbeds keeps carrying the same
//     dub → ref map for consumers. The unknown-dub wall stays a typed
//     not-found: the state carries the credited dub, and a dub the
//     episode does not carry fails exactly like the compiled
//     provider's RawEmbeds miss.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// animemobiBase is the production base_url literal the script pins
// (the fixture pages carry the production domain in their absolute
// links, so expectations keep it).
const animemobiBase = "https://animemobi.com"

// animemobiStateJSON builds the {n, d, r} state JSON the script
// encodes into raw_id (the fresh-sandbox streams() state carrier: the
// episode number, the credited dub and the embed ref).
func animemobiStateJSON(t *testing.T, num, dub, ref string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"n": num, "d": dub, "r": ref})
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	return string(b)
}

// TestAnimeMobiSearch pins the catalog search against the real captured
// "black lagoon" answer: DLE full-search rows (div.shortstory) filtered to
// the anime sections. The live answer surfaces 10 rows — 3 anime releases
// plus 7 AMV/cover/news cards the fan-out must drop (anistar's news-filter
// precedent).
func TestAnimeMobiSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_search.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The DLE full-search POST rides the site root: the form fields
	// are the documented search contract, UTF-8 (unlike anistar's
	// cp1251 form).
	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST (the DLE full-search form)", rec.Method)
	}
	if rec.Path != "/" {
		t.Errorf("request path = %q, want / (the form posts the site root)", rec.Path)
	}
	if got := rec.Form["do"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("do form field = %q, want search", got)
	}
	if got := rec.Form["subaction"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("subaction form field = %q, want search", got)
	}
	if got := rec.Form["story"]; len(got) != 1 || got[0] != "black lagoon" {
		t.Errorf("story form field = %q, want the raw query", got)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form encoding", ct)
	}

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (the anime rows; AMV/cover cards filtered)", len(results))
	}

	want := []contracts.SearchResult{
		{
			Title: "Black Lagoon: Roberta's Blood Trail / Пираты «Чёрной лагуны»: Кровавая тропа Роберты (RUS)",
			URL:   animemobiBase + "/anime-rus/ova-rus/5857-black-lagoon-robertas-blood-trail-piraty-chernoj-laguny-krovavaja-tropa-roberty-rus.html",
		},
		{
			Title: "Black Lagoon: The Second Barrage / Пираты «Черной лагуны» [ТВ-2] (RUS)",
			URL:   animemobiBase + "/anime-rus/tv-rus/5856-black-lagoon-the-second-barrage-piraty-chernoj-laguny-tv-2-rus.html",
		},
		{
			Title: "Black Lagoon / Пираты «Черной лагуны» [ТВ-1] (RUS)",
			URL:   animemobiBase + "/anime-rus/tv-rus/5855-black-lagoon-piraty-chernoj-laguny-tv-1-rus.html",
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
// div.shortstory (h2.title). Same query, same answers — only the
// markup differs; the parser must surface both.
func TestAnimeMobiSearchDesktopSkin(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_search_desktop.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

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
	if first.URL != animemobiBase+"/anime-rus/tv-rus/2421-boruto-naruto-next-generations-boruto-sleduyuschee-pokolenie-naruto-rus.html" {
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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_search_miss.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_anime_tv.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

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

	// The {n, d, r} state JSON rides RawID: the episode number, the
	// credited dub and the embed ref — the fresh-sandbox streams()
	// channel (the anikado {n,u} precedent).
	for _, ep := range []contracts.Episode{first, last} {
		var state struct {
			N string `json:"n"`
			D string `json:"d"`
			R string `json:"r"`
		}
		if err := json.Unmarshal([]byte(ep.RawID), &state); err != nil {
			t.Fatalf("episode %s RawID = %q, want the {n,d,r} state JSON: %v", ep.Num, ep.RawID, err)
		}
		if state.N != ep.Num || state.D != "Многоголосый" {
			t.Errorf("episode %s state = {n:%q d:%q}, want {n:%q d:Многоголосый}", ep.Num, state.N, state.D, ep.Num)
		}
		if state.R != ep.RawEmbeds["Многоголосый"][0] {
			t.Errorf("episode %s state r = %q, want the episode's embed ref", ep.Num, state.R)
		}
	}
}

// TestAnimeMobiGetEpisodesMovie pins the movie shape (Naruto film 3
// capture): one «Фильм 01» anchor counted as episode 1, the dub name
// entity-decoded from the «Озвучка:» field ([TimaMan &amp; Lem0nka]).
func TestAnimeMobiGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_anime_movie.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animemobi_anime_season.html"))
	})
	p := luaProvider(t, "animemobi", srv.URL)

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

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body><h1>site news</h1></body></html>")
	})
	p := luaProvider(t, "animemobi", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/main/1-post1.html")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !strings.Contains(err.Error(), "no onlinevideo anchors") {
		t.Errorf("error = %v, want the anchor-miss context", err)
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "animemobi" {
		t.Errorf("error = %v, want an animemobi ProviderError", err)
	}
}

// TestAnimeMobiSearchTransportError pins the transport failure path: a
// dead endpoint surfaces the error instead of an empty success.
func TestAnimeMobiSearchTransportError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "animemobi", "http://"+newDeadListener(t).Addr().String())
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

	p := luaProvider(t, "animemobi", srv.URL)
	ref := srv.URL + "/kodik/seria/12345/xyz/720p"
	ep := contracts.Episode{
		Num:   "1",
		RawID: animemobiStateJSON(t, "1", "Многоголосый", ref),
	}

	stream, err := p.ResolveStream(context.Background(), ep, "Многоголосый")
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
	if stream.DubName != "Многоголосый" {
		t.Errorf("DubName = %q, want the requested dub", stream.DubName)
	}
}

// TestAnimeMobiResolveStreamUnknownDub pins the typed dub miss: a dub the
// episode does not carry is the compiled provider's RawEmbeds miss — the
// state JSON carries the credited dub, and a mismatch fails not-found
// without touching the network.
func TestAnimeMobiResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the unknown-dub wall must not fetch anything (the state carries the ref)")
		http.Error(w, "no fetch expected", http.StatusInternalServerError)
	})
	p := luaProvider(t, "animemobi", srv.URL)

	ep := contracts.Episode{
		Num:   "1",
		RawID: animemobiStateJSON(t, "1", "Многоголосый", "https://kodikplayer.com/seria/1/h/720p"),
	}

	_, err := p.ResolveStream(context.Background(), ep, "NoSuchDub")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAnimeMobiResolveStreamTransportFailsLoud pins the dead-embed path:
// an unresolvable embed surfaces the extractor-tagged transport failure
// wrapped in the provider context, never a silent zero-link answer.
func TestAnimeMobiResolveStreamTransportFailsLoud(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "animemobi", animemobiBase)
	ref := "//" + newDeadListener(t).Addr().String() + "/kodik/e/9"
	ep := contracts.Episode{
		Num:   "1",
		RawID: animemobiStateJSON(t, "1", "Многоголосый", ref),
	}

	_, err := p.ResolveStream(context.Background(), ep, "Многоголосый")
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

	p := luaProviderAtProduction(t, "animemobi")
	if p.ID() != "animemobi" || p.Name() != "AnimeMobi" || p.BaseURL() != animemobiBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the animemobi script lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", got)
	}
}

// TestAnimeMobiNamePreferenceRU pins the search routing (PR42 semantics):
// animemobi.com's DLE index matches the Cyrillic fragments of its
// composite titles («наруто» verified live 2026-09-23) — the provider
// stays in the RU group and must NOT declare the latin-only preference
// (anilibria-torrent precedent). Under the capability adapter the
// declaration surface answers the default (anilibria precedent).
func TestAnimeMobiNamePreferenceRU(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animemobi")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the content_lang adapter must keep the capability surface assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefDefault {
		t.Errorf("NamePreference = %v, want NamePrefDefault (the RU group)", got)
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

	p := luaProviderAtProduction(t, "animemobi")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("animemobi must declare contracts.SmokeQueryProvider (the shared RU probe misses)")
	}
	if got := sq.SmokeQuery(); got != "боруто" {
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
// animemobi.com in the fetch only). Shared with the anitokyo pins.
func serveFixture(t *testing.T, name string) *httptest.Server {
	t.Helper()

	data := fixture(t, name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}
