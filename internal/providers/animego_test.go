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

// animegoServer serves the three-request flow of GetEpisodes: the anime
// page, then the player API.
func animegoServer(t *testing.T, animePage, playerBody string, playerStatus int) (*httptest.Server, *recordedRequest) {
	t.Helper()

	return fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/player"):
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

	results, err := p.Search(context.Background(), "re:zero")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/search/anime" {
		t.Errorf("request path = %q", rec.Path)
	}
	if rec.Query != "q=re%3Azero" {
		t.Errorf("request query = %q, want q=re%%3Azero", rec.Query)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (third .col-ul-2 lacks a title link and must be skipped)", len(results))
	}
	if results[0].Title != "Re:Zero. Жизнь с нуля в другом мире" {
		t.Errorf("Title = %q, want the a[title] attribute", results[0].Title)
	}
	if results[0].URL != "https://animego.one/anime/re-zero-kara-hajimeru-isekai-seikatsu-1469" {
		t.Errorf("URL = %q", results[0].URL)
	}
	if results[0].SourceID != "animego" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != "https://animego.one/media/thumbs/rezero.jpg" {
		t.Errorf("Poster = %q, want the lazy[data-original] attribute", results[0].Poster)
	}
	// Second item has no thumb node: poster stays empty.
	if results[1].Poster != "" {
		t.Errorf("Poster = %q, want empty without .lazy[data-original]", results[1].Poster)
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
	// Python re-asserts the configured user agent (animego.py:24); the
	// netclient already sends cfg.UserAgent on every request.
	if got := rec.Header.Get("User-Agent"); got == "" {
		t.Error("User-Agent header missing")
	}
}

func TestAnimegoGetEpisodesSeries(t *testing.T) {
	t.Parallel()

	// Route-aware stub: page, player API, then the PR44 tier-1 dub-
	// list fetch (/anime/series for the first episode).
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/anime/series"):
			_, _ = w.Write(fixture(t, "animego_series.json"))
		case strings.HasSuffix(r.URL.Path, "/player"):
			_, _ = w.Write(fixture(t, "animego_player_series.json"))
		default:
			_, _ = w.Write(fixture(t, "animego_anime.html"))
		}
	}))
	t.Cleanup(srv.Close)
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/re-zero-1469")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The request sequence: the anime page carries the numeric id, the
	// player API URL carries it plus _allow=true (animego.py:61), the
	// tier-1 dub-list fetch rides /anime/series?id=901.
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/anime/re-zero-1469", "/anime/1469/player?_allow=true", "/anime/series?id=901"}
	if len(paths) != len(want) {
		t.Fatalf("requests = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("request[%d] = %q, want %q", i, paths[i], want[i])
		}
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2 (third carousel item lacks data-id)", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[0].RawID != "901" || episodes[0].Title != "Начало конца" {
		t.Errorf("episode 1 = %+v", episodes[0])
	}
	if episodes[1].Num != "2" || episodes[1].RawID != "902" {
		t.Errorf("episode 2 = %+v", episodes[1])
	}
	if episodes[1].Title != "Ускорение" {
		t.Errorf("episode 2 Title = %q", episodes[1].Title)
	}
	// Episode one keeps its real player links; episode two carries the
	// release's dub keys with EMPTY lists (on-demand resolve).
	if len(episodes[0].RawEmbeds["AniLib"]) == 0 {
		t.Errorf("episode 1 embeds = %v, want the real links from the tier-1 fetch", episodes[0].RawEmbeds)
	}
	if links := episodes[1].RawEmbeds["AniLib"]; links == nil || len(links) != 0 {
		t.Errorf("episode 2 AniLib links = %v, want an empty list", links)
	}
}

func TestAnimegoGetEpisodesFilmParsesEmbedsInline(t *testing.T) {
	t.Parallel()

	srv, _ := animegoServer(t,
		`<div class="br-2"><div class="my-list-anime" id="my-list-777"></div></div>`,
		string(fixture(t, "animego_player_film.json")), http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/film-777")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want the single film episode", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" || ep.Title != "Фильм" || ep.RawID != "777" {
		t.Errorf("film episode = %+v (animego.py:83-88)", ep)
	}

	// Inline embed parsing over the player content, exercising the
	// `#video-players > span` fallback branch (animego.py:115).
	embeds := ep.RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("embeds = %v, want 2 dubs", embeds)
	}
	anilib, ok := embeds["AniLib"]
	if !ok || len(anilib) != 2 {
		t.Fatalf("embeds[AniLib] = %v, want 2 players", anilib)
	}
	// Protocol-relative data-player gains https: (animego.py:122).
	if anilib[0] != "https://aniboom.one/embed/123?ep=1" {
		t.Errorf("anilib[0] = %q", anilib[0])
	}
	// Protocol-relative data-player URLs gain https: in parseEmbeds
	// (animego.py:122) — including direct media links.
	if anilib[1] != "https://cdn.example.com/static/film.m3u8" {
		t.Errorf("anilib[1] = %q", anilib[1])
	}
	band, ok := embeds["Studio Band"]
	if !ok || len(band) != 1 || band[0] != "https://kodik.info/serial/12345/xyz" {
		t.Errorf("embeds[Studio Band] = %v", embeds["Studio Band"])
	}
}

func TestAnimegoGetEpisodesMissingIDNodeReturnsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := animegoServer(t, `<html><body>no id node here</body></html>`, `{}`, http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/broken")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without .br-2 .my-list-anime", len(episodes))
	}
}

func TestAnimegoGetEpisodesMalformedPlayerJSONIsTypedError(t *testing.T) {
	t.Parallel()

	srv, _ := animegoServer(t,
		`<div class="br-2"><div class="my-list-anime" id="my-list-7"></div></div>`,
		"<html>not json</html>", http.StatusOK)
	p := newAnimego(srv.URL, testClient(t, "animego"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if err == nil {
		t.Fatal("malformed player JSON must fail (Python json.loads raises)")
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
		_, _ = w.Write(fixture(t, "animego_series.json"))
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := &contracts.Episode{Num: "1", RawID: "901", RawEmbeds: map[string][]string{}}
	got, err := p.FetchDubs(context.Background(), episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if got != episode {
		t.Fatal("FetchDubs must return the same episode pointer")
	}
	if rec.Path != "/anime/series" || rec.Query != "id=901" {
		t.Errorf("request = %s?%s, want /anime/series?id=901", rec.Path, rec.Query)
	}

	embeds := episode.RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("embeds = %v, want 2 dubs", embeds)
	}
	if got := embeds["AniLib"]; len(got) != 1 || got[0] != "https://aniboom.one/embed/9001?ep=1" {
		t.Errorf("embeds[AniLib] = %v (protocol-relative fixed)", got)
	}
	// data-provide-dubbing=999 has no #video-dubbing entry: dub name
	// falls back to "Unknown" (animego.py:124).
	if got := embeds["Unknown"]; len(got) != 1 || got[0] != "https://player-cdn.example/embed/zzz" {
		t.Errorf("embeds[Unknown] = %v, want the unmatched dubbing under Unknown (https-prefixed)", got)
	}
}

func TestAnimegoFetchDubsSkipsWhenEmbedsPresent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("FetchDubs must not hit the network when raw embeds exist")
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := &contracts.Episode{
		RawID:     "901",
		RawEmbeds: map[string][]string{"AniLib": {"https://x/y.m3u8"}},
	}
	got, err := p.FetchDubs(context.Background(), episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}
	if got != episode {
		t.Fatal("FetchDubs must return the same episode pointer")
	}
	if got.RawEmbeds["AniLib"][0] != "https://x/y.m3u8" {
		t.Errorf("existing embeds must survive: %v", got.RawEmbeds)
	}
}

func TestAnimegoResolveStreamDirectFallback(t *testing.T) {
	t.Parallel()

	p := newAnimego(AnimeGoBase, testClient(t, "animego"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{
			"AniLib": {"//cdn.example.com/static/film.m3u8"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AniLib")
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
	if stream.DubName != "AniLib" {
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
}
