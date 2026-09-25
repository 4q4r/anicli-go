package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// anikado.net is NOT a Python-tree port (like animedia): the Go
// provider was written against the live site characterized on
// 2026-09-25 (PR102). The site is a DataLife Engine install (UTF-8,
// anonymous, direct 200): search is the DLE search form POST
// (do=search&subaction=search&story=…, server-rendered cards), the
// title page carries a THREE-tab player block (kodik — active and
// primary; vkg — a client-side hydrated mali aggregator; tomion —
// a frame-gated embed that 404s outside its iframe), episode links
// render server-side on the title page, and EVERY episode page
// carries the per-(episode, dub) kodik embed table as
// b-translator__item rows. Movies skip the episode pages: their kodik
// /video/ embed sits directly in the title page's kodik tab. Streams
// resolve through the shared kodik extractor (kodik.info embed hosts
// are normalized onto the interchangeable kodikplayer.com mirror —
// same /seria/ path answers 200 with the hash-consistent player page,
// live-verified 2026-09-25). All fixtures below are real captures of
// 2026-09-25 trimmed to the load-bearing markup, except the walled
// page, which is derived (provenance noted in the file).

func TestAniKadoSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikado_search.html"))
	})
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

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
	// absolutizes them against its base URL.
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
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

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
	// bounded-parallel, so this test serves through its own
	// httptest.Server with a mutex-guarded path recorder (the shared
	// fixtureServer recorder is single-request) and rewrites the title
	// fixture off the real origin before the server starts (base is
	// assigned before the first request fires); the episode capture
	// references no site origin and serves verbatim.
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
	p := newAniKado(srv.URL, testClient(t, "anikado"), 4)

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
		if ep.RawID != ep.Num {
			t.Errorf("episodes[%d].RawID = %q, want the episode num", i, ep.RawID)
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
	// (including the site's doubled ?hide_selectors=true quirk).
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
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/11-klinok-rassekajuschij-demonov-beskonechnyj-poezd-film.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" || ep.RawID != "1" {
		t.Errorf("Num/RawID = %q/%q, want 1/1", ep.Num, ep.RawID)
	}
	links := ep.RawEmbeds["AniKado"]
	if len(links) != 1 {
		t.Fatalf("dubs = %v, want the single AniKado service dub", ep.RawEmbeds)
	}
	const want = "https://kodikplayer.com/video/109611/d41372e3683900687a68073a26e671f0/720p"
	if links[0] != want {
		t.Errorf("movie embed = %s, want the /video/ src absolutized verbatim", links[0])
	}
}

func TestAniKadoGetEpisodesNoPlayerIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikado_anime_walled.html"))
	})
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

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

func TestAniKadoResolveStream(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("direct .mp4 embeds must not be fetched")
	})
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

	// The plumbing under test: RawEmbeds[dubID] → resolveEmbeds →
	// MediaStream. A bare .mp4 embed resolves without network (the
	// resolveEmbeds direct fallback; the kodik extractor itself is
	// behavior-tested in internal/extractors).
	embed := srv.URL + "/stream/episode-5.mp4"
	ep := contracts.Episode{
		Num:       "5",
		RawID:     "5",
		RawEmbeds: map[string][]string{"SHIZA Project": {embed}},
	}
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
	if src.URL != embed {
		t.Errorf("Links[720].URL = %q, want the embed verbatim", src.URL)
	}
}

func TestAniKadoResolveStreamUnknownDubIsTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unknown dub must fail before any fetch")
	})
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

	ep := contracts.Episode{
		Num:       "1",
		RawID:     "1",
		RawEmbeds: map[string][]string{"Silver AniAge": {"https://kodikplayer.com/seria/1265743/6788/720p?episode=1"}},
	}
	_, err := p.ResolveStream(context.Background(), ep, "Ancord")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want contracts.ErrInvalidInput wrap", err)
	}
}

func TestAniKadoResolveStreamEmptyExtractionIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("embeds with no matching extractor must fail before any fetch")
	})
	p := newAniKado(srv.URL, testClient(t, "anikado"), 1)

	// The tomion tab's embed host has no extractor (and 404s outside
	// its iframe context): the typed extract wall.
	ep := contracts.Episode{
		Num:       "1",
		RawID:     "1",
		RawEmbeds: map[string][]string{"AniKado": {"https://tomion.org/yal/40456"}},
	}
	_, err := p.ResolveStream(context.Background(), ep, "AniKado")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed wrap", err)
	}
}

// TestAniKadoEmbedNormalization pins the embed-URL rules: protocol-
// relative absolutization, the kodik.info → kodikplayer.com mirror
// normalization (same /seria/ path answers the hash-consistent player
// page, live-verified 2026-09-25), and the verbatim preservation of
// the site's doubled ?hide_selectors=true query quirk.
func TestAniKadoEmbedNormalization(t *testing.T) {
	t.Parallel()

	got := akNormalizeEmbed("//kodik.info/seria/1265743/6788d79b1b6e3d622f0863ee05c13e41/720p" +
		"?season=1&episode=2&only_translations=2835&hide_selectors=true?hide_selectors=true")
	want := "https://kodikplayer.com/seria/1265743/6788d79b1b6e3d622f0863ee05c13e41/720p" +
		"?season=1&episode=2&only_translations=2835&hide_selectors=true?hide_selectors=true"
	if got != want {
		t.Errorf("akNormalizeEmbed = %s, want %s", got, want)
	}

	// A URL already on kodikplayer.com passes unchanged.
	same := "https://kodikplayer.com/video/109611/d41372e3683900687a68073a26e671f0/720p"
	if got := akNormalizeEmbed(same); got != same {
		t.Errorf("akNormalizeEmbed = %s, want %s verbatim", got, same)
	}
}

// TestAniKadoSmokeProbeIsShared pins the PR51 ruling: the shared RU
// smoke probe («черная лагуна») surfaces this catalog — 2 real hits
// live-verified 2026-09-25 — so the provider must NOT declare a
// SmokeQuery (a declared probe would override the shared one for no
// reason).
func TestAniKadoSmokeProbeIsShared(t *testing.T) {
	t.Parallel()

	p := newAniKado(AniKadoBase, nil, 1)
	if _, ok := any(p).(contracts.SmokeQueryProvider); ok {
		t.Error("anikado must not implement SmokeQueryProvider: the shared RU probe hits the catalog")
	}
}

// TestAniKadoIdentity pins the registration-card values the factory
// roster and the README table render.
func TestAniKadoIdentity(t *testing.T) {
	t.Parallel()

	p := newAniKado(AniKadoBase, nil, 1)
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
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
	// maxParallel <= 0 degrades to 1, never 0 (a zero-bounded fan-out
	// would never run).
	if p.maxParallel != 1 {
		t.Errorf("maxParallel = %d, want the 1 fallback", p.maxParallel)
	}
}
