package providers

// Fixture provenance: animego_search.html, animego_anime.html,
// animego_player_series.json, animego_player_film.json and
// animego_videos.json are VERBATIM live captures from animego.me taken
// on 2026-09-18 (the PR48 probe):
//
//	GET /search/anime?q=lagoon                          -> 200 (3 grid items, one titleless)
//	GET /anime/piraty-chernoy-laguny-2115               -> 200 (loader → /player/2115)
//	GET /player/2115                                    -> 200 (carousel + episode-one provider buttons)
//	GET /player/videos/27784                            -> 200 (one episode's provider buttons)
//
// PR124: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animego/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shifts
// forced by the fresh-sandbox Lua adapter (the anilib/animevost
// precedent), documented here rather than hidden:
//
//   - the PR44 tier-1 dub-list distribution moved INTO the script's
//     episodes() (episode one keeps its real links, the rest carry the
//     release's dub keys with empty lists — the pins are verbatim);
//   - streams(raw_id, dub) ALWAYS re-fetches /player/videos/{raw_id}
//     (the fresh-sandbox state contract: raw_id is the bare episode
//     id) — the Go FetchDubs short-circuit on pre-existing embeds is
//     structural in the adapter, so the always-fresh refetch is what
//     the hydration pins assert;
//   - the resolve pins drive the real hydration route: the direct-
//     fallback pin serves an ABSOLUTE direct-media URL through the
//     real parse (the protocol-relative "//" prefixing is pinned at
//     the parse level), and the extractor-miss pin rides the typed
//     extract failure through the same marker classification.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// animegoEnvelope wraps a player-content HTML fragment in the site's
// JSON envelope (data.content carries the payload).
func animegoEnvelope(content string) string {
	b, err := json.Marshal(map[string]any{
		"status":  "success",
		"message": nil,
		"data":    map[string]any{"content": content},
	})
	if err != nil {
		panic("animegoEnvelope: " + err.Error())
	}
	return string(b)
}

// animegoStubServer routes /player/* requests to playerHandler and
// everything else to pageHandler, recording each request URI in order.
func animegoStubServer(t *testing.T, playerHandler, pageHandler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/player/") {
			playerHandler(w, r)
			return
		}
		pageHandler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestAnimegoSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animego_search.html"))
	})
	p := luaProvider(t, "animego", srv.URL)

	results, err := p.Search(context.Background(), "lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/search/anime" {
		t.Errorf("request path = %q", rec.Path)
	}
	if rec.Query != "q=lagoon" {
		t.Errorf("request query = %q, want q=lagoon", rec.Query)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (third .ani-grid__item lacks a title link and must be skipped)", len(results))
	}
	if results[0].Title != "Пираты «Чёрной лагуны»" {
		t.Errorf("Title = %q, want the a[title] attribute", results[0].Title)
	}
	// The site emits RELATIVE hrefs; Search must absolutize them
	// against the provider base so GetEpisodes can fetch the URL.
	if results[0].URL != srv.URL+"/anime/piraty-chernoy-laguny-2115" {
		t.Errorf("URL = %q, want the absolutized /anime/piraty-chernoy-laguny-2115", results[0].URL)
	}
	if results[0].SourceID != "animego" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != "https://img.cdngos.com/v/250x350/anime/63/631480ebd9c8f408385195" {
		t.Errorf("Poster = %q, want the .ani-grid__item-picture img[src] attribute", results[0].Poster)
	}
	// Second item has no picture node: poster stays empty.
	if results[1].Poster != "" {
		t.Errorf("Poster = %q, want empty without .ani-grid__item-picture", results[1].Poster)
	}
	if results[1].URL != srv.URL+"/anime/piraty-chernoy-laguny-vtoroy-zalp-2116" {
		t.Errorf("URL = %q, want the second item's absolutized href", results[1].URL)
	}
}

func TestAnimegoSearchSendsSiteHeaders(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html></html>")
	})
	p := luaProvider(t, "animego", srv.URL)

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	for header, want := range map[string]string{
		"Referer":          srv.URL,
		"X-Requested-With": "XMLHttpRequest",
		"Accept-Language":  "ru-RU",
	} {
		if got := rec.Header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	if got := rec.Header.Get("User-Agent"); got == "" {
		t.Error("User-Agent header missing")
	}
}

func TestAnimegoGetEpisodesSeries(t *testing.T) {
	t.Parallel()

	// Route-aware stub: the anime page, then /player/{id}. The site's
	// player fragment ALREADY carries the first episode's provider
	// buttons, so the PR44 tier-1 dub-list fetch is free: exactly two
	// requests cover the release.
	srv, paths := animegoStubServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "animego_player_series.json"))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(fixture(t, "animego_anime.html"))
		})
	p := luaProvider(t, "animego", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/piraty-chernoy-laguny-2115")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	want := []string{"/anime/piraty-chernoy-laguny-2115", "/player/2115"}
	got := paths()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	// Carousel: data-episode-number + data-episode, no per-episode
	// title attribute (the site dropped episode titles).
	if episodes[0].Num != "1" || episodes[0].RawID != "27779" {
		t.Errorf("episode 1 = %+v", episodes[0])
	}
	if episodes[1].Num != "2" || episodes[1].RawID != "27780" {
		t.Errorf("episode 2 = %+v", episodes[1])
	}
	if episodes[0].Title != "" || episodes[1].Title != "" {
		t.Errorf("episode titles = %q/%q, want empty (carousel carries no titles)", episodes[0].Title, episodes[1].Title)
	}
	// Episode one keeps the real provider links parsed from the same
	// fragment (AniBoom + Kodik under the MC Entertainment translation,
	// https-prefixed in fragment order); episode two carries the
	// release's dub keys with EMPTY lists (on-demand resolve, the PR44
	// owner model the script distributes).
	mc := episodes[0].RawEmbeds["MC Entertainment"]
	if len(mc) != 2 {
		t.Fatalf("episode 1 MC Entertainment links = %v, want the AniBoom+Kodik pair from the fragment", mc)
	}
	if !strings.HasPrefix(mc[0], "https://aniboom.one/embed/") {
		t.Errorf("episode 1 link[0] = %q, want the https-prefixed aniboom embed", mc[0])
	}
	if !strings.HasPrefix(mc[1], "https://kodikplayer.com/seria/") {
		t.Errorf("episode 1 link[1] = %q, want the https-prefixed kodik embed", mc[1])
	}
	if links := episodes[1].RawEmbeds["MC Entertainment"]; links == nil || len(links) != 0 {
		t.Errorf("episode 2 MC Entertainment links = %v, want an empty list", links)
	}
}

func TestAnimegoGetEpisodesFilmParsesEmbedsInline(t *testing.T) {
	t.Parallel()

	srv, _ := animegoStubServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "animego_player_film.json"))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<div class="player__video" data-controller="anime-player-loader" data-anime-player-loader-url-value="/player/4060"></div>`)
		})
	p := luaProvider(t, "animego", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/utrachennoye-nebesami-4060")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want the single film episode", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" || ep.Title != "Фильм" || ep.RawID != "4060" {
		t.Errorf("film episode = %+v (RawID from the loader path)", ep)
	}

	// Inline embed parsing over the player content: real Kodik buttons
	// tagged with data-translation-title (AniDUB, SHIZA Project).
	embeds := ep.RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("embeds = %v, want 2 translations", embeds)
	}
	anidub, ok := embeds["AniDUB"]
	if !ok || len(anidub) != 1 {
		t.Fatalf("embeds[AniDUB] = %v, want 1 player", anidub)
	}
	if !strings.HasPrefix(anidub[0], "https://kodikplayer.com/video/10081/") {
		t.Errorf("anidub[0] = %q, want the https-prefixed kodik link", anidub[0])
	}
	shiza, ok := embeds["SHIZA Project"]
	if !ok || len(shiza) != 1 || !strings.HasPrefix(shiza[0], "https://kodikplayer.com/video/46246/") {
		t.Errorf("embeds[SHIZA Project] = %v", embeds["SHIZA Project"])
	}
}

func TestAnimegoGetEpisodesMissingLoaderReturnsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := animegoStubServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<html><body>no loader node here</body></html>`)
		})
	p := luaProvider(t, "animego", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/broken")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without data-anime-player-loader-url-value", len(episodes))
	}
}

func TestAnimegoGetEpisodesMalformedPlayerJSONIsTypedError(t *testing.T) {
	t.Parallel()

	srv, _ := animegoStubServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, "<html>not json</html>")
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<div data-anime-player-loader-url-value="/player/7"></div>`)
		})
	p := luaProvider(t, "animego", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if err == nil {
		t.Fatal("malformed player JSON must fail")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want *contracts.ProviderError", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %v, want decode context", err)
	}
}

func TestAnimegoGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := luaProvider(t, "animego", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

// TestAnimegoStreamsUnknownDubHydratesEmpty drives the hydration route
// against the real /player/videos capture: streams() re-fetches the
// episode detail, and a dub the fragment does not name resolves to an
// empty stream — never an error (the anilib parity rule).
func TestAnimegoStreamsUnknownDubHydratesEmpty(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animego_videos.json"))
	})
	p := luaProvider(t, "animego", srv.URL)

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "5", RawID: "27784"}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if rec.Path != "/player/videos/27784" {
		t.Errorf("request = %s, want the on-demand /player/videos/27784 hydration", rec.Path)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for a dub the hydration does not name", stream.Links)
	}
	if stream.DubName != "NoSuchDub" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestAnimegoStreamsHydratesAndResolvesDirect covers the named-dub
// hydration: the /player/videos fragment's provider buttons parse with
// the same rules as the player fragment (https prefixing, translation
// grouping) and resolve through the shared extractor factory — a
// direct media URL takes the suffix fast path with the URL untouched.
func TestAnimegoStreamsHydratesAndResolvesDirect(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, animegoEnvelope(`<button data-anime-player-target="provider" data-player="//cdn.example.com/static/film.m3u8" data-translation-title="MC Entertainment"></button>`))
	})
	p := luaProvider(t, "animego", srv.URL)

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "27779"}, "MC Entertainment")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if rec.Path != "/player/videos/27779" {
		t.Errorf("request = %s, want the on-demand /player/videos/27779 hydration", rec.Path)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want the direct 720 fallback", stream.Links)
	}
	if src.URL != "https://cdn.example.com/static/film.m3u8" {
		t.Errorf("URL = %q, want the https-prefixed link untouched by the fallback", src.URL)
	}
	if stream.DubName != "MC Entertainment" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestAnimegoEpisodesUnknownTranslationFallsBackToUnknown covers a
// provider button missing data-translation-title at the parse level:
// the link lands under "Unknown" instead of being dropped.
func TestAnimegoEpisodesUnknownTranslationFallsBackToUnknown(t *testing.T) {
	t.Parallel()

	srv, _ := animegoStubServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, animegoEnvelope(`<button data-anime-player-target="provider" data-player="//kodikplayer.com/video/1/abc/720p"></button>`))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `<div data-anime-player-loader-url-value="/player/1"></div>`)
		})
	p := luaProvider(t, "animego", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x-1")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want the single film episode", len(episodes))
	}
	links := episodes[0].RawEmbeds["Unknown"]
	if len(links) != 1 || links[0] != "https://kodikplayer.com/video/1/abc/720p" {
		t.Errorf("embeds[Unknown] = %v, want the untitled provider under Unknown (https-prefixed)", episodes[0].RawEmbeds["Unknown"])
	}
}

// TestAnimegoResolveStreamSkippedExtractor covers the typed error for
// an embed URL whose extractor is deliberately unported (unreachable
// from the registered providers): the resolve path surfaces the
// extractor error wrapped in the provider context with its sentinel.
func TestAnimegoResolveStreamSkippedExtractor(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, animegoEnvelope(`<button data-anime-player-target="provider" data-player="https://csst.online/embed/2" data-translation-title="Studio Band"></button>`))
	})
	p := luaProvider(t, "animego", srv.URL)

	_, err := p.ResolveStream(context.Background(), contracts.Episode{RawID: "1"}, "Studio Band")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want ErrExtractFailed", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "animego" {
		t.Errorf("error = %v, want animego ProviderError", err)
	}
	if !strings.Contains(err.Error(), "extractor:csst") {
		t.Errorf("error = %v, want extractor:csst context", err)
	}
}

func TestAnimegoProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animego")
	if p.ID() != "animego" || p.Name() != "AnimeGo" || p.BaseURL() != "https://animego.me" {
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
