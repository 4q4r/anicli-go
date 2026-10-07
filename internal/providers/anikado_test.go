package providers

// [LIVE-VERIFIED 2026-09-25, re-verified 2026-10-06 through the direct
// route] anikado.net is a DataLife Engine install (UTF-8, anonymous,
// direct 200): search is the DLE search form POST (do=search&
// subaction=search&story=…, server-rendered cards — live 2026-10-06:
// «черная лагуна» → «найдено 2 ответ», 2 cards, 0.73s), the title page
// carries a THREE-tab player block (kodik — active and primary; vkg —
// a client-side hydrated mali aggregator; tomion — a frame-gated embed
// that 404s outside its iframe), episode links render server-side on
// the title page (.flex-episodes-links, live: 12 anchors), and EVERY
// episode page carries the per-(episode, dub) kodik embed table as
// b-translator__item rows (live: 4 translators). Movies skip the
// episode pages: their kodik /video/ embed sits directly in the title
// page's kodik tab. Streams resolve through the shared kodik extractor
// (kodik.info embed hosts are normalized onto the interchangeable
// kodikplayer.com mirror — same /seria/ path answers 200 with the
// hash-consistent player page). All fixtures below are real captures
// of 2026-09-25 trimmed to the load-bearing markup, except the walled
// page, which is derived (provenance noted in the file).
//
// PR133: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anikado/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shifts
// forced by the fresh-sandbox Lua adapter (the animedia/anikoto
// precedent), documented here rather than hidden:
//
//   - episode RawID carries the streams() state JSON instead of the
//     bare episode number (the only state channel into the
//     per-invocation streams(raw_id, dub) call): series ride the
//     {n, u} page-state shape (the animedia/anizone precedent), movies
//     the {n, e} embed-state shape — the movie embed is fully
//     determined at listing time, so the movie resolve stays a
//     zero-fetch extract exactly like the Go original.
//   - a series resolve re-fetches the episode page to rebuild the
//     translator table (+1 fetch per resolve; the animedia rule) —
//     the direct media URL itself is still never fetched (the
//     extractors' .mp4/.m3u8 fast path).
//   - the episode-page fan-out rides http.get_batch bounded-parallel;
//     the bound is the config network.max_parallel default (4) pinned
//     in the script — out of script reach, and get_batch clamps ≤0 to
//     1 (the kickassanime review-F3 precedent). The Go constructor's
//     maxParallel<=0→1 fallback test died with the constructor.
//   - the akNormalizeEmbed unit pins fold into the series listing pins:
//     the fixture translator rows carry protocol-relative //kodik.info
//     srcs with the doubled ?hide_selectors=true query, and the
//     produced raw_embeds pins assert absolutization, mirror
//     normalization and verbatim query preservation in one.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// akEmbedStateJSON builds the movie embed-state JSON the script encodes
// into a movie episode's raw_id ({n, e}: the episode number and the
// already-normalized kodik /video/ embed).
func akEmbedStateJSON(t *testing.T, num, embed string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"n": num, "e": embed})
	if err != nil {
		t.Fatalf("marshal movie state: %v", err)
	}
	return string(b)
}

func TestAniKadoSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikado_search.html"))
	})
	p := luaProvider(t, "anikado", srv.URL)

	results, err := p.Search(context.Background(), "черная лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The search is the DLE form POST: do/subaction/story urlencoded —
	// the exact shape the site's own quicksearch form submits
	// [LIVE-VERIFIED 2026-09-25: POST and GET answer identically; the
	// form method is POST].
	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/index.php" {
		t.Errorf("request path = %q, want /index.php", rec.Path)
	}
	if got := rec.Query; !strings.Contains(got, "do=search") {
		t.Errorf("request query = %q, want do=search", got)
	}
	if got := rec.Form["do"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("form do = %v, want [search]", got)
	}
	if got := rec.Form["subaction"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("form subaction = %v, want [search]", got)
	}
	if got := rec.Form["story"]; len(got) != 1 || got[0] != "черная лагуна" {
		t.Errorf("form story = %v, want [черная лагуна]", got)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form encoding", ct)
	}

	// Exactly the 2 real result cards of the fixture, in document
	// order.
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 fixture cards", len(results))
	}
	if results[0].Title != "Пираты «Чёрной лагуны»: Второй залп (2006)" {
		t.Errorf("Title = %q, want the h2.card__title text", results[0].Title)
	}
	if results[0].URL != "https://anikado.net/573-piraty-chernoj-laguny-vtoroj-zalp.html" {
		t.Errorf("URL = %q, want the a.card__img href verbatim", results[0].URL)
	}
	if results[0].SourceID != "anikado" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	// The result posters are site-relative img srcs; the provider
	// absolutizes them against its (harness-rewritten) base URL.
	if results[0].Poster != srv.URL+"/uploads/posts/2024-03/piraty-chernoj-laguny-vtoroj-zalp.webp" {
		t.Errorf("Poster = %q, want the base-URL-prefixed img src", results[0].Poster)
	}
	if results[1].Title != "Пираты «Чёрной лагуны» (2006)" {
		t.Errorf("Title[1] = %q", results[1].Title)
	}
	if results[1].URL != "https://anikado.net/572-piraty-chernoj-laguny.html" {
		t.Errorf("URL[1] = %q", results[1].URL)
	}
}

func TestAniKadoSearchMissIsEmpty(t *testing.T) {
	t.Parallel()

	// The real zero-results page shape [LIVE-VERIFIED 2026-09-25: a
	// junk query answers HTTP 200 with the DLE search shell and the
	// «не дал никаких результатов» message — zero cards, no error].
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body><div id='dle-content'><form name='fullsearch'>" +
			"<div class='message-info'><div class='message-info__content'>К сожалению, поиск по сайту не дал никаких результатов.</div></div>" +
			"</form></div></body></html>"))
	})
	p := luaProvider(t, "anikado", srv.URL)

	results, err := p.Search(context.Background(), "дандадан")
	if err != nil {
		t.Fatalf("Search miss must not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

func TestAniKadoGetEpisodesSeries(t *testing.T) {
	t.Parallel()

	// The title page carries the 12 episode anchors; every episode
	// fetch is answered with the (real) episode-2 capture. Fetches run
	// bounded-parallel (http.get_batch), so this test serves through
	// its own httptest.Server with a mutex-guarded path recorder (the
	// shared fixtureServer recorder is single-request) and rewrites
	// the title fixture off the real origin before the server starts
	// (base is assigned before the first request fires); the episode
	// capture references no site origin and serves verbatim.
	titleFixture := string(fixture(t, "anikado_anime.html"))
	episodeFixture := fixture(t, "anikado_episode.html")
	var base string
	var mu sync.Mutex
	var episodePaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/episode-") {
			mu.Lock()
			episodePaths = append(episodePaths, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write(episodeFixture)
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(titleFixture, "https://anikado.net", base)))
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	p := luaProvider(t, "anikado", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/572-piraty-chernoj-laguny.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The title page renders all 12 anchors server-side (no
	// pagination, live-verified on a 52-episode title).
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12", len(episodes))
	}
	for i, ep := range episodes {
		if ep.Num != strconv.Itoa(i+1) {
			t.Fatalf("episodes[%d].Num = %q, want ascending document order", i, ep.Num)
		}
		// The state JSON rides RawID ({n, u}): the episode number and
		// its episode-page URL — the fresh-sandbox streams() channel.
		var state struct {
			N string `json:"n"`
			U string `json:"u"`
		}
		if err := json.Unmarshal([]byte(ep.RawID), &state); err != nil {
			t.Fatalf("episodes[%d].RawID = %q, want the {n,u} state JSON: %v", i, ep.RawID, err)
		}
		if state.N != ep.Num {
			t.Errorf("episodes[%d] state n = %q, want the episode num", i, state.N)
		}
		if state.U != srv.URL+"/572-piraty-chernoj-laguny/episode-"+ep.Num+".html" {
			t.Errorf("episodes[%d] state u = %q, want the episode-page URL", i, state.U)
		}
	}

	// Every episode page was fetched exactly once, at its site path.
	if len(episodePaths) != 12 {
		t.Fatalf("episode fetches = %d, want 12", len(episodePaths))
	}
	// The recorder appends in completion order (the fan-out is
	// bounded-parallel) — compare as a set.
	gotPaths := map[string]bool{}
	for _, path := range episodePaths {
		gotPaths[path] = true
	}
	for i := range 12 {
		want := "/572-piraty-chernoj-laguny/episode-" + strconv.Itoa(i+1) + ".html"
		if !gotPaths[want] {
			t.Errorf("episode fetch %q missing; got %v", want, episodePaths)
		}
	}

	// The episode page carries the per-(episode, dub) kodik embed
	// table: 4 translators on the fixture episode.
	ep2 := episodes[1]
	wantDubs := []string{"Silver AniAge", "SHIZA Project", "MC Entertainment", "Субтитры"}
	if len(ep2.RawEmbeds) != len(wantDubs) {
		t.Fatalf("episode 2 dubs = %d (%v), want 4", len(ep2.RawEmbeds), keys(ep2.RawEmbeds))
	}
	for _, name := range wantDubs {
		if _, ok := ep2.RawEmbeds[name]; !ok {
			t.Errorf("episode 2 missing dub %q", name)
		}
	}

	// The embed of each dub is its b-translator__item data-this_link:
	// protocol-relative src absolutized, kodik.info host normalized
	// onto the interchangeable kodikplayer.com mirror, query verbatim
	// (including the site's doubled ?hide_selectors=true quirk) — the
	// akNormalizeEmbed pins, folded here (see the header).
	const wantSilver = "https://kodikplayer.com/seria/1265743/6788d79b1b6e3d622f0863ee05c13e41/720p" +
		"?season=1&episode=2&only_translations=2835&hide_selectors=true?hide_selectors=true"
	if got := ep2.RawEmbeds["Silver AniAge"][0]; got != wantSilver {
		t.Errorf("Silver AniAge ep2 embed = %s, want %s", got, wantSilver)
	}
	const wantSubs = "https://kodikplayer.com/seria/1099966/7129a48016636c64027921ca5bfaa43a/720p" +
		"?season=1&episode=2&only_translations=869&hide_selectors=true?hide_selectors=true"
	if got := ep2.RawEmbeds["Субтитры"][0]; got != wantSubs {
		t.Errorf("Субтитры ep2 embed = %s, want %s", got, wantSubs)
	}
}

func TestAniKadoGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Movie pages never fetch episode pages [LIVE-VERIFIED
		// 2026-09-25: the film title page has no episode anchors and
		// no translator list — its kodik /video/ embed sits in the
		// active kodik tab].
		_, _ = w.Write(fixture(t, "anikado_anime_movie.html"))
	})
	p := luaProvider(t, "anikado", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/11-klinok-rassekajuschij-demonov-beskonechnyj-poezd-film.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want 1", ep.Num)
	}
	// The embed-state JSON rides RawID ({n, e}): the movie's single
	// kodik /video/ embed, already normalized, resolved later with no
	// page fetch (the Go zero-fetch movie resolve kept).
	var state struct {
		N string `json:"n"`
		E string `json:"e"`
	}
	if err := json.Unmarshal([]byte(ep.RawID), &state); err != nil {
		t.Fatalf("RawID = %q, want the {n,e} state JSON: %v", ep.RawID, err)
	}
	if state.N != "1" {
		t.Errorf("state n = %q, want 1", state.N)
	}
	const wantEmbed = "https://kodikplayer.com/video/109611/d41372e3683900687a68073a26e671f0/720p"
	if state.E != wantEmbed {
		t.Errorf("state e = %q, want the /video/ src absolutized verbatim", state.E)
	}
	links := ep.RawEmbeds["AniKado"]
	if len(links) != 1 {
		t.Fatalf("dubs = %v, want the single AniKado service dub", ep.RawEmbeds)
	}
	if links[0] != wantEmbed {
		t.Errorf("movie embed = %s, want the /video/ src absolutized verbatim", links[0])
	}
}

func TestAniKadoGetEpisodesNoPlayerIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikado_anime_walled.html"))
	})
	p := luaProvider(t, "anikado", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/11-klinok-rassekajuschij-demonov-beskonechnyj-poezd-film.html")
	if err == nil {
		t.Fatal("a title page with no resolvable player must fail loud, not return empty episodes")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want contracts.ErrNotFound wrap", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "anikado" {
		t.Fatalf("err = %v, want an anikado ProviderError", err)
	}
}

// TestAniKadoResolveStream pins the resolve chain through the fresh
// sandbox: streams(raw_id, dub) re-fetches the episode page named in
// the state JSON, rebuilds the translator table and resolves the
// chosen dub's embed through the shared extractor factory. A bare .mp4
// embed resolves through the extractors' direct fast path — the media
// URL itself must never be fetched (the kodik extractor itself is
// behavior-tested in internal/extractors).
func TestAniKadoResolveStream(t *testing.T) {
	t.Parallel()

	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/stream/") {
			t.Error("direct .mp4 embeds must not be fetched")
			http.Error(w, "media is never fetched", http.StatusInternalServerError)
			return
		}
		// The minimal episode-page shape: one translator row whose
		// data-this_link is the direct mp4.
		_, _ = w.Write([]byte(`<html><body><ul>` +
			`<li class="b-translator__item" data-this_translator="SHIZA Project" data-this_link="` +
			base + `/stream/episode-5.mp4"></li></ul></body></html>`))
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	p := luaProvider(t, "anikado", srv.URL)

	page := srv.URL + "/572-piraty-chernoj-laguny/episode-5.html"
	rawID, err := luaStateJSON(page, "5")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	ep := contracts.Episode{Num: "5", RawID: rawID}

	stream, err := p.ResolveStream(context.Background(), ep, "SHIZA Project")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "SHIZA Project" {
		t.Errorf("DubName = %q, want the requested dub", stream.DubName)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry", stream.Links)
	}
	if want := srv.URL + "/stream/episode-5.mp4"; src.URL != want {
		t.Errorf("Links[720].URL = %q, want the embed verbatim", src.URL)
	}
}

// TestAniKadoResolveStreamUnknownDubIsTypedWall pins the ask-first
// dub-miss doctrine (#159 port — the round-2 shape walled the miss
// invalid_input, a caller-mistake class the tui dubNotCarriedFailure
// predicate cannot see): a dub the episode does not carry walls typed
// ErrNotFound whose message names the requested dub and LISTS the
// dubs the episode actually carries (the rebuilt translator table's
// keys, byte-sorted — a Lua map has no order).
func TestAniKadoResolveStreamUnknownDubIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><ul>` +
			`<li class="b-translator__item" data-this_translator="Silver AniAge" ` +
			`data-this_link="//kodik.info/seria/1265743/6788/720p?episode=1"></li></ul></body></html>`))
	})
	p := luaProvider(t, "anikado", srv.URL)

	page := srv.URL + "/572-piraty-chernoj-laguny/episode-1.html"
	rawID, err := luaStateJSON(page, "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	ep := contracts.Episode{Num: "1", RawID: rawID}

	_, err = p.ResolveStream(context.Background(), ep, "Ancord")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (never a silent dub substitution)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `carries no dub "Ancord"`) {
		t.Errorf("message = %q, want the requested dub named", msg)
	}
	if !strings.Contains(msg, "(episode dubs: Silver AniAge)") {
		t.Errorf("message = %q, want the episode's carrier list", msg)
	}
}

// TestAniKadoResolveStreamMovieDubMissListsServiceDub pins the movie
// branch of the doctrine (#159): the embed-state resolve carries the
// single service dub, so a miss walls typed ErrNotFound listing
// AniKado — the actionable payload even on the zero-fetch branch.
func TestAniKadoResolveStreamMovieDubMissListsServiceDub(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the embed-state resolve must not fetch anything")
		http.Error(w, "no fetch expected", http.StatusInternalServerError)
	})
	p := luaProvider(t, "anikado", srv.URL)

	embed := akEmbedStateJSON(t, "1", "https://kodikplayer.com/video/113757/15dee/720p")
	ep := contracts.Episode{Num: "1", RawID: embed}

	_, err := p.ResolveStream(context.Background(), ep, "NoSuchDub")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (never a silent dub substitution)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `carries no dub "NoSuchDub"`) {
		t.Errorf("message = %q, want the requested dub named", msg)
	}
	if !strings.Contains(msg, "(episode dubs: AniKado)") {
		t.Errorf("message = %q, want the service dub listed", msg)
	}
}

// TestAniKadoResolveStreamZeroDubsIsTypedWall pins the zero-dubs
// edge: an episode page whose re-fetch carries no translator rows at
// all walls typed ErrNotFound (a data-shape fact, not a caller
// mistake) with the marker but NO carrier list.
func TestAniKadoResolveStreamZeroDubsIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>episode shell, no translator rows</body></html>`))
	})
	p := luaProvider(t, "anikado", srv.URL)

	page := srv.URL + "/572-piraty-chernoj-laguny/episode-1.html"
	rawID, err := luaStateJSON(page, "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	_, err = p.ResolveStream(context.Background(),
		contracts.Episode{Num: "1", RawID: rawID}, "Silver AniAge")
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

// TestAniKadoResolveStreamGarbageStateIsTypedWall pins the typed wall
// for ANY raw_id byte sequence (the #157 class): merged-convention
// prefix bytes, plain non-json text, state JSON carrying neither the
// u nor the e leg, and the empty id must surface as ErrInvalidInput —
// never the raw json.decode VM error through to the user.
func TestAniKadoResolveStreamGarbageStateIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a malformed state must not fetch anything")
		http.Error(w, "no fetch expected", http.StatusInternalServerError)
	})
	p := luaProvider(t, "anikado", srv.URL)

	cases := []struct {
		name  string
		rawID string
	}{
		{"merged-convention prefix bytes", `anikado:{"n":"1","u":"` + srv.URL + `/ep.html"}`},
		{"plain non-json text", "about:blank"},
		{"state json carrying neither leg", `{"n":"1"}`},
		{"state json of the wrong shape", `[1,2,3]`},
		{"empty raw id", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.ResolveStream(context.Background(),
				contracts.Episode{Num: "1", RawID: tc.rawID}, "AniKado")
			if !errors.Is(err, contracts.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestAniKadoResolveStreamEmptyExtractionIsTypedWall pins the typed
// extract wall: an embed with no matching extractor (the tomion tab's
// host, which also 404s outside its iframe context) fails
// ErrExtractFailed. The movie embed-state resolve is pure — the
// handler must never be reached (the zero-fetch movie resolve kept).
func TestAniKadoResolveStreamEmptyExtractionIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the embed-state resolve must not fetch anything")
		http.Error(w, "no fetch expected", http.StatusInternalServerError)
	})
	p := luaProvider(t, "anikado", srv.URL)

	embed := akEmbedStateJSON(t, "1", "https://tomion.org/yal/40456")
	ep := contracts.Episode{Num: "1", RawID: embed}

	_, err := p.ResolveStream(context.Background(), ep, "AniKado")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed wrap", err)
	}
}

// TestAniKadoSmokeQueryUndeclared pins the PR51 smoke routing: the
// shared RU smoke probe («черная лагуна») surfaces this catalog — 2
// real hits, re-verified live 2026-10-06 — so the script declares no
// probe of its own; the adapter-declared capability answers empty and
// the shared probe applies (Go parity: the compiled provider
// implemented no usable SmokeQuery either).
func TestAniKadoSmokeQueryUndeclared(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anikado")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the content_lang adapter must keep the capability surface assertions-stable")
	}
	if got := sq.SmokeQuery(); got != "" {
		t.Errorf("SmokeQuery = %q, want empty (the shared RU probe applies)", got)
	}
}

// TestAniKadoIdentity pins the registration-card values the factory
// roster and the README table render, through the bundled script's own
// declarations.
func TestAniKadoIdentity(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anikado")
	if p.ID() != "anikado" {
		t.Errorf("ID = %q", p.ID())
	}
	if p.Name() != "AniKado" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.BaseURL() != "https://anikado.net" {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the anikado script lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", got)
	}
}
