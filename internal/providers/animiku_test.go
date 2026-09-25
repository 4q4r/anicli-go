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

// Live-capture provenance: every animiku fixture below is a verbatim
// capture of beta.animiku.tokyo taken 2026-09-25 (anonymous guest
// requests, desktop Chrome User-Agent, no cookies). The search fixtures
// ride the DLE full-search GET (do=search&subaction=search&story=…);
// the player fixtures are the mrdeath/aaparser bridge answers —
// POST engine/ajax/controller.php?mod=anime_grabber&module=
// kodik_playlist_ajax with news_id+action=load_player — for the two
// observed shapes: the serial player (b-simple_episode__item grid) and
// the movie player (per-dub data-this_link, kodik_translates_alt).

// TestAnimikuSearch pins the catalog search against the real captured
// «черная лагуна» answer: 4 article.news-container-chapter rows, titles
// from p.title-text, release URLs index.php?newsid=N, posters
// absolutized from the relative /uploads/ paths.
func TestAnimikuSearch(t *testing.T) {
	t.Parallel()

	srv := serveFixture(t, "animiku_search.html")
	p := newAnimiku(srv.URL, testClient(t, "animiku"))
	results, err := p.Search(context.Background(), "черная лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("len(results) = %d, want 4 (the live capture)", len(results))
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
		if wantURL := AniMikuBase + "/index.php?newsid=" + w.newsid; got.URL != wantURL {
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

	srv := serveFixture(t, "animiku_search_miss.html")
	p := newAnimiku(srv.URL, testClient(t, "animiku"))
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
	p := newAnimiku(srv.URL, testClient(t, "animiku"))

	episodes, err := p.GetEpisodes(context.Background(), AniMikuBase+"/index.php?newsid=9134")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 12 {
		t.Fatalf("len(episodes) = %d, want 12 (live capture)", len(episodes))
	}

	if rec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST (the bridge answers GET with an empty body)", rec.Method)
	}
	if got := strings.Join(rec.Form["news_id"], ","); got != "9134" {
		t.Errorf("news_id = %q, want 9134", got)
	}
	if got := strings.Join(rec.Form["action"], ","); got != "load_player" {
		t.Errorf("action = %q, want load_player", got)
	}

	first := episodes[0]
	if first.Num != "1" || episodes[11].Num != "12" {
		t.Errorf("nums = %q..%q, want \"1\"..\"12\"", first.Num, episodes[11].Num)
	}
	if first.Title != "Серия 1" {
		t.Errorf("first.Title = %q, want the captured anchor label", first.Title)
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

	srv := serveFixture(t, "animiku_player_movie.html")
	p := newAnimiku(srv.URL, testClient(t, "animiku"))

	episodes, err := p.GetEpisodes(context.Background(), AniMikuBase+"/index.php?newsid=3147")
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

	srv := serveFixture(t, "animiku_player_tv.html")
	p := newAnimiku(srv.URL, testClient(t, "animiku"))

	_, err := p.GetEpisodes(context.Background(), AniMikuBase+"/")
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

	p := newAnimiku(srv.URL, testClient(t, "animiku"))
	_, err := p.GetEpisodes(context.Background(), AniMikuBase+"/index.php?newsid=1")
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

	p := newAnimiku("http://"+newDeadListener(t).Addr().String(), testClient(t, "animiku"))
	if _, err := p.Search(context.Background(), "черная лагуна"); err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
}

// TestAnimikuResolveStreamKodikRoundTrip covers the resolve branch: the
// stored protocol-relative kodik embed ref runs through the shared
// extractor factory (Matches kodikplayer.com) and yields the /ftor
// sources, typed by URL shape.
func TestAnimikuResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	}))
	t.Cleanup(srv.Close)

	p := newAnimiku(AniMikuBase, testClient(t, "animiku"))
	episode := contracts.Episode{
		Num: "1",
		RawEmbeds: map[string][]string{
			// Test-server-hosted kodik embed (the animemobi round-trip
			// pattern): tests never touch the real network — the host
			// must carry the "kodik" substring the extractor Matches.
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

// TestAnimikuResolveStreamUnknownDub pins the typed dub miss.
func TestAnimikuResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p := newAnimiku(AniMikuBase, testClient(t, "animiku"))
	episode := contracts.Episode{
		Num:       "1",
		RawEmbeds: map[string][]string{"MC Entertainment": {"//kodikplayer.com/serial/7485/h/720p"}},
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

	p := newAnimiku(AniMikuBase, testClient(t, "animiku"))
	if p.ID() != "animiku" || p.Name() != "AniMiku" || p.BaseURL() != AniMikuBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// TestAnimikuNamePreferenceRU pins the search routing (PR42 semantics):
// the DLE index matches Cyrillic word prefixes («черная лагуна»
// surfaced 4 rows live 2026-09-25, е/ё-equivalence included) — the
// provider stays in the RU group and must NOT declare the latin-only
// preference (anilibria-torrent precedent).
func TestAnimikuNamePreferenceRU(t *testing.T) {
	t.Parallel()

	p := newAnimiku(AniMikuBase, testClient(t, "animiku"))
	if _, declares := any(p).(contracts.NamePreferenceProvider); declares {
		t.Error("animiku must stay in the RU group (no latin preference declaration)")
	}
}

// TestAnimikuSmokeQueryShared pins the probe routing: the shared RU
// smoke probe «черная лагуна» HITS this catalog (4 live rows, first
// resolves through the reachable kodikplayer.com embeds), so the
// provider must NOT declare contracts.SmokeQueryProvider — declaring
// would drop the RU fallback needlessly (PR51 semantics).
func TestAnimikuSmokeQueryShared(t *testing.T) {
	t.Parallel()

	p := newAnimiku(AniMikuBase, testClient(t, "animiku"))
	if _, declares := any(p).(contracts.SmokeQueryProvider); declares {
		t.Error("animiku must not declare SmokeQueryProvider (the shared RU probe hits)")
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
