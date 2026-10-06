package providers

// [LIVE-VERIFIED 2026-09-18] The DLE POST search is ALIVE: POST
// /index.php?do=search with the do/subaction/story form renders real
// shortstory results server-side (live: 2 cards for the fixture query,
// junk query → 0 cards, HTTP 200, Referer not required). The fixtures
// (sameband_search.html, sameband_anime.html, sameband_player.html,
// sameband_playlist.json) are byte-verbatim live captures; the player
// page's Playerjs bootstrap sits inside a Cloudflare Rocket Loader
// retyped script tag and the playlist URL carries RAW SPACES (fetched
// percent-encoded).
//
// PR131: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/sameband/main.lua) — these tests
// pin the script through the same contracts.Provider surface and the
// same fixtures the compiled Go implementation was held to. Contract
// shift forced by the fresh-sandbox Lua adapter (the animeheaven/
// anikoto precedent), documented here rather than hidden:
//
//   - the raw quality-prefixed file field rides episode RawID alone
//     (the only state channel into the per-invocation streams(raw_id,
//     dub) call — the Go provider read it back from RawEmbeds, which
//     the adapter does not pass into streams); RawEmbeds keeps
//     carrying the same file string for consumers.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestSameBandSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "sameband_search.html"))
	})
	p := luaProvider(t, "sameband", srv.URL)

	results, err := p.Search(context.Background(), "дьявол")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST (DLE search form, sameband.py:30)", rec.Method)
	}
	if rec.Path != "/index.php" || rec.Query != "do=search" {
		t.Errorf("request target = %q?%q, want /index.php?do=search", rec.Path, rec.Query)
	}
	if got := rec.Form["do"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("do form field = %q, want search", got)
	}
	if got := rec.Form["subaction"]; len(got) != 1 || got[0] != "search" {
		t.Errorf("subaction form field = %q, want search", got)
	}
	if got := rec.Form["story"]; len(got) != 1 || got[0] != "дьявол" {
		t.Errorf("story form field = %q, want the raw query", got)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form encoding", ct)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want the 2 captured cards", len(results))
	}
	if results[0].Title != "Дьявол Может Плакать" {
		t.Errorf("Title = %q, want the .poster[title] attribute", results[0].Title)
	}
	if results[1].URL != "https://sameband.studio/anime/122-djavol-mozhet-plakat-2.html" {
		t.Errorf("URL = %q, want the absolute href", results[1].URL)
	}
	// The poster src is always prefixed with the site root, even when
	// already absolute — sameband.py:44 quirk preserved.
	if results[0].Poster != srv.URL+"/v/posters/IMG_26210.webp" {
		t.Errorf("Poster = %q, want base-prefixed relative src", results[0].Poster)
	}
}

func TestSameBandSearchNoResultsIsEmpty(t *testing.T) {
	t.Parallel()

	// A junk query answers the same results shell with zero cards
	// (verified live 2026-09-18: story=лагуна → 0 cards, HTTP 200).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><body><div id="dle-content"></div></body></html>`)
	})
	p := luaProvider(t, "sameband", srv.URL)

	results, err := p.Search(context.Background(), "лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

// The netclient maps a 403 (WAF wall) onto the typed sentinel before
// the provider sees it; the SDK transport layer raises it under the
// anicli:provider_403: marker so the Lua adapter re-attaches the
// sentinel (consumer errors.Is branches hold, PR116).
func TestSameBandSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	p := luaProvider(t, "sameband", srv.URL)

	_, err := p.Search(context.Background(), "дьявол")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("err = %v, want ErrProvider403", err)
	}
}

// [LIVE-VERIFIED 2026-09-18] Full chain against the real capture paths:
// anime page → /v/play/Devil_May_Cry_S02.html player (its Playerjs
// bootstrap sits inside a Cloudflare Rocket Loader retyped script tag)
// → /v/list/Devil May Cry S02_list.txt playlist — the playlist URL
// carries RAW SPACES and must be fetched percent-encoded (the httptest
// route only matches when the client escaped them). The real playlist
// titles are HTML blobs (poster/duration markup); they are kept raw
// like the Python original (sameband.py:77).
func TestSameBandGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anime/122-djavol-mozhet-plakat-2.html":
			_, _ = w.Write(fixture(t, "sameband_anime.html"))
		case "/v/play/Devil_May_Cry_S02.html":
			_, _ = w.Write(fixture(t, "sameband_player.html"))
		case "/v/list/Devil May Cry S02_list.txt":
			_, _ = w.Write(fixture(t, "sameband_playlist.json"))
		default:
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "sameband", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(),
		srv.URL+"/anime/122-djavol-mozhet-plakat-2.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 9 {
		t.Fatalf("episodes = %d, want 8 captured + 1 modeled", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" {
		t.Errorf("Num = %q, want 1", first.Num)
	}
	// PR131 contract shift: the raw file field rides RawID alone (the
	// fresh-sandbox streams(raw_id, dub) state channel).
	wantTitle := "<img src='/v/anime/Devil May Cry S02/SnapShots/Devil May Cry S02 - 01_RUS_snapshot.jpg' class=playlist_poster><div class=playlist_duration>39:29</div>Серия 01"
	if first.Title != wantTitle {
		t.Errorf("Title = %q, want the raw playlist title (kept like Python)", first.Title)
	}
	wantFile := "[480p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_2/index.m3u8," +
		"[720p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_1/index.m3u8," +
		"[1080p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_0/index.m3u8"
	if first.RawID != wantFile {
		t.Errorf("RawID = %q, want the raw quality-prefixed file string (the streams state channel)", first.RawID)
	}
	raw := first.RawEmbeds["SameBand"]
	if len(raw) != 1 || raw[0] != wantFile {
		t.Errorf("RawEmbeds = %v, want the raw quality-prefixed file string", raw)
	}
	// Python item.get("title", f"Episode {i}") — the modeled trailing
	// entry has no title and falls back to the 1-based index.
	if episodes[8].Title != "Episode 9" || episodes[8].Num != "9" {
		t.Errorf("modeled entry = %q/%q, want the index fallback", episodes[8].Title, episodes[8].Num)
	}
}

// Divergence from Python (task ruling, PR47): a missing player iframe
// is a typed NotFound, not a silent empty result.
func TestSameBandGetEpisodesNoIframeTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no player here</body></html>")
	})
	p := luaProvider(t, "sameband", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/none")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if episodes != nil {
		t.Errorf("episodes = %v, want nil alongside the error", episodes)
	}
}

// Divergence from Python (task ruling, PR47): a player page without a
// Playerjs file field is a typed ExtractFailed, not a silent empty.
func TestSameBandGetEpisodesPlayerWithoutFileTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anime/x":
			_, _ = fmt.Fprint(w, `<div class="player"><div class="player-content"><iframe src="/player/9"></iframe></div></div>`)
		case "/player/9":
			_, _ = fmt.Fprint(w, `Playerjs({id:"player"})`)
		default:
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "sameband", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

// Divergence from Python (task ruling, PR47): a non-JSON playlist is a
// typed ExtractFailed — the Python bare except (sameband.py:68-71) hid
// exactly this breakage during the 2026-09 outage.
func TestSameBandGetEpisodesPlaylistDecodeTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anime/x":
			_, _ = fmt.Fprint(w, `<div class="player"><div class="player-content"><iframe src="/player/9"></iframe></div></div>`)
		case "/player/9":
			_, _ = fmt.Fprint(w, `Playerjs({file:"/playlist/9.txt"})`)
		case "/playlist/9.txt":
			_, _ = fmt.Fprint(w, "not-json")
		default:
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "sameband", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

// ResolveStream splits the raw file field on commas and maps each
// "[NNNp]<url>" part (port of sameband.py:83-96) — asserted against the
// real captured file string: three qualities, relative paths
// base-prefixed verbatim (raw spaces preserved like Python). No
// network: the file string rides RawID (PR131 contract shift), the
// resolve is pure string mapping like the Go original.
func TestSameBandResolveStream(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "sameband")
	file := "[480p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_2/index.m3u8," +
		"[720p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_1/index.m3u8," +
		"[1080p]/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_0/index.m3u8"
	episode := contracts.Episode{
		Num:   "1",
		RawID: file,
		RawEmbeds: map[string][]string{
			"SameBand": {file},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "SameBand")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 3 {
		t.Fatalf("Links = %v, want 480/720/1080", stream.Links)
	}
	src480, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links = %v, want a 480 entry", stream.Links)
	}
	if src480.URL != "https://sameband.studio/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_2/index.m3u8" {
		t.Errorf("URL = %q, want the base-prefixed relative path", src480.URL)
	}
	if src480.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8 (sameband.py:95)", src480.Type)
	}
	// Task ruling (PR5): direct site media carries the Referer for mpv;
	// the Python original left stream headers empty.
	if src480.Headers["Referer"] != "https://sameband.studio" {
		t.Errorf("Referer = %q, want the site root", src480.Headers["Referer"])
	}
	src1080 := stream.Links["1080"]
	if src1080.URL != "https://sameband.studio/v/anime/Devil May Cry S02/Devil May Cry S02 - 01_RUS_0/index.m3u8" {
		t.Errorf("1080 URL = %q", src1080.URL)
	}
	if stream.DubName != "SameBand" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestSameBandResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "sameband")
	stream, err := p.ResolveStream(context.Background(), contracts.Episode{
		RawID:     "[720p]/x.m3u8",
		RawEmbeds: map[string][]string{"SameBand": {"[720p]/x.m3u8"}},
	}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty (Python defaults the file to \"\")", stream.Links)
	}
}

func TestSameBandProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "sameband")
	if p.ID() != "sameband" || p.Name() != "SameBand" || p.BaseURL() != "https://sameband.studio" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}

// The live smoke query: the catalog is the studio's own dubs under
// server-side DLE matching, so the shared probes can never surface —
// the script declares its own live-verified hit («дьявол», 2 cards)
// and the adapter surfaces it as the SmokeQueryProvider capability.
func TestSameBandSmokeQuery(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "sameband")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatalf("SameBand does not declare SmokeQueryProvider")
	}
	if sq.SmokeQuery() != "дьявол" {
		t.Fatalf("SmokeQuery = %q, want the live-verified «дьявол» probe", sq.SmokeQuery())
	}
}
