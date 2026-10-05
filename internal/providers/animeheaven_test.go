package providers

// Fixture provenance: animeheaven_search.html, animeheaven_search_miss.html,
// animeheaven_anime.html and animeheaven_gate.html are VERBATIM live
// captures from animeheaven.me taken on 2026-09-25 (re-verified live
// 2026-10-05: same three «black lagoon» cards and ids, the space-after-
// paren gateh shape, four gate <source> elements — the primary edge
// host rotated rk→cu but the first-/video.mp4 pick rule is
// host-agnostic). NOT behind Cloudflare; anonymous.
//
// PR126: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animeheaven/main.lua) — these tests
// pin the script through the same contracts.Provider surface and the
// same fixtures the compiled Go implementation was held to. Contract
// shifts forced by the fresh-sandbox Lua adapter (the anilibria/
// animevost precedent), documented here rather than hidden:
//
//   - the gate key rides episode RawID alone (the only state channel
//     into the per-invocation streams(raw_id, dub) call); RawEmbeds
//     keeps carrying the same key for consumers;
//   - the empty-embeds caller-bug guard fires on an empty RawID (the
//     adapter does not pass RawEmbeds into streams) — same typed
//     ErrInvalidInput, same no-request-before-failure shape;
//   - the SmokeQueryProvider capability is adapter-declared with an
//     empty query (the content_lang adapter keeps the capability
//     surface assertions-stable) — the shared «black lagoon» probe
//     still applies, Go parity.
//
// No anicli.extract leg: the gate answers DIRECT mp4 <source> URLs
// (no iframe embed anywhere) — the anilibria no-extractor precedent.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestAnimeHeavenSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animeheaven_search.html"))
	})
	p := luaProvider(t, "animeheaven", srv.URL)

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
	// The script builds the query in site order (xhr first, then s;
	// query_escape encodes the space "+", the reference axios shape).
	if want := "xhr=1&s=black+lagoon"; rec.Query != want {
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
	p := luaProvider(t, "animeheaven", srv.URL)

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
	p := luaProvider(t, "animeheaven", srv.URL)

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

	// A listener whose port is closed: connections are refused. The
	// origin is slow even when healthy (last live matrix 11.4s) — the
	// 60ms budget exhausts the netclient retry ladder and the failure
	// maps onto ErrProviderTimeout through the Lua transport markers.
	dead := newDeadListener(t)

	cfg := config.Default().Network
	cfg.RequestTimeout = 60 * time.Millisecond
	p := luaProviderWithNet(t, "animeheaven", "http://"+dead.Addr().String(), cfg)

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
	p := luaProvider(t, "animeheaven", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php?nc7bk")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/anime.php" {
		t.Errorf("request path = %q, want /anime.php", rec.Path)
	}
	// The fixture page renders 71 episodes, newest-first; the script
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
	embeds := first.RawEmbeds["Sub"]
	if len(embeds) != 1 || embeds[0] != ep1Key {
		t.Errorf("RawEmbeds[Sub] = %v, want [%s]", embeds, ep1Key)
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
	p := luaProvider(t, "animeheaven", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php")
	if err == nil {
		t.Fatal("GetEpisodes without an id must fail")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestAnimeHeavenGetEpisodesNoAnchors(t *testing.T) {
	t.Parallel()

	// A parsed page with no gate anchors (markup drift, removed title,
	// substituted page) is a typed NOT-FOUND wall — an empty list here
	// would fake a healthy title with no episodes (the roster
	// doctrine: anizone/animedia/anikado/anitokyo/animiku/animevib).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class='content'>nothing here</div></body></html>`))
	})
	p := luaProvider(t, "animeheaven", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php?nc7bk")
	if err == nil {
		t.Fatal("GetEpisodes on an anchor-less page must fail")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAnimeHeavenResolveStream(t *testing.T) {
	t.Parallel()

	const epKey = "1383adfc6a074fcceed863c8b9e2b5db"
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animeheaven_gate.html"))
	})
	p := luaProvider(t, "animeheaven", srv.URL)

	episode := contracts.Episode{
		Num:       "1",
		RawID:     epKey,
		RawEmbeds: map[string][]string{"Sub": {epKey}},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "Sub")
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

	if stream.DubName != "Sub" {
		t.Errorf("DubName = %q, want %q", stream.DubName, "Sub")
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
	p := luaProvider(t, "animeheaven", srv.URL)

	episode := contracts.Episode{
		Num:       "2",
		RawID:     "k2",
		RawEmbeds: map[string][]string{"Sub": {"k2"}},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "Sub")
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
	p := luaProvider(t, "animeheaven", srv.URL)

	episode := contracts.Episode{
		Num:       "3",
		RawID:     "k3",
		RawEmbeds: map[string][]string{"Sub": {"k3"}},
	}
	_, err := p.ResolveStream(context.Background(), episode, "Sub")
	if err == nil {
		t.Fatal("ResolveStream on a source-less gate page must fail")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

func TestAnimeHeavenResolveStreamEmptyKey(t *testing.T) {
	t.Parallel()

	// An episode without a key is a caller bug (the wave-A review F2 /
	// animedia precedent): a silent empty MediaStream would look like a
	// healthy resolution. The Lua contract surfaces it as an empty
	// RawID (the adapter does not pass RawEmbeds into streams) — same
	// typed invalid-input wall, no request fired. (resolveAllStreams
	// only passes keys present in RawEmbeds, so TUI flows never hit
	// this — the guard is the typed contract.)
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected for a missing key")
	})
	p := luaProvider(t, "animeheaven", srv.URL)

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "4"}, "Sub")
	if err == nil {
		t.Fatal("ResolveStream with no key must fail")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty", stream.Links)
	}
}

// TestAnimeHeavenProviderMeta pins the identity block through the Lua
// adapter: the site root as BaseURL, the ja content language (JA audio,
// EN subs — the site carries no dub option), SourceTypeBoth and the
// latin-only search index (PR42: romaji/english titles match, Cyrillic
// queries are guaranteed-zero).
func TestAnimeHeavenProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animeheaven")
	if p.ID() != "animeheaven" || p.Name() != "AnimeHeaven" {
		t.Errorf("ID/Name = %q/%q", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://animeheaven.me" {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both (ja audio, en subs)", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok || lc.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %v, want ja", lc)
	}
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin (EN index)", got)
	}
}

// TestAnimeHeavenSmokeQueryUndeclared pins the smoke routing: the
// catalog answers the shared latin probe (verified live: «black
// lagoon» → 3 cards), so the script declares no probe of its own —
// the adapter-declared capability answers empty and the shared probe
// applies (Go parity: the compiled provider implemented no
// SmokeQueryProvider either).
func TestAnimeHeavenSmokeQueryUndeclared(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animeheaven")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the content_lang adapter must keep the capability surface assertions-stable")
	}
	if got := sq.SmokeQuery(); got != "" {
		t.Errorf("SmokeQuery = %q, want empty (the shared latin probe applies)", got)
	}
}

// TestAnimeHeavenGateKeyShapes drives the gate-key parser through the
// episodes surface (the script is a black box — the compiled provider
// unit-tested ahParseGateKey directly): the LIVE markup shape
// (single-quoted attributes with a space after the paren, which broke
// the reference scraper regex) AND the reference no-space shape must
// both parse, gateh and gatea alike; foreign handlers are ignored.
func TestAnimeHeavenGateKeyShapes(t *testing.T) {
	t.Parallel()

	const key = "150ade6c175b08e68dd1605332596272"
	mk := func(attr string) string {
		return `<a class='c' ` + attr + ` href='gate.php'>` +
			`<div class='trackep0 watch bc2'><div class='watch2 bc '>1</div></div></a>`
	}
	// One anchor per shape; the dedupe-by-key rule collapses the four
	// same-key parses into ONE episode.
	page := `<html><body>` +
		mk(`onmouseover='gateh( "`+key+`")'`) +
		mk(`onclick='gatea("`+key+`")'`) +
		mk(`onmouseover='ratethis("x")'`) +
		`</body></html>`

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page))
	})
	p := luaProvider(t, "animeheaven", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime.php?shapeid")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1 (all gate shapes parse, foreign handlers do not)", len(episodes))
	}
	if episodes[0].RawID != key {
		t.Errorf("RawID = %q, want %q", episodes[0].RawID, key)
	}
}

// ahGateFixtureSanity keeps the fixture honest: the captured gate page
// must still carry the four <source> elements the pick rule relies on.
func TestAnimeHeavenGateFixtureSanity(t *testing.T) {
	t.Parallel()

	doc := string(fixture(t, "animeheaven_gate.html"))
	if !strings.Contains(doc, "rk.animeheaven.me/video.mp4") {
		t.Fatal("gate fixture lost the primary rk source")
	}
	if strings.Count(doc, "<source") != 4 {
		t.Fatalf("gate fixture carries %d <source> elements, want 4", strings.Count(doc, "<source"))
	}
}
