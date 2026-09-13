package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// [LIVE-VERIFIED 2026-09-13] anidub is NOT a Python-tree port: the Go
// provider was written against the live site after the frozen anicli-py
// roster. online.anidub.com is a DLE site whose POST search still
// renders results server-side (unlike sameband): GET
// /?do=search&subaction=search&story=<q> answers a results page reusing
// the catalog .th-item card template.
func TestAnidubSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anidub_search.html"))
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The DLE search form is a GET with the query in the URL (the
	// server filters; no client-side catalog filter like sameband).
	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}
	if rec.Path != "/" {
		t.Errorf("request path = %q, want /", rec.Path)
	}
	if rec.Query != "do=search&subaction=search&story=naruto" {
		t.Errorf("request query = %q, want the DLE search params", rec.Query)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 fixture cards", len(results))
	}
	if results[0].Title != "Наруто (спэшлы) / Naruto Specials [02 из 02]" {
		t.Errorf("Title = %q, want the .th-title text", results[0].Title)
	}
	if results[0].URL != "https://online.anidub.com/10856-naruto-speshly-naruto-specials-02-iz-02.html" {
		t.Errorf("URL = %q, want the a.th-in href verbatim", results[0].URL)
	}
	if results[0].SourceID != "anidub" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != srv.URL+"/uploads/posts/2023-08/poster-naruto-spjeshly.jpg" {
		t.Errorf("Poster = %q, want the site-root-prefixed img src", results[0].Poster)
	}
}

func TestAnidubSearchRussianQueryPercentEncoded(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body></body></html>")
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	if _, err := p.Search(context.Background(), "наруто"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// pyQuote encodes UTF-8 one byte at a time, spaces as %20 — never
	// the form-style "+" of url.Values.Encode.
	if !strings.HasPrefix(rec.Query, "do=search&subaction=search&story=%D0%BD%D0%B0%D1%80%D1%83%D1%82%D0%BE") {
		t.Errorf("request query = %q, want percent-encoded наруто", rec.Query)
	}
	if strings.Contains(rec.Query, "+") {
		t.Errorf("request query = %q, must not contain form-style +", rec.Query)
	}
}

func TestAnidubSearchNoResultsIsEmpty(t *testing.T) {
	t.Parallel()

	// A junk query answers the same shell with zero cards (verified
	// live: story=zxqjunknothing -> 0 cards, HTTP 200).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><body><div class="sect-content sect-items"></div></body></html>`)
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	results, err := p.Search(context.Background(), "zxqjunknothing")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAnidubSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

// The "Запасной плеер" tab carries one span per episode: "Серия N" with
// a sibnet shell embed in the data attribute. The "Основной плеер" span
// (ПЛЕЕР #1) is a full-title playlist player whose episodes cannot be
// split client-side and is skipped.
func TestAnidubGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anidub_anime.html"))
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/12254-blich.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 8 {
		t.Fatalf("episodes = %d, want 8 series spans", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[7].Num != "8" {
		t.Errorf("nums = %q..%q, want 1..8 in document order", episodes[0].Num, episodes[7].Num)
	}
	if episodes[0].Title != "Серия 1" {
		t.Errorf("Title = %q, want the span text", episodes[0].Title)
	}
	raw := episodes[0].RawEmbeds["AniDUB"]
	if len(raw) != 1 || raw[0] != "https://video.sibnet.ru/shell.php?videoid=6251180" {
		t.Errorf("RawEmbeds = %v, want the sibnet shell embed from data", raw)
	}
	if episodes[0].RawID != "1" {
		t.Errorf("RawID = %q, want the episode number", episodes[0].RawID)
	}
}

// Movie pages carry a single "Серия 1" span and no primary playlist
// tab (verified live on the Naruto movie pages).
func TestAnidubGetEpisodesMovieSingle(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="fplayer tabs-box">
			<div class="tabs-b video-box"><div class="fthree tabs-box">
				<div class="tabs-sel series-tab">
					<span data="https://video.sibnet.ru/shell.php?videoid=3653578">Серия 1</span>
				</div>
			</div></div>
		</div>`)
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/155-naruto-movie-2-2005.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want one movie episode", episodes)
	}
}

// Pages without the .fplayer block (or with only the unsplittable
// ПЛЕЕР #1 playlist span) yield no episodes, no error.
func TestAnidubGetEpisodesNoPlayerIsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no player here</body></html>")
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/none.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

// ResolveStream runs the episode embeds through the extractor factory
// (animego pattern): the sibnet extractor turns shell.php embeds into a
// 480p mp4 source. The fake shell page reproduces the sibnet
// player-page shape so no network is touched.
func TestAnidubResolveStream(t *testing.T) {
	t.Parallel()

	// The embed URL is served locally but contains "sibnet" so the
	// extractor's substring gate matches while the fetch stays on the
	// test server.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `player = new Playerjs({src: "https://video.sibnet.ru/videos/6251180/ep1.mp4"});`)
	})
	p := newAnidub(srv.URL, testClient(t, "anidub"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"AniDUB": {srv.URL + "/sibnet/shell.php?videoid=6251180"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AniDUB")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links = %v, want a 480 sibnet entry", stream.Links)
	}
	if src.URL != "https://video.sibnet.ru/videos/6251180/ep1.mp4" {
		t.Errorf("URL = %q, want the absolute sibnet mp4", src.URL)
	}
	if src.Headers["Referer"] != srv.URL+"/sibnet/shell.php?videoid=6251180" {
		t.Errorf("Referer = %q, want the embed URL", src.Headers["Referer"])
	}
	if stream.DubName != "AniDUB" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestAnidubResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newAnidub(AnidubBase, testClient(t, "anidub"))

	stream, err := p.ResolveStream(context.Background(),
		contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for an unknown dub", stream.Links)
	}
}

func TestAnidubProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnidub(AnidubBase, testClient(t, "anidub"))
	if p.ID() != "anidub" || p.Name() != "AniDUB" || p.BaseURL() != AnidubBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// Russian dub = wanted-language audio + video (PR23 semantics:
	// SourceType describes content suitability, and anidub's RU dub
	// audio makes it BOTH).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}
