package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// animegoServer serves the two-request flow of GetEpisodes: the anime
// page, then the /player/{id} JSON fragment.
func animegoServer(t *testing.T, animePage, playerBody string, playerStatus int) (*httptest.Server, *recordedRequest) {
	t.Helper()

	return fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/player/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(playerStatus)
			_, _ = fmt.Fprint(w, playerBody)
		default:
			_, _ = fmt.Fprint(w, animePage)
		}
	})
}

func TestAnimegoSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animego_search.html"))
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

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
	// The new site emits RELATIVE hrefs; Search must absolutize them
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
	p := newAnimego(srv.URL, testClient(t, "animego"))

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

	// Route-aware stub: the anime page, then /player/{id}. The new
	// site's player fragment ALREADY carries the first episode's
	// provider buttons, so the PR44 tier-1 dub-list fetch is free:
	// exactly two requests cover the release.
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/player/"):
			_, _ = w.Write(fixture(t, "animego_player_series.json"))
		default:
			_, _ = w.Write(fixture(t, "animego_anime.html"))
		}
	}))
	t.Cleanup(srv.Close)
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/piraty-chernoy-laguny-2115")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"/anime/piraty-chernoy-laguny-2115", "/player/2115"}
	if len(paths) != len(want) {
		t.Fatalf("requests = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("request[%d] = %q, want %q", i, paths[i], want[i])
		}
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	// New carousel: data-episode-number + data-episode, no per-episode
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
	// fragment (AniBoom + Kodik under the MC Entertainment translation);
	// episode two carries the release's dub keys with EMPTY lists
	// (on-demand resolve, PR44 owner model).
	if links := episodes[0].RawEmbeds["MC Entertainment"]; len(links) != 2 {
		t.Errorf("episode 1 MC Entertainment links = %v, want the AniBoom+Kodik pair from the fragment", links)
	}
	if links := episodes[1].RawEmbeds["MC Entertainment"]; links == nil || len(links) != 0 {
		t.Errorf("episode 2 MC Entertainment links = %v, want an empty list", links)
	}
}

func TestAnimegoGetEpisodesFilmParsesEmbedsInline(t *testing.T) {
	t.Parallel()

	srv, _ := animegoServer(t,
		`<div class="player__video" data-controller="anime-player-loader" data-anime-player-loader-url-value="/player/4060"></div>`,
		string(fixture(t, "animego_player_film.json")), http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

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

	srv, _ := animegoServer(t, `<html><body>no loader node here</body></html>`, `{}`, http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

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

	srv, _ := animegoServer(t,
		`<div data-anime-player-loader-url-value="/player/7"></div>`,
		"<html>not json</html>", http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

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
	p := newAnimego(srv.URL, testClient(t, "animego"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestAnimegoFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animego_videos.json"))
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := &contracts.Episode{Num: "5", RawID: "27784", RawEmbeds: map[string][]string{}}
	got, err := p.FetchDubs(context.Background(), episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if got != episode {
		t.Fatal("FetchDubs must return the same episode pointer")
	}
	if rec.Path != "/player/videos/27784" {
		t.Errorf("request = %s, want /player/videos/27784", rec.Path)
	}

	embeds := episode.RawEmbeds
	if len(embeds) != 1 {
		t.Fatalf("embeds = %v, want 1 translation", embeds)
	}
	links := embeds["MC Entertainment"]
	if len(links) != 2 {
		t.Fatalf("embeds[MC Entertainment] = %v, want the AniBoom+Kodik pair", links)
	}
	if !strings.HasPrefix(links[0], "https://aniboom.one/embed/") {
		t.Errorf("links[0] = %q, want the https-prefixed aniboom embed", links[0])
	}
	if !strings.HasPrefix(links[1], "https://kodikplayer.com/seria/") {
		t.Errorf("links[1] = %q, want the https-prefixed kodik embed", links[1])
	}
}

// TestAnimegoFetchDubsUnknownTranslationFallsBackToUnknown covers a
// provider button missing data-translation-title: the link lands under
// "Unknown" instead of being dropped.
func TestAnimegoFetchDubsUnknownTranslationFallsBackToUnknown(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","message":null,"data":{"content":"<button data-anime-player-target=\"provider\" data-player=\"//kodikplayer.com/video/1/abc/720p\"></button>"}}`)
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := &contracts.Episode{Num: "1", RawID: "1", RawEmbeds: map[string][]string{}}
	if _, err := p.FetchDubs(context.Background(), episode); err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if links := episode.RawEmbeds["Unknown"]; len(links) != 1 || links[0] != "https://kodikplayer.com/video/1/abc/720p" {
		t.Errorf("embeds[Unknown] = %v, want the untitled provider under Unknown (https-prefixed)", episode.RawEmbeds["Unknown"])
	}
}

func TestAnimegoFetchDubsSkipsWhenEmbedsPresent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("FetchDubs must not hit the network when raw embeds exist")
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := &contracts.Episode{
		RawID:     "27779",
		RawEmbeds: map[string][]string{"MC Entertainment": {"https://x/y.m3u8"}},
	}
	got, err := p.FetchDubs(context.Background(), episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if got != episode {
		t.Fatal("FetchDubs must return the same episode pointer")
	}
	if got.RawEmbeds["MC Entertainment"][0] != "https://x/y.m3u8" {
		t.Errorf("existing embeds must survive: %v", got.RawEmbeds)
	}
}

func TestAnimegoResolveStreamDirectFallback(t *testing.T) {
	t.Parallel()

	p := newAnimego(AnimeGoBase, testClient(t, "animego"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"MC Entertainment": {"//cdn.example.com/static/film.m3u8"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "MC Entertainment")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want the direct 720 fallback", stream.Links)
	}
	if src.URL != "//cdn.example.com/static/film.m3u8" {
		t.Errorf("URL = %q, want the link untouched", src.URL)
	}
	if stream.DubName != "MC Entertainment" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestAnimegoResolveStreamSkippedExtractor covers the typed error for an
// embed URL whose extractor is deliberately unported (unreachable from
// the registered providers): the resolve path surfaces the extractor
// error wrapped in the provider context instead of the old pending
// marker.
func TestAnimegoResolveStreamSkippedExtractor(t *testing.T) {
	t.Parallel()

	p := newAnimego(AnimeGoBase, testClient(t, "animego"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"Studio Band": {"https://csst.online/embed/2"},
		},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Studio Band")
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

func TestAnimegoResolveStreamEmptyDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newAnimego(AnimeGoBase, testClient(t, "animego"))

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty", stream.Links)
	}
}

func TestAnimegoProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimego(AnimeGoBase, testClient(t, "animego"))
	if p.ID() != "animego" || p.Name() != "AnimeGo" || p.BaseURL() != AnimeGoBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if AnimeGoBase != "https://animego.me" {
		t.Errorf("AnimeGoBase = %q, want the live animego.me base", AnimeGoBase)
	}
}
