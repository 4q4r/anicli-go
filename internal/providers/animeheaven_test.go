package providers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// [LIVE-VERIFIED 2026-09-25] animeheaven is NOT a port: animeheaven.me is
// an EN sub-only catalog written against the live site (the AniVault
// scraper family — SH0MIK/Anivault-Scraper and jsmat0m/Anivault-Scraper,
// src/scrapers/animeheaven.ts — documents the same request shapes). NOT
// behind Cloudflare (no FlareSolverr in the reference either). The wire
// shapes, all captured into testdata with byte fidelity:
//
//   - search: GET /fastsearch.php?xhr=1&s=<query> (Accept:
//     text/html,*/*) → anchor cards a[href*="anime.php?"] whose id IS
//     the href query part (5-char base36-ish, e.g. 11t3p); title from
//     div.fastname (HTML entities), img[alt] fallback; junk query
//     answers HTTP 200 «No results found» (zero anchors).
//   - episodes: GET /anime.php?<id> → a[onmouseover*="gateh("] /
//     a[onclick*="gatea("] anchors; the gate key is the quoted arg of
//     gateh/gatea. LIVE DELTA vs the reference scrapers: the real
//     markup single-quotes its attributes and puts a SPACE after the
//     paren — onmouseover='gateh( "150ade…")' — which the reference
//     regex gate[ha]\("([^"]+)" (no \s*) can no longer match; the
//     provider's regex tolerates it. Episode number is the div.watch2
//     text ("71"), rendered newest-first; the provider sorts ascending
//     and dedupes by key.
//   - watch: GET /gate.php with Cookie: key=<episode key>, Referer:
//     <base>/ (verified stateless: a cold jar with only that cookie
//     answers 200) → <video><source src="https://rk.animeheaven.me/
//     video.mp4?…"> direct MP4 (type='video/mp4', HTTP 206 with Range).
//     The 2nd/3rd sources are the site's own onerror-fallback CDNs
//     (ct/ck …&error / &error2 — a direct hit answers 404) and the 4th
//     duplicates the 1st; the provider picks the FIRST /video.mp4
//     source (the reference rule), which the fixture pins to the rk
//     host. The gate page exposes no quality selector — the captured
//     file's MP4 tkhd reports 928x720, so the single link is labelled
//     720.

func TestAnimeHeavenSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animeheaven_search.html"))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}
	if rec.Path != "/fastsearch.php" {
		t.Errorf("request path = %q, want /fastsearch.php", rec.Path)
	}
	// url.Values.Encode() canonicalizes alphabetically; the server
	// treats parameter order as irrelevant (verified live).
	if want := "s=black+lagoon&xhr=1"; rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}
	if got := rec.Header.Get("Accept"); got != "text/html,*/*" {
		t.Errorf("request Accept = %q, want text/html,*/*", got)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 fixture cards", len(results))
	}
	first := results[0]
	if first.Title != "Black Lagoon: Roberta's Blood Trail" {
		t.Errorf("Title = %q, want the div.fastname text (entities decoded)", first.Title)
	}
	if first.URL != srv.URL+"/anime.php?11t3p" {
		t.Errorf("URL = %q, want the absolutized anime.php link", first.URL)
	}
	if first.SourceID != "animeheaven" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster != srv.URL+"/image.php?3rmhz" {
		t.Errorf("Poster = %q, want the absolutized img src", first.Poster)
	}
	if results[2].Title != "Black Lagoon" || results[2].URL != srv.URL+"/anime.php?538gu" {
		t.Errorf("results[2] = %q / %q", results[2].Title, results[2].URL)
	}
}

func TestAnimeHeavenSearchAltFallback(t *testing.T) {
	t.Parallel()

	// A card whose fastname div is empty falls back to the img[alt]
	// (the reference rule); a card with neither is skipped.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<a class='ac' href='/anime.php?altid'><div class='fastitem bc1 ac'>` +
			`<div class='fastimg'><img class='coverimg' src='/image.php?px1' alt='Alt Title'></div>` +
			`<div class='fastname'></div></div></a>` +
			`<a class='ac' href='/anime.php?noid'><div class='fastitem bc1 ac'>` +
			`<div class='fastimg'><img class='coverimg' src='/image.php?px2' alt=''></div>` +
			`<div class='fastname'></div></div></a>`))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	results, err := p.Search(context.Background(), "alt")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the alt-titled card)", len(results))
	}
	if results[0].Title != "Alt Title" || results[0].URL != srv.URL+"/anime.php?altid" {
		t.Errorf("results[0] = %q / %q", results[0].Title, results[0].URL)
	}
}

func TestAnimeHeavenSearchNoResultsIsEmpty(t *testing.T) {
	t.Parallel()

	// A junk query answers the «No results found» shell (verified live:
	// s=zxqjunknothing99 → HTTP 200, zero anime.php anchors).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animeheaven_search_miss.html"))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	results, err := p.Search(context.Background(), "zxqjunknothing99")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAnimeHeavenSearchTimeout(t *testing.T) {
	t.Parallel()

	p := newAnimeHeaven("http://"+newDeadListener(t).Addr().String(), testClient(t, "animeheaven"))

	_, err := p.Search(context.Background(), "black lagoon")
	if err == nil {
		t.Fatal("Search on a dead listener must fail")
	}
	if !errors.Is(err, contracts.ErrProviderTimeout) {
		t.Fatalf("err = %v, want ErrProviderTimeout", err)
	}
}

func TestAnimeHeavenGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "nc7bk" {
			t.Errorf("request query = %q, want nc7bk", r.URL.RawQuery)
		}
		_, _ = w.Write(fixture(t, "animeheaven_anime.html"))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php?nc7bk")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/anime.php" {
		t.Errorf("request path = %q, want /anime.php", rec.Path)
	}
	// The fixture page renders 71 episodes, newest-first; the provider
	// must sort ascending and dedupe by gate key.
	if len(episodes) != 71 {
		t.Fatalf("episodes = %d, want 71", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" {
		t.Errorf("episodes[0].Num = %q, want 1 (ascending sort)", first.Num)
	}
	if first.Title != "Episode 1" {
		t.Errorf("episodes[0].Title = %q, want Episode 1", first.Title)
	}
	const ep1Key = "1383adfc6a074fcceed863c8b9e2b5db"
	if first.RawID != ep1Key {
		t.Errorf("episodes[0].RawID = %q, want the ep-1 gate key %q", first.RawID, ep1Key)
	}
	embeds := first.RawEmbeds[ahServiceDub]
	if len(embeds) != 1 || embeds[0] != ep1Key {
		t.Errorf("RawEmbeds[%s] = %v, want [%s]", ahServiceDub, embeds, ep1Key)
	}
	last := episodes[70]
	if last.Num != "71" {
		t.Errorf("episodes[70].Num = %q, want 71", last.Num)
	}
	if last.RawID != "150ade6c175b08e68dd1605332596272" {
		t.Errorf("episodes[70].RawID = %q, want the ep-71 gate key", last.RawID)
	}
}

func TestAnimeHeavenGetEpisodesBadURL(t *testing.T) {
	t.Parallel()

	// An anime URL without an id query part is caller error — typed
	// invalid input, no request fired.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected for a malformed anime URL")
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php")
	if err == nil {
		t.Fatal("GetEpisodes without an id must fail")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestAnimeHeavenResolveStream(t *testing.T) {
	t.Parallel()

	const epKey = "1383adfc6a074fcceed863c8b9e2b5db"
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animeheaven_gate.html"))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	episode := contracts.Episode{
		Num:       "1",
		RawID:     epKey,
		RawEmbeds: map[string][]string{ahServiceDub: {epKey}},
	}
	stream, err := p.ResolveStream(context.Background(), episode, ahServiceDub)
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	if rec.Path != "/gate.php" {
		t.Errorf("request path = %q, want /gate.php", rec.Path)
	}
	if got := rec.Header.Get("Cookie"); got != "key="+epKey {
		t.Errorf("request Cookie = %q, want key=%s", got, epKey)
	}
	if got := rec.Header.Get("Referer"); got != srv.URL+"/" {
		t.Errorf("request Referer = %q, want %s/", got, srv.URL)
	}

	if stream.DubName != ahServiceDub {
		t.Errorf("DubName = %q, want %q", stream.DubName, ahServiceDub)
	}
	link, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want the single 720 entry", stream.Links)
	}
	// The fixture carries four <source> elements: the rk primary, the
	// ct/ck onerror-fallback CDNs and a duplicate of the primary. The
	// FIRST /video.mp4 source is the playable one (verified live:
	// HTTP 206 video/mp4 with Range support; the &error hosts 404).
	if !strings.Contains(link.URL, "rk.animeheaven.me/video.mp4?") {
		t.Errorf("URL = %q, want the primary rk host", link.URL)
	}
	if link.Quality != "720" {
		t.Errorf("Quality = %q, want 720", link.Quality)
	}
	if link.Type != "mp4" {
		t.Errorf("Type = %q, want mp4", link.Type)
	}
	if got := link.Headers["Referer"]; got != srv.URL+"/" {
		t.Errorf("link Referer = %q, want %s/", got, srv.URL)
	}
}

func TestAnimeHeavenResolveStreamFallbackSource(t *testing.T) {
	t.Parallel()

	// A gate page without a /video.mp4 source degrades to the first
	// http(s) source (the reference `|| sources[0]` rule).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<video id='vid'><source src='https://mirror.example/v/448.mkv' type='video/mp4'></video>`))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	episode := contracts.Episode{
		Num:       "2",
		RawID:     "k2",
		RawEmbeds: map[string][]string{ahServiceDub: {"k2"}},
	}
	stream, err := p.ResolveStream(context.Background(), episode, ahServiceDub)
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	link, ok := stream.Links["720"]
	if !ok || link.URL != "https://mirror.example/v/448.mkv" {
		t.Fatalf("Links = %v, want the single fallback source", stream.Links)
	}
}

func TestAnimeHeavenResolveStreamNoSources(t *testing.T) {
	t.Parallel()

	// A gate page with no playable source is a typed extraction
	// failure, not an empty success.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class='gate'>dead</div></body></html>`))
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	episode := contracts.Episode{
		Num:       "3",
		RawID:     "k3",
		RawEmbeds: map[string][]string{ahServiceDub: {"k3"}},
	}
	_, err := p.ResolveStream(context.Background(), episode, ahServiceDub)
	if err == nil {
		t.Fatal("ResolveStream on a source-less gate page must fail")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

func TestAnimeHeavenResolveStreamEmptyEmbeds(t *testing.T) {
	t.Parallel()

	// No embeds under the requested dub: an empty stream, no error
	// (the anizone semantics — the dub filter layer relies on it).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected for empty embeds")
	})
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "4"}, ahServiceDub)
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty", stream.Links)
	}
}

func TestAnimeHeavenIdentity(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	p := newAnimeHeaven(srv.URL, testClient(t, "animeheaven"))

	if p.ID() != "animeheaven" {
		t.Errorf("ID = %q", p.ID())
	}
	if p.Name() != "AnimeHeaven" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.BaseURL() != srv.URL {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both (ja audio, en subs)", p.SourceType())
	}
	if p.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %q, want ja", p.ContentLanguage())
	}
	if p.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin (EN index)", p.NamePreference())
	}
	// The shared «black lagoon» probe hits the catalog (verified live:
	// 3 cards), so NO SmokeQuery may be declared — the optional
	// capability stays off (the PR51 mechanism: undeclared = shared
	// probes apply).
	if _, declared := any(p).(contracts.SmokeQueryProvider); declared {
		t.Error("SmokeQueryProvider declared, want undeclared (shared probe hits)")
	}
}

func TestAnimeHeavenParseGateKey(t *testing.T) {
	t.Parallel()

	// The LIVE markup shape: single-quoted attributes with a space
	// after the paren (the reference scraper regex no longer matches
	// it); the provider's regex tolerates both shapes.
	for _, attr := range []string{
		`gateh( "150ade6c175b08e68dd1605332596272")`, // live: space after (
		`gateh("150ade6c175b08e68dd1605332596272")`,  // reference: no space
		`gatea( "150ade6c175b08e68dd1605332596272")`,
		`gatea("150ade6c175b08e68dd1605332596272")`,
	} {
		if got := ahParseGateKey(attr); got != "150ade6c175b08e68dd1605332596272" {
			t.Errorf("ahParseGateKey(%q) = %q", attr, got)
		}
	}
	if got := ahParseGateKey(`ratethis("x")`); got != "" {
		t.Errorf("ahParseGateKey on a foreign attr = %q, want empty", got)
	}
}

// ahGateFixtureSanity keeps the fixture honest: the captured gate page
// must still carry the four <source> elements the pick rule relies on.
func TestAnimeHeavenGateFixtureSanity(t *testing.T) {
	t.Parallel()

	doc := string(fixture(t, "animeheaven_gate.html"))
	if !bytes.Contains([]byte(doc), []byte("rk.animeheaven.me/video.mp4")) {
		t.Fatal("gate fixture lost the primary rk source")
	}
	if strings.Count(doc, "<source") != 4 {
		t.Fatalf("gate fixture carries %d <source> elements, want 4", strings.Count(doc, "<source"))
	}
}
