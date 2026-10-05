package providers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR123: the YummyAnime provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/yummy/main.lua) — the fourth Go→Lua
// migration. These tests pin the script through the same
// contracts.Provider surface and the same fixtures the compiled Go
// implementation was held to. The fresh-sandbox state contract adapts
// one pin: streams(raw_id, dub) receives only RawID, so raw_id carries
// the {a, n} state JSON and the resolve leg re-fetches /anime/{id}/videos
// (the animedia precedent).
//
// The harness rewrites BOTH production literals the script fetches —
// api.yani.tv and plapi.cdnvideohub.com — at the fixture server; the
// declared base_url stays site.yummyani.me (identity only, never
// fetched). yummyanime.in, the historical SSR fallback, is dead
// (HTTP 410, live-reverified 2026-10-05): the API surface is the only
// live one, exactly as the Go provider documented.

// yummyJSON writes a JSON envelope with the JSON content type.
func yummyJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:gosec // G705: test-only fixture writer; bodies are static
	// captures or the httptest server's own host echoed back.
	_, _ = w.Write(body)
}

// luaYummy loads the bundled yummy script against one fixture server
// standing in for both the api.yani.tv base and the CDNVideoHub
// player API base (the Go tests wired srv.URL into both too).
func luaYummy(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (contracts.Provider, *recordedRequest) {
	t.Helper()

	srv, rec := fixtureServer(t, handle)
	return luaProvider(t, "yummy", srv.URL), rec
}

// TestYummyMeta pins the service-level identity: registration identity,
// site base, source type and the RU content language.
func TestYummyMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "yummy")
	if p.ID() != "yummy" || p.Name() != "YummyAnime" {
		t.Errorf("identity = %q/%q, want yummy/YummyAnime", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://site.yummyani.me" {
		t.Errorf("BaseURL = %q, want https://site.yummyani.me", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if lc := p.(interface{ ContentLanguage() string }); lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", lc.ContentLanguage())
	}
}

// TestYummySmokeQuery pins the declared probe (PR68): the shared RU
// probe «черная лагуна» cannot reach «Пираты «Чёрной лагуны»» — the
// index matches single tokens and returns 20 unrelated «чёрная*»
// titles for it — while the substring «лагуна» surfaces the target at
// rank 1. The declared query gets no RU/latin fallback (the PR51
// declared-probe precedent).
func TestYummySmokeQuery(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "yummy")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("yummy must declare contracts.SmokeQueryProvider")
	}
	if got := sq.SmokeQuery(); got != "лагуна" {
		t.Errorf("SmokeQuery = %q, want лагуна", got)
	}
}

// TestYummySearch pins the catalog search against the real captured
// response (testdata/yummy_search.json, GET api.yani.tv/anime with
// q=лагуна, captured live 2026-09-19; the endpoint live-reverified
// 2026-10-05): request path, query form, Accept header, and the result
// fields — the id-as-URL contract (animevost precedent), the
// absolutized protocol-relative poster.
func TestYummySearch(t *testing.T) {
	t.Parallel()

	p, rec := luaYummy(t, func(w http.ResponseWriter, _ *http.Request) {
		yummyJSON(w, fixture(t, "yummy_search.json"))
	})

	results, err := p.Search(context.Background(), "лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/anime" {
		t.Errorf("request path = %q, want /anime", rec.Path)
	}
	if q := rec.Query; !containsAll(q, "q=%D0%BB%D0%B0%D0%B3%D1%83%D0%BD%D0%B0", "offset=0", "limit=20") {
		t.Errorf("query = %q, want the encoded RU query, offset=0 and limit=20", q)
	}
	if accept := rec.Header.Get("Accept"); accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}

	if len(results) != 20 {
		t.Fatalf("results = %d, want 20", len(results))
	}
	first := results[0]
	if first.Title != "Пираты «Чёрной лагуны»" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "1080" {
		t.Errorf("URL = %q, want the anime_id (GetEpisodes resolves it against /anime/{id}/videos)", first.URL)
	}
	if first.Poster != "https://static.yani.tv/posters/full/1595876271.jpg" {
		t.Errorf("Poster = %q, want the https-absolutized protocol-relative URL", first.Poster)
	}
	if first.SourceID != "yummy" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if results[1].Title != "Пираты «Чёрной лагуны»: Второй залп" || results[1].URL != "1081" {
		t.Errorf("second = %q/%q, want Второй залп/1081", results[1].Title, results[1].URL)
	}
}

// TestYummySearchMiss pins the empty outcome: the API answers a miss
// with HTTP 200 {"response":[]} (live-verified 2026-09-19) — zero
// results, no error (the caller renders the honest empty surface).
func TestYummySearchMiss(t *testing.T) {
	t.Parallel()

	p, _ := luaYummy(t, func(w http.ResponseWriter, _ *http.Request) {
		yummyJSON(w, fixture(t, "yummy_search_miss.json"))
	})

	results, err := p.Search(context.Background(), "zzzzqqqq")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

// TestYummySearchErrorEnvelope pins the defensive Err200 branch of the
// upstream API contract (yummy_anime_me_parser: HTTP 200 bodies may
// carry {"error": ...}). The envelope body is a real wire capture (the
// 400 body of /anime/abc/videos — the only live trigger found; served
// here at 200 to exercise the branch the matcher documents).
func TestYummySearchErrorEnvelope(t *testing.T) {
	t.Parallel()

	p, _ := luaYummy(t, func(w http.ResponseWriter, _ *http.Request) {
		yummyJSON(w, fixture(t, "yummy_error_envelope.json"))
	})

	_, err := p.Search(context.Background(), "лагуна")
	if err == nil {
		t.Fatal("error = nil, want the typed miss for the error envelope")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Arguments error") {
		t.Errorf("error = %v, want the server's message", err)
	}
}

// TestYummyGetEpisodes pins the episode listing against the real
// captured /anime/1080/videos response (testdata/yummy_videos.json,
// live 2026-09-19): 120 video rows arrive with MIXED zero-padded and
// bare numbers ("01".."12" and "1".."9" — the wire's own wart). The
// canonical int identity of upstream's ordinal=int(num) merges them
// into 12 episodes; dub names key RawEmbeds; protocol-relative iframe
// URLs are absolutized. RawID carries the {a,n} resolve-state JSON
// (the fresh-sandbox streams() contract).
func TestYummyGetEpisodes(t *testing.T) {
	t.Parallel()

	p, rec := luaYummy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anime/1080/videos" {
			t.Errorf("request path = %q, want /anime/1080/videos", r.URL.Path)
		}
		yummyJSON(w, fixture(t, "yummy_videos.json"))
	})

	episodes, err := p.GetEpisodes(context.Background(), "1080")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/anime/1080/videos" {
		t.Errorf("request path = %q, want /anime/1080/videos", rec.Path)
	}
	if accept := rec.Header.Get("Accept"); accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}

	// 21 raw number-strings ("01".."12", "1".."9") → 12 canonical
	// episodes in first-seen order.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12 (the zero-padded/bare duplicates merged)", len(episodes))
	}
	for i, ep := range episodes {
		if want := strconv.Itoa(i + 1); ep.Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, ep.Num, want)
		}
		if ep.Title != "" {
			t.Errorf("episodes[%d].Title = %q, want empty (the API carries no episode titles)", i, ep.Title)
		}
		if !strings.Contains(ep.RawID, `"n":"`+strconv.Itoa(i+1)+`"`) || !strings.Contains(ep.RawID, `"a":"1080"`) {
			t.Errorf("episodes[%d].RawID = %q, want the {a,n} state JSON", i, ep.RawID)
		}
	}

	ep1 := episodes[0]
	mc := ep1.RawEmbeds["Озвучка MC Entertainment"]
	// 6 links: the padded-«01» kodik serial embed first (its own wire
	// row), then the bare-«1» five-player block (kodik season, aksor,
	// alloha, sibnet, cvh) — the merge appends in wire order.
	if len(mc) != 6 {
		t.Fatalf("MC Entertainment links = %d, want 6 (2×kodik, aksor, alloha, sibnet, cvh)", len(mc))
	}
	if !strings.HasPrefix(mc[0], "https://kodikplayer.com/serial/11971/") {
		t.Errorf("link[0] = %q, want the absolutized kodik serial embed", mc[0])
	}
	if !strings.HasPrefix(mc[1], "https://kodikplayer.com/season/39771/") {
		t.Errorf("link[1] = %q, want the absolutized kodik season embed", mc[1])
	}
	if mc[2] != "https://player.aksor.tv/video/fccc776e8f4908a63a613f39ced88f27" {
		t.Errorf("link[2] = %q, want the aksor embed", mc[2])
	}
	if !strings.HasPrefix(mc[3], "https://alloha.yani.tv/") {
		t.Errorf("link[3] = %q, want the absolutized alloha embed", mc[3])
	}
	if mc[4] != "https://video.sibnet.ru/shell.php?videoid=1571325" {
		t.Errorf("link[4] = %q, want the sibnet embed", mc[4])
	}
	wantCVH := "https://ru.yummyani.me/iframeCVH.html?dubbing_code=MC+Entertaiment&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+MC+Entertaiment"
	if mc[5] != wantCVH {
		t.Errorf("link[5] = %q, want the absolutized CVH iframe", mc[5])
	}
	if subs := ep1.RawEmbeds["Субтитры"]; len(subs) != 2 {
		t.Errorf("Субтитры links = %d, want 2 (alloha + kodik)", len(subs))
	}
	wantDubs := []string{
		"Озвучка MC Entertainment", "Озвучка SHIZA Project",
		"Озвучка Silver AniAge", "Субтитры", "Озвучка LampStudio",
	}
	if len(ep1.RawEmbeds) != len(wantDubs) {
		t.Fatalf("episode 1 dubs = %d, want %d", len(ep1.RawEmbeds), len(wantDubs))
	}
	for _, dub := range wantDubs {
		if _, ok := ep1.RawEmbeds[dub]; !ok {
			t.Errorf("episode 1 missing dub %q", dub)
		}
	}
}

// TestYummyGetEpisodesUnknownID pins the typed miss: a nonexistent id
// answers HTTP 200 {"response":[]} (live-verified) — surfaced as
// ErrNotFound instead of a silent empty list (animevost precedent:
// fail loud over the Python swallow).
func TestYummyGetEpisodesUnknownID(t *testing.T) {
	t.Parallel()

	p, _ := luaYummy(t, func(w http.ResponseWriter, _ *http.Request) {
		yummyJSON(w, fixture(t, "yummy_search_miss.json"))
	})

	_, err := p.GetEpisodes(context.Background(), "999999999")
	if err == nil {
		t.Fatal("error = nil, want the typed miss")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// yummyCVHWorld serves the four-stage CVH chain from the real captures:
// iframe page → route-chunk JS → plapi playlist → plapi video, plus
// the /anime/889/videos state leg the fresh-sandbox resolve re-fetches
// (the Lua streams() contract: raw_id carries {a, n} and the listing
// is re-grouped; the host-swapped state payload keeps the chain
// hermetic). Every stage records its path/query so the tests pin the
// exact wire calls.
func yummyCVHWorld(t *testing.T) (contracts.Provider, string, *[]string) {
	t.Helper()
	return yummyCVHWorldState(t,
		"Озвучка MC Entertainment",
		"/iframeCVH.html?dubbing_code=MC+Entertaiment&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+MC+Entertaiment")
}

// yummyCVHWorldState is yummyCVHWorld with the state leg's dub and
// embed parametrized (the no-candidate test swaps in a studio the
// playlist has no video for).
func yummyCVHWorldState(t *testing.T, stateDub, stateIframe string) (contracts.Provider, string, *[]string) {
	t.Helper()

	var calls []string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		host := "http://" + r.Host
		switch {
		case r.URL.Path == "/anime/889/videos":
			yummyJSON(w, yummyCVHStateJSON(t, host, stateDub, stateIframe))
		case r.URL.Path == "/iframeCVH.html":
			_, _ = w.Write(fixture(t, "yummy_cvh_iframe.html"))
		case strings.HasPrefix(r.URL.Path, "/assets/src-routes-catalog-item-"):
			_, _ = w.Write(fixture(t, "yummy_cvh_js.txt"))
		case r.URL.Path == "/api/v1/player/sv/playlist":
			_, _ = w.Write(fixture(t, "yummy_cvh_playlist.json"))
		case strings.HasPrefix(r.URL.Path, "/api/v1/player/sv/video/"):
			_, _ = w.Write(fixture(t, "yummy_cvh_video.json"))
		default:
			t.Errorf("unexpected CVH-chain request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "yummy", srv.URL)
	return p, srv.URL, &calls
}

// yummyCVHStateJSON builds the one-row /anime/889/videos payload the
// resolve leg re-fetches: the dub's embed host-swapped to the fixture
// server (the live payload's shape, one triple).
func yummyCVHStateJSON(t *testing.T, host, dub, iframe string) []byte {
	t.Helper()

	payload := `{"response":[{"number":"1","data":{"dubbing":"` + dub + `"},"iframe_url":"` + host + iframe + `"}]}`
	return []byte(payload)
}

// yummyCVHEpisode builds the resolve input: the same iframe URL shape
// the live /anime/1080/videos payload carries, host-swapped to the
// fixture server. RawID carries the {a,n} state JSON the episode
// listing produced.
func yummyCVHEpisode(host string) contracts.Episode {
	return contracts.Episode{
		Num:   "1",
		RawID: `{"a":"889","n":"1"}`,
		RawEmbeds: map[string][]string{
			"Озвучка MC Entertainment": {
				host + "/iframeCVH.html?dubbing_code=MC+Entertaiment&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+MC+Entertaiment",
			},
		},
	}
}

// TestYummyResolveStreamCVH pins the four-stage CDNVideoHub chain
// (upstream yummy Source.get_videos special case, verified live
// 2026-09-19): the iframe page yields the module-script path, the JS
// chunk yields publisher 745 / aggregator "mali", the playlist is
// queried with pub/aggr/anime_id and filtered by episode+voiceStudio
// ("MC Entertaiment" — the + in the query decodes to a space), and the
// vkId fetch maps mpeg* qualities with hls/dash pinned to the max
// quality. The okcdn UA-echo header rides on every source — the
// config-default UA the netclient itself sends (the script pins the
// same constant; the animevost precedent).
func TestYummyResolveStreamCVH(t *testing.T) {
	t.Parallel()

	p, host, calls := yummyCVHWorld(t)

	stream, err := p.ResolveStream(context.Background(), yummyCVHEpisode(host), "Озвучка MC Entertainment")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "/iframeCVH.html?dubbing_code=MC+Entertaiment&anime_id=889&episode=1") {
		t.Errorf("calls miss the iframe fetch:\n%s", joined)
	}
	if !strings.Contains(joined, "/assets/src-routes-catalog-item-_uri_-_components-player-players-cvh-ZhP7BGdK.js") {
		t.Errorf("calls miss the JS chunk extracted from the iframe page:\n%s", joined)
	}
	if !strings.Contains(joined, "/api/v1/player/sv/playlist?aggr=mali&id=889&pub=745") &&
		!strings.Contains(joined, "/api/v1/player/sv/playlist?pub=745&aggr=mali&id=889") {
		t.Errorf("calls miss the playlist query pub=745 aggr=mali id=889:\n%s", joined)
	}
	if !strings.Contains(joined, "/api/v1/player/sv/video/9747961240146") {
		t.Errorf("calls miss the vkId video fetch:\n%s", joined)
	}

	// The capture carries mpegTiny..mpeg4k plus hls/dash; mpegQhdUrl is
	// empty and must not surface. The 4096 key carries the mpd (dash is
	// appended after hls — last write wins in the quality map, and the
	// map collapses upstream's list; documented divergence).
	wantTypes := map[string]string{
		"144":  "mp4",
		"240":  "mp4",
		"360":  "mp4",
		"480":  "mp4",
		"720":  "mp4",
		"1080": "mp4",
		"2048": "mp4",
		"4096": "mpd",
	}
	if len(stream.Links) != len(wantTypes) {
		t.Fatalf("links = %d entries (%v), want %d", len(stream.Links), stream.Links, len(wantTypes))
	}
	wantUA := config.Default().Network.UserAgent
	for q, wantType := range wantTypes {
		src, ok := stream.Links[q]
		if !ok {
			t.Errorf("missing quality %q", q)
			continue
		}
		if src.Type != wantType {
			t.Errorf("quality %s type = %q, want %q", q, src.Type, wantType)
		}
		if src.Quality != q {
			t.Errorf("quality %s label = %q", q, src.Quality)
		}
		if src.URL == "" {
			t.Errorf("quality %s carries an empty URL", q)
		}
		if got := src.Headers["User-Agent"]; got != wantUA {
			t.Errorf("quality %s UA = %q, want the extraction UA %q (okcdn ties playback to it)", q, got, wantUA)
		}
	}
	if strings.Contains(stream.Links["4096"].URL, "video.m3u8") {
		t.Errorf("4096 = the hls URL, want the dash URL appended after it")
	}
	if stream.DubName != "Озвучка MC Entertainment" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestYummyResolveStreamCVHNoCandidate pins the legitimate empty
// outcome: a studio listed in the source metadata with no video in the
// playlist yields an empty stream without an error (upstream
// _cdnvideohub_extract_vkid_cadidate returning None).
func TestYummyResolveStreamCVHNoCandidate(t *testing.T) {
	t.Parallel()

	p, host, _ := yummyCVHWorldState(t,
		"Озвучка Ghost Studio",
		"/iframeCVH.html?dubbing_code=Ghost+Studio&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+Ghost+Studio")

	episode := yummyCVHEpisode(host)
	episode.RawEmbeds = map[string][]string{
		"Озвучка Ghost Studio": {
			host + "/iframeCVH.html?dubbing_code=Ghost+Studio&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+Ghost+Studio",
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Озвучка Ghost Studio")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("links = %v, want empty", stream.Links)
	}
}

// TestYummyResolveStreamCVHShapeError pins the typed shape failure:
// a JS chunk without the publisher/aggregator constants fails the
// resolve with ErrExtractFailed, never a silent empty.
func TestYummyResolveStreamCVHShapeError(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		switch {
		case r.URL.Path == "/anime/889/videos":
			yummyJSON(w, yummyCVHStateJSON(t, host,
				"Озвучка MC Entertainment",
				"/iframeCVH.html?dubbing_code=MC+Entertaiment&anime_id=889&episode=1&dubbing=%D0%9E%D0%B7%D0%B2%D1%83%D1%87%D0%BA%D0%B0+MC+Entertaiment"))
		case r.URL.Path == "/iframeCVH.html":
			_, _ = w.Write(fixture(t, "yummy_cvh_iframe.html"))
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			_, _ = w.Write([]byte("console.log('no constants here')"))
		default:
			t.Errorf("unexpected request past the JS stage: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "yummy", srv.URL)

	_, err := p.ResolveStream(context.Background(), yummyCVHEpisode(srv.URL), "Озвучка MC Entertainment")
	if err == nil {
		t.Fatal("error = nil, want the shape failure")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("error = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "cdnvideohub") {
		t.Errorf("error = %v, want the cdnvideohub chain named", err)
	}
}

// TestYummyResolveStreamKodikRoundTrip pins the non-CVH delegation:
// links that are not iframeCVH iframes run through the shared
// extractor factory via anicli.extract (the anilib kodik round-trip
// shape) — here the kodik branch, the workhorse dub player of the
// yummy catalog.
func TestYummyResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		switch r.URL.Path {
		case "/anime/889/videos":
			yummyJSON(w, yummyCVHStateJSON(t, host,
				"Озвучка SHIZA Project", "/kodik/serial/54336/abc/720p"))
		case "/ftor":
			yummyJSON(w, []byte(`{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`))
		default:
			_, _ = w.Write([]byte(`<html><script>var hash = "h123"; var id = "456";</script></html>`))
		}
	})
	p := luaProvider(t, "yummy", srv.URL)
	episode := contracts.Episode{
		RawID: `{"a":"889","n":"1"}`,
		RawEmbeds: map[string][]string{
			"Озвучка SHIZA Project": {srv.URL + "/kodik/serial/54336/abc/720p"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Озвучка SHIZA Project")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if src, ok := stream.Links["720"]; !ok || src.URL != "https://plain.example/x/720.m3u8" {
		t.Errorf("720 = %+v, want the kodik extractor result", src)
	}
}
