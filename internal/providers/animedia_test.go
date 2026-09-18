package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// amd.online is NOT a Python-tree port (like anidub): the Go provider
// was written against the live site characterized on 2026-09-18. The
// site moved from animedia.online/api.animedia.online (the frozen
// anicli-py JSON v3 API, now 404) to amd.online — a DataLife Engine
// install fronted by DDoS-Guard. Search is the DLE search form (POST,
// server-rendered results); episodes and dubs are server-rendered in
// the anime page's player blocks as kodik embeds; streams resolve
// through the existing kodik extractor. All fixtures below are real
// captures trimmed to the load-bearing markup.

func TestAniMediaSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animedia_search.html"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	results, err := p.Search(context.Background(), "врата")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The search is the DLE form POST: do/subaction/story in the form
	// body, urlencoded (spaces as '+'), not the GET query — a GET with
	// the same params returns the site chrome with a recommendation
	// feed instead of results [LIVE-VERIFIED 2026-09-18].
	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/" {
		t.Errorf("request path = %q, want /", rec.Path)
	}
	if got := rec.Form["do"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("form do = %v, want [search]", got)
	}
	if got := rec.Form["subaction"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("form subaction = %v, want [search]", got)
	}
	if got := rec.Form["story"]; len(got) != 1 || got[0] != "врата" {
		t.Errorf("form story = %v, want [врата]", got)
	}

	// Exactly the 3 real result cards of the fixture: the header
	// recommendation widget poster rendered BEFORE #dle-content must
	// not leak into the results (live pages carry ~14 such widgets).
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 fixture cards", len(results))
	}
	if results[0].Title != "Врата Штейна" {
		t.Errorf("Title = %q, want the h3.poster__title text", results[0].Title)
	}
	if results[0].URL != "https://amd.online/4368-vrata-shtejna.html" {
		t.Errorf("URL = %q, want the a.poster__link href verbatim", results[0].URL)
	}
	if results[0].SourceID != "animedia" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	// The result posters are site-relative img srcs; the provider
	// absolutizes them against its base URL.
	if results[0].Poster != srv.URL+"/uploads/posts/2026-08/vrata-shtejna.webp" {
		t.Errorf("Poster = %q, want the site-root-prefixed img src", results[0].Poster)
	}
	if results[1].Title != "Врата Штейна: Великая мудрость когнитивного компьютера" {
		t.Errorf("Title[1] = %q", results[1].Title)
	}
	if results[2].URL != "https://amd.online/3777-vrata-tam-bjutsja-nashi-voiny-ognedyshaschie-drakony.html" {
		t.Errorf("URL[2] = %q", results[2].URL)
	}
}

func TestAniMediaSearchMissIsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body><div id='dle-content'>" +
			"<article class='box story searchpage'><div class='search_result_num grey'>По Вашему запросу найдено 0 ответов</div></article>" +
			"</div></body></html>"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	results, err := p.Search(context.Background(), "лагуна")
	if err != nil {
		t.Fatalf("Search miss must not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

func TestAniMediaGetEpisodesSeries(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animedia_anime.html"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/4368-vrata-shtejna.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}

	// The nav carries 24 anchors; the live page repeats them across the
	// PWA and desktop blocks — the provider must dedupe.
	if len(episodes) != 24 {
		t.Fatalf("episodes = %d, want 24 deduped anchors", len(episodes))
	}
	for i, ep := range episodes {
		if ep.Num != strconv.Itoa(i+1) {
			t.Fatalf("episodes[%d].Num = %q, want ascending data-vid order", i, ep.Num)
		}
	}

	// Every dub — including the is-active default — is an
	// amd-kodik-voice button; the PWA and desktop blocks duplicate the
	// 11 buttons, dedupe by data-voice.
	const dubAmber = "Amber" // the is-active default (voice 933)
	ep1 := episodes[0]
	wantDubs := []string{
		"СВ-Дубль", "Animedia", "SHIZA Project", "LE-Production",
		"AniLibria.TV", dubAmber, "Ancord", "Freedub Studio",
		"oDaletY", "Субтитры", "SovetRomantica.Subtitles",
	}
	if len(ep1.RawEmbeds) != len(wantDubs) {
		t.Fatalf("episode 1 dubs = %d, want 11 deduped voices", len(ep1.RawEmbeds))
	}
	for _, name := range wantDubs {
		if _, ok := ep1.RawEmbeds[name]; !ok {
			t.Errorf("episode 1 missing dub %q; got %v", name, keys(ep1.RawEmbeds))
		}
	}

	// The embed of each (episode, dub) pair is the dub's kodik src with
	// the episode query param substituted; season stays from the src.
	amber := ep1.RawEmbeds[dubAmber][0]
	if !strings.Contains(amber, "kodikplayer.com/serial/63832/0cda7cbdeda3c76852b7700878598763/720p") {
		t.Errorf("Amber ep1 embed carries the wrong kodik path: %s", amber)
	}
	if !strings.Contains(amber, "season=1") || !strings.Contains(amber, "episode=1") {
		t.Errorf("Amber ep1 embed = %s, want season=1&episode=1", amber)
	}
	if !strings.HasPrefix(amber, "https://") {
		t.Errorf("Amber ep1 embed = %s, want the protocol-relative src absolutized", amber)
	}
	if got := episodes[4].RawEmbeds[dubAmber][0]; !strings.Contains(got, "episode=5") || strings.Contains(got, "episode=1&") {
		t.Errorf("Amber ep5 embed = %s, want episode=5 substituted", got)
	}
	// A different dub is a different kodik title: substitution must not
	// touch the path.
	if got := episodes[4].RawEmbeds["AniLibria.TV"][0]; !strings.Contains(got, "kodikplayer.com/serial/27423/9bc813cf9293f1305ce036b03845618a/720p") {
		t.Errorf("AniLibria.TV ep5 embed carries the wrong kodik path: %s", got)
	}
	if got := episodes[4].RawEmbeds["AniLibria.TV"][0]; !strings.Contains(got, "only_translations=610") {
		t.Errorf("AniLibria.TV ep5 embed = %s, want only_translations preserved", got)
	}

	if episodes[0].RawID != "1" || episodes[23].RawID != "24" {
		t.Errorf("RawID = %q/%q, want the data-vid values", episodes[0].RawID, episodes[23].RawID)
	}
}

func TestAniMediaGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animedia_anime_movie.html"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/771-klinok.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// Movie pages carry ONE kodik /video/ iframe, no nav anchors and no
	// voice buttons [LIVE-VERIFIED 2026-09-18]: a single episode under
	// the service dub name.
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" || ep.RawID != "1" {
		t.Errorf("Num/RawID = %q/%q, want 1/1", ep.Num, ep.RawID)
	}
	links := ep.RawEmbeds["AniMedia"]
	if len(links) != 1 {
		t.Fatalf("dubs = %v, want the single AniMedia service dub", ep.RawEmbeds)
	}
	const want = "https://kodikplayer.com/video/79765/9f42e884f916bf6b1cadad618a58c37b/720p?season=1&episode=1&hide_selectors=true"
	if links[0] != want {
		t.Errorf("movie embed = %s, want the /video/ src verbatim (https-absolutized)", links[0])
	}
}

func TestAniMediaGetEpisodesUnsupportedPlayerIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animedia_anime_rutube.html"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/704-puteshestvie.html")
	if err == nil {
		t.Fatal("rutube-served title must fail loud, not return empty episodes")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed wrap", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "animedia" {
		t.Fatalf("err = %v, want an animedia ProviderError", err)
	}
	if !strings.Contains(err.Error(), "rutube") {
		t.Errorf("err = %v, want the unsupported player named", err)
	}
}

// TestAniMediaGetEpisodesWallNamesHost pins that the unsupported-
// player wall names the serving HOST, whatever it is (the live «Врата
// Штейна 0» archive page serves frame_video_mod from aser.pro — a
// "unknown player" message would hide the actual blocker).
func TestAniMediaGetEpisodesWallNamesHost(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animedia_anime_aser.html"))
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/69-vrata-shteyna-0.html")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed wrap", err)
	}
	if !strings.Contains(err.Error(), "aser.pro") {
		t.Errorf("err = %v, want the serving host named", err)
	}
}

func TestAniMediaResolveStream(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("direct .mp4 embeds must not be fetched")
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	// The plumbing under test: RawEmbeds[dubID] → resolveEmbeds →
	// MediaStream. A bare .mp4 embed resolves without network (the
	// resolveEmbeds direct fallback; the kodik extractor itself is
	// behavior-tested in internal/extractors).
	embed := srv.URL + "/stream/episode-5.mp4"
	ep := contracts.Episode{
		Num:       "5",
		RawID:     "5",
		RawEmbeds: map[string][]string{"Amber": {embed}},
	}
	stream, err := p.ResolveStream(context.Background(), ep, "Amber")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Amber" {
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

func TestAniMediaResolveStreamUnknownDubIsTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unknown dub must fail before any fetch")
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	ep := contracts.Episode{
		Num:       "1",
		RawID:     "1",
		RawEmbeds: map[string][]string{"Amber": {"https://kodikplayer.com/serial/63832/0cda/720p?episode=1"}},
	}
	_, err := p.ResolveStream(context.Background(), ep, "Ancord")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want contracts.ErrInvalidInput wrap", err)
	}
}

func TestAniMediaResolveStreamEmptyExtractionIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("embeds with no matching extractor must fail before any fetch")
	})
	p := newAniMedia(srv.URL, testClient(t, "animedia"))

	ep := contracts.Episode{
		Num:       "1",
		RawID:     "1",
		RawEmbeds: map[string][]string{"AniMedia": {"https://rutube.ru/play/embed/c6beb695"}},
	}
	_, err := p.ResolveStream(context.Background(), ep, "AniMedia")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed wrap", err)
	}
}

// TestAniMediaSmokeQuery pins the declared smoke probe (PR51
// mechanism): the shared RU probe («черная лагуна») and the latin
// fallback both miss this catalog — Black Lagoon is not on amd.online
// [LIVE-VERIFIED 2026-09-18], so the provider speaks for itself.
func TestAniMediaSmokeQuery(t *testing.T) {
	t.Parallel()

	p := newAniMedia("https://amd.online", nil)
	if got := p.SmokeQuery(); got != "врата штейна" {
		t.Errorf("SmokeQuery() = %q, want врата штейна", got)
	}
}

// TestAniMediaIdentity pins the registration-card values the factory
// roster and the README table render.
func TestAniMediaIdentity(t *testing.T) {
	t.Parallel()

	p := newAniMedia("https://amd.online", nil)
	if p.ID() != "animedia" {
		t.Errorf("ID = %q", p.ID())
	}
	if p.Name() != "AniMedia" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.BaseURL() != "https://amd.online" {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// TestAniMediaEpisodeParamSubstitutionRule pins the substitution edge:
// a dub src without an episode param is carried verbatim into every
// episode (the substitution only rewrites an existing param).
func TestAniMediaEpisodeParamSubstitutionRule(t *testing.T) {
	t.Parallel()

	src := amdSubstituteEpisode("https://kodikplayer.com/video/79765/9f42/720p?season=1&hide_selectors=true", "7")
	if !strings.HasSuffix(src, "?season=1&hide_selectors=true") {
		t.Errorf("src without episode param = %s, want verbatim", src)
	}
	src = amdSubstituteEpisode("https://kodikplayer.com/serial/63832/0cda/720p?season=1&episode=1&only_translations=933", "12")
	if !strings.Contains(src, "episode=12") || strings.Contains(src, "episode=1&") {
		t.Errorf("src = %s, want episode=12 substituted", src)
	}
	if u, err := url.Parse(src); err != nil || u.Query().Get("season") != "1" || u.Query().Get("only_translations") != "933" {
		t.Errorf("src = %s, want the other params untouched (%v)", src, err)
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
