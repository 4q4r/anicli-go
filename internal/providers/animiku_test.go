package providers

// [LIVE-VERIFIED 2026-09-25, re-verified 2026-10-06] Every animiku
// fixture below is a verbatim capture of beta.animiku.tokyo (anonymous
// guest requests, desktop Chrome User-Agent, no cookies). The search
// fixtures ride the DLE full-search GET (do=search&subaction=search&
// story=…); the player fixtures are the mrdeath/aaparser bridge
// answers — POST engine/ajax/controller.php?mod=anime_grabber&module=
// kodik_playlist_ajax with news_id+action=load_player — for the two
// observed shapes: the serial player (b-simple_episode__item grid) and
// the movie player (per-dub data-this_link, kodik_translates_alt).
// The 2026-10-06 re-probe answered byte-equivalent: the same 4 search
// rows (newsids 9134/8640/5706/5743), the 12-episode/2-dub serial
// grid and the 13-dub movie translator row, direct route, ~0.7s/leg.
//
// PR134: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animiku/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shift
// forced by the fresh-sandbox Lua adapter (the anitokyo precedent),
// documented here rather than hidden:
//
//   - the release state (newsid + episode num) rides RawID as the
//     {n,id} JSON object (the only state channel into the
//     per-invocation streams(raw_id, dub) call — the Go provider read
//     the refs back from RawEmbeds, which the adapter does not pass
//     into streams); RawEmbeds keeps carrying the same refs for
//     consumers, and streams() re-POSTs the bridge to re-derive them.

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

const animikuProductionBase = "https://beta.animiku.tokyo"

// TestAnimikuSearch pins the catalog search against the real captured
// «черная лагуна» answer: 4 article.news-container-chapter rows, titles
// from p.title-text, release URLs index.php?newsid=N, posters
// absolutized from the relative /uploads/ paths.
func TestAnimikuSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animiku_search.html"))
	})
	p := luaProvider(t, "animiku", srv.URL)

	results, err := p.Search(context.Background(), "черная лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4 (the live capture)", len(results))
	}

	if rec.Method != http.MethodGet {
		t.Errorf("request method = %q, want GET (the site header form is method=get)", rec.Method)
	}
	if rec.Path != "/index.php" {
		t.Errorf("request path = %q, want /index.php", rec.Path)
	}
	wantQuery := "do=search&subaction=search&story=" +
		"%D1%87%D0%B5%D1%80%D0%BD%D0%B0%D1%8F+%D0%BB%D0%B0%D0%B3%D1%83%D0%BD%D0%B0"
	if rec.Query != wantQuery {
		t.Errorf("request query = %q, want %q", rec.Query, wantQuery)
	}

	want := []struct {
		title  string
		newsid string
		poster string
	}{
		{"Пираты «Чёрной лагуны»: Второй залп", "9134", "/uploads/posts/x0une4xhgw_piraty-chernoj-laguny-vtoroj-zalp.webp"},
		{"Пираты «Чёрной лагуны»", "8640", "/uploads/posts/kzcxibtpzc_piraty-chernoj-laguny.webp"},
		{"Пираты «Черной лагуны» [ТВ-2]", "5706", "/uploads/posts/mrhncv9gqn_piraty-chernoj-laguny-tv-2.webp"},
		{"Пираты «Черной лагуны» [ТВ-1]", "5743", "/uploads/posts/aytfu3o5wn_piraty-chernoj-laguny-tv-1.webp"},
	}
	for i, w := range want {
		got := results[i]
		if got.Title != w.title {
			t.Errorf("results[%d].Title = %q, want %q", i, got.Title, w.title)
		}
		if wantURL := animikuProductionBase + "/index.php?newsid=" + w.newsid; got.URL != wantURL {
			t.Errorf("results[%d].URL = %q, want %q", i, got.URL, wantURL)
		}
		if got.SourceID != "animiku" {
			t.Errorf("results[%d].SourceID = %q, want animiku", i, got.SourceID)
		}
		if !strings.HasSuffix(got.Poster, w.poster) {
			t.Errorf("results[%d].Poster = %q, want the absolutized captured poster %q", i, got.Poster, w.poster)
		}
	}
}

// TestAnimikuSearchMiss pins the zero-result answer (captured live
// 2026-09-25 with a junk query): DLE answers HTTP 200 with no
// news-container-chapter rows — an empty result list, not an error
// (anistar/animemobi precedent).
func TestAnimikuSearchMiss(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animiku_search_miss.html"))
	})
	p := luaProvider(t, "animiku", srv.URL)

	results, err := p.Search(context.Background(), "zzzqqqxxx")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

// TestAnimikuGetEpisodesTV pins the serial shape against the Black
// Lagoon: The Second Barrage player capture: 12 episodes from the
// b-simple_episode__item grid, TWO dubs resolved from the translator
// ids (757=MC Entertainment, 2835=Silver AniAge), protocol-relative
// kodikplayer.com links kept per (episode, dub) — the dubs ride
// DIFFERENT kodik hashes (7485 vs 63728), and the sparse matrix means
// each episode keeps only the dubs that list it. The POST form is
// pinned too: news_id=9134&action=load_player on the aaparser bridge.
func TestAnimikuGetEpisodesTV(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animiku_player_tv.html"))
	})
	p := luaProvider(t, "animiku", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(),
		animikuProductionBase+"/index.php?newsid=9134")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 12 {
		t.Fatalf("len(episodes) = %d, want 12 (live capture)", len(episodes))
	}

	if rec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST (the bridge answers GET with an empty body)", rec.Method)
	}
	if rec.Path != "/engine/ajax/controller.php" {
		t.Errorf("request path = %q, want /engine/ajax/controller.php", rec.Path)
	}
	if rec.Query != "mod=anime_grabber&module=kodik_playlist_ajax" {
		t.Errorf("request query = %q, want the aaparser bridge module pair", rec.Query)
	}
	if got := strings.Join(rec.Form["news_id"], ","); got != "9134" {
		t.Errorf("news_id = %q, want 9134", got)
	}
	if got := strings.Join(rec.Form["action"], ","); got != "load_player" {
		t.Errorf("action = %q, want load_player", got)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form encoding", ct)
	}

	first := episodes[0]
	if first.Num != "1" || episodes[11].Num != "12" {
		t.Errorf("nums = %q..%q, want \"1\"..\"12\"", first.Num, episodes[11].Num)
	}
	if first.Title != "Серия 1" {
		t.Errorf("first.Title = %q, want the captured anchor label", first.Title)
	}
	// PR134 contract shift: the {n,id} state JSON rides RawID (the
	// fresh-sandbox streams state channel).
	if !strings.Contains(first.RawID, `"n":"1"`) || !strings.Contains(first.RawID, `"id":"9134"`) {
		t.Errorf("first.RawID = %q, want the {n,id} state JSON", first.RawID)
	}
	if len(first.RawEmbeds) != 2 {
		t.Fatalf("first.RawEmbeds = %v, want exactly two dubs (the sparse matrix at episode 1)", first.RawEmbeds)
	}
	wantMC := "//kodikplayer.com/serial/7485/a8616362e3d99267f8abae0962e2f0c6/720p?season=2&episode=1&only_translations=757&hide_selectors=true"
	if refs := first.RawEmbeds["MC Entertainment"]; len(refs) != 1 || refs[0] != wantMC {
		t.Errorf("MC Entertainment refs = %v, want [%s]", first.RawEmbeds["MC Entertainment"], wantMC)
	}
	wantSilver := "//kodikplayer.com/serial/63728/88eba6445f2e574ed1805c4ee7bef8d0/720p?season=2&episode=1&only_translations=2835&hide_selectors=true"
	if refs := first.RawEmbeds["Silver AniAge"]; len(refs) != 1 || refs[0] != wantSilver {
		t.Errorf("Silver AniAge refs = %v, want [%s]", first.RawEmbeds["Silver AniAge"], wantSilver)
	}

	last := episodes[11]
	wantLast := "//kodikplayer.com/serial/63728/88eba6445f2e574ed1805c4ee7bef8d0/720p?season=2&episode=12&only_translations=2835&hide_selectors=true"
	if refs := last.RawEmbeds["Silver AniAge"]; len(refs) != 1 || refs[0] != wantLast {
		t.Errorf("last Silver AniAge refs = %v, want [%s] (the dub rides its own kodik hash)", last.RawEmbeds["Silver AniAge"], wantLast)
	}
}

// TestAnimikuGetEpisodesMovie pins the movie shape against the «Твоё
// имя» player capture: no episode grid — each b-translator__item
// carries its own data-this_link (the kodik_translates_alt shape), so
// the release collapses to ONE episode keyed "1" listing every dub,
// each with its own /video/ embed.
func TestAnimikuGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animiku_player_movie.html"))
	})
	p := luaProvider(t, "animiku", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(),
		animikuProductionBase+"/index.php?newsid=3147")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("len(episodes) = %d, want 1 (movies collapse to one episode)", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want \"1\"", ep.Num)
	}
	if len(ep.RawEmbeds) != 13 {
		t.Fatalf("len(RawEmbeds) = %d, want 13 (the live capture's translator row)", len(ep.RawEmbeds))
	}
	wantRef := "//kodikplayer.com/video/20648/322de8515166076a5f361d2a5504f4b7/720p?translations=false&only_translations=737"
	if refs := ep.RawEmbeds["AlexFilm"]; len(refs) != 1 || refs[0] != wantRef {
		t.Errorf("AlexFilm refs = %v, want [%s]", ep.RawEmbeds["AlexFilm"], wantRef)
	}
	for _, dub := range []string{"AniLibria.TV", "SHIZA Project", "Дублированный", "Мосфильм"} {
		if _, ok := ep.RawEmbeds[dub]; !ok {
			t.Errorf("RawEmbeds missing dub %q (have %v)", dub, dubNames(ep.RawEmbeds))
		}
	}
}

// TestAnimikuGetEpisodesNoNewsID pins the typed input error: a release
// URL without the newsid query param (the only id shape this catalog
// uses — friendly URLs do not exist for releases) is the invalid-input
// sentinel, never a blind request.
func TestAnimikuGetEpisodesNoNewsID(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animiku_player_tv.html"))
	})
	p := luaProvider(t, "animiku", srv.URL)

	_, err := p.GetEpisodes(context.Background(), animikuProductionBase+"/")
	if err == nil {
		t.Fatal("error = nil, want the typed invalid-input")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("error = %v, want contracts.ErrInvalidInput class", err)
	}
}

// TestAnimikuGetEpisodesEmptyPlayer pins the typed miss: a player
// answer with neither an episode grid nor per-dub links (a news page or
// a stripped release) is contracts.ErrNotFound, not an empty success.
func TestAnimikuGetEpisodesEmptyPlayer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="b-translators__block"><ul id="translators-list"></ul></div>`)
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "animiku", srv.URL)
	_, err := p.GetEpisodes(context.Background(), animikuProductionBase+"/index.php?newsid=1")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAnimikuSearchTransportError pins the transport failure path: a
// dead endpoint surfaces the error instead of an empty success.
func TestAnimikuSearchTransportError(t *testing.T) {
	t.Parallel()

	p := luaProvider(t, "animiku", "http://"+newDeadListener(t).Addr().String())
	if _, err := p.Search(context.Background(), "черная лагуна"); err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
}

// animikuDubWorld serves the bridge answer with ONE episode carrying
// TWO dubs (the roundtrip world, two translators wide) — the minimal
// carrier-scan surface.
func animikuDubWorld(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/engine/ajax/controller.php" {
			_, _ = fmt.Fprint(w,
				`<li class="b-translator__item" data-this_translator="757">MC Entertainment</li>`+
					`<li class="b-translator__item" data-this_translator="2835">Silver AniAge</li>`+
					`<li class="b-simple_episode__item" data-this_episode="1" data-this_translator="757" data-this_link="//kodikplayer.com/serial/7485/h/720p?episode=1">Серия 1</li>`+
					`<li class="b-simple_episode__item" data-this_episode="1" data-this_translator="2835" data-this_link="//kodikplayer.com/serial/7486/h/720p?episode=1">Серия 1</li>`)
			return
		}
		_, _ = fmt.Fprint(w, `<html></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnimikuResolveStreamDubMissTypesCarriers pins the ask-first
// dub-miss doctrine (#159 port): a dub the episode does not carry
// walls typed ErrNotFound whose message names the requested dub and
// LISTS the dubs the episode actually carries (the g.refs keys,
// byte-sorted — a Lua map has no order). The stable marker
// `carries no dub "X" (episode dubs: …)` is what the tui
// dubNotCarriedFailure predicate keys on.
func TestAnimikuResolveStreamDubMissTypesCarriers(t *testing.T) {
	t.Parallel()

	srv := animikuDubWorld(t)
	p := luaProvider(t, "animiku", srv.URL)

	episode := contracts.Episode{Num: "1", RawID: `{"id":"9134","n":"1"}`}
	_, err := p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (never a silent dub substitution)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `carries no dub "NoSuchDub"`) {
		t.Errorf("message = %q, want the requested dub named", msg)
	}
	if got := "(episode dubs: MC Entertainment, Silver AniAge)"; !strings.Contains(msg, got) {
		t.Errorf("message = %q, want %q (the sorted carrier list)", msg, got)
	}
}

// TestAnimikuResolveStreamZeroDubsIsTypedWall pins the zero-dubs
// edge: an episode the bridge answer no longer lists (the grid moved
// on) walls typed ErrNotFound with the marker but NO carrier list.
func TestAnimikuResolveStreamZeroDubsIsTypedWall(t *testing.T) {
	t.Parallel()

	// First answer carries episodes 1–2; the resolve-time re-answer
	// dropped episode 1 entirely (the fresh-sandbox re-derive sees
	// the rotated grid).
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/engine/ajax/controller.php" {
			_, _ = fmt.Fprint(w, `<li class="b-translator__item" data-this_translator="757">MC Entertainment</li>`+
				`<li class="b-simple_episode__item" data-this_episode="2" data-this_translator="757" data-this_link="`+srv.URL+`/kodik/serial/7485/h/720p?episode=2">Серия 2</li>`)
			return
		}
		_, _ = fmt.Fprint(w, `<html></html>`)
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "animiku", srv.URL)

	episode := contracts.Episode{Num: "1", RawID: `{"id":"9134","n":"1"}`}
	_, err := p.ResolveStream(context.Background(), episode, "MC Entertainment")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (the zero-dubs wall)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "carries no dub") {
		t.Errorf("message = %q, want the typed marker", msg)
	}
	if strings.Contains(msg, "episode dubs:") {
		t.Errorf("message = %q, want no carrier list on the zero-dubs wall", msg)
	}
}

// TestAnimikuResolveStreamGarbageStateIsTypedWall pins the typed wall
// for ANY raw_id byte sequence (the #157 class): merged-convention
// prefix bytes, plain non-json text, state JSON missing the id leg
// and the empty id must surface as ErrInvalidInput — never the raw
// json.decode VM error through to the user.
func TestAnimikuResolveStreamGarbageStateIsTypedWall(t *testing.T) {
	t.Parallel()

	srv := animikuDubWorld(t)
	p := luaProvider(t, "animiku", srv.URL)

	cases := []struct {
		name  string
		rawID string
	}{
		{"merged-convention prefix bytes", `animiku:{"n":"1","id":"9134"}`},
		{"plain non-json text", "about:blank"},
		{"state json missing the id leg", `{"n":"1"}`},
		{"state json of the wrong shape", `[1,2,3]`},
		{"empty raw id", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.ResolveStream(context.Background(),
				contracts.Episode{Num: "1", RawID: tc.rawID, RawEmbeds: map[string][]string{}}, "MC Entertainment")
			if !errors.Is(err, contracts.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestAnimikuResolveStreamKodikRoundTrip covers the resolve branch:
// streams() re-POSTs the bridge from the {n,id} RawID state (the
// fresh-sandbox re-derive), picks the dub's protocol-relative kodik
// ref and runs it through the shared extractor factory via
// anicli.extract (Matches kodikplayer.com), yielding the /ftor
// sources typed by URL shape.
func TestAnimikuResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/engine/ajax/controller.php" {
			// The bridge re-answer the fresh-sandbox streams() call
			// re-derives from: one episode, one dub, the test-server
			// hosted kodik embed (tests never touch the real network —
			// the host must carry the "kodik" substring the extractor
			// Matches; the animemobi round-trip pattern).
			_, _ = fmt.Fprint(w, `<li class="b-translator__item" data-this_translator="757">MC Entertainment</li>`+
				`<li class="b-simple_episode__item" data-this_episode="1" data-this_translator="757" data-this_link="`+srv.URL+`/kodik/serial/7485/h/720p?season=2&amp;episode=1">Серия 1</li>`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "animiku", srv.URL)
	// The state JSON exactly episodes() encodes: {n,id}, key order
	// free (json.Marshal sorts).
	episode := contracts.Episode{
		Num:   "1",
		RawID: `{"id":"9134","n":"1"}`,
		RawEmbeds: map[string][]string{
			"MC Entertainment": {srv.URL + "/kodik/serial/7485/h/720p?season=2&episode=1"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "MC Entertainment")
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

// TestAnimikuResolveStreamUnknownDub pins the typed dub miss: a dub
// the bridge answer does not list on the episode is the not-found
// sentinel, resolved BEFORE any extractor fetch.
func TestAnimikuResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<li class="b-translator__item" data-this_translator="757">MC Entertainment</li>`+
			`<li class="b-simple_episode__item" data-this_episode="1" data-this_translator="757" data-this_link="//kodikplayer.com/serial/7485/h/720p">Серия 1</li>`)
	}))
	t.Cleanup(srv.Close)

	p := luaProvider(t, "animiku", srv.URL)
	episode := contracts.Episode{
		Num:   "1",
		RawID: `{"id":"9134","n":"1"}`,
		RawEmbeds: map[string][]string{
			"MC Entertainment": {"//kodikplayer.com/serial/7485/h/720p"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "NoSuchDub")
	if err == nil {
		t.Fatal("error = nil, want the typed not-found")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want contracts.ErrNotFound class", err)
	}
}

// TestAnimikuProviderMeta pins the identity block: RU content language,
// SourceTypeBoth (RU voice-over with watchable video — the roster-wide
// semantics), and the roster id.
func TestAnimikuProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animiku")
	if p.ID() != "animiku" || p.Name() != "AniMiku" || p.BaseURL() != animikuProductionBase {
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

// TestAnimikuNamePreferenceRU pins the search routing (PR42 semantics):
// the DLE index matches Cyrillic word prefixes («черная лагуна»
// surfaced 4 rows live 2026-09-25, е/ё-equivalence included) — the
// provider stays in the RU group and must not declare the latin-only
// preference (anilibria-torrent precedent). The capability adapter
// keeps the surface assertions-stable (the animeheaven precedent).
func TestAnimikuNamePreferenceRU(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animiku")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got == contracts.NamePrefLatin {
		t.Error("animiku must stay in the RU group (no latin preference declaration)")
	}
}

// TestAnimikuSmokeQueryShared pins the probe routing: the shared RU
// smoke probe «черная лагуна» HITS this catalog (4 live rows, first
// resolves through the reachable kodikplayer.com embeds), so the
// script must not declare a probe of its own — the adapter-declared
// capability answers empty and the shared probe applies (PR51
// semantics; the animeheaven precedent).
func TestAnimikuSmokeQueryShared(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animiku")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the content_lang adapter must keep the capability surface assertions-stable")
	}
	if got := sq.SmokeQuery(); got != "" {
		t.Errorf("SmokeQuery = %q, want empty (the shared RU probe applies)", got)
	}
}

// dubNames lists a RawEmbeds map's keys (test diagnostics helper).
func dubNames(embeds map[string][]string) []string {
	out := make([]string, 0, len(embeds))
	for k := range embeds {
		out = append(out, k)
	}
	return out
}
