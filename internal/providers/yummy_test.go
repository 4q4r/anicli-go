package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// yummyTestUA is the playback User-Agent wired into the test provider:
// the CVH (okcdn) resolve must echo the extraction UA on the video
// sources (anicli-api player/cdnvideohub.py: the CDN ties a playback
// session to the UA that fetched the links).
const yummyTestUA = "TestUA/1.0 (yummy provider test)"

// testYummy builds the provider against one httptest fixture server
// standing in for the api.yani.tv base, the ru.yummyani.me iframe host
// AND the plapi.cdnvideohub.com player API (every URL the provider
// contacts is server-relative).
func testYummy(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*Yummy, *recordedRequest) {
	t.Helper()
	srv, rec := fixtureServer(t, handle)
	return newYummy(YummySiteBase, srv.URL, srv.URL, yummyTestUA, testClient(t, "yummy")), rec
}

// yummyJSON writes a JSON envelope with the JSON content type.
func yummyJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// TestYummyMeta pins the service-level identity: registration identity,
// BOTH content semantics (RU voice-overs over present video), the RU
// content language and the declared smoke query. The provider
// deliberately does NOT declare NamePreference — the RU index is the
// default query routing.
func TestYummyMeta(t *testing.T) {
	t.Parallel()

	p := newYummy(YummySiteBase, "https://api.yani.tv", yummyCDNVideoHubBase, yummyTestUA, testClient(t, "yummy"))
	if p.ID() != "yummy" || p.Name() != "YummyAnime" {
		t.Errorf("identity = %q/%q, want yummy/YummyAnime", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://site.yummyani.me" {
		t.Errorf("BaseURL = %q, want the site root", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// Compile-time capability pins: declaring the smoke query fails the
// build if the implementation drops it; NOT declaring NamePreference is
// equally enforced — a latin-only declaration would be a visible type
// change.
var _ contracts.SmokeQueryProvider = (*Yummy)(nil)

// TestYummySmokeQuery pins the declared probe (PR68): the shared RU
// probe «черная лагуна» cannot reach «Пираты «Чёрной лагуны»» — the
// index matches single tokens, so it returns 20 unrelated «чёрная*»
// titles (verified live 2026-09-19). The substring probe «лагуна»
// surfaces the target at rank 1.
func TestYummySmokeQuery(t *testing.T) {
	t.Parallel()

	p := newYummy(YummySiteBase, "https://api.yani.tv", yummyCDNVideoHubBase, yummyTestUA, testClient(t, "yummy"))
	if got := p.SmokeQuery(); got != "лагуна" {
		t.Errorf("SmokeQuery = %q, want лагуна", got)
	}
}

// TestYummySearch pins the catalog search against the real captured
// response (testdata/yummy_search.json, GET api.yani.tv/anime with
// q=лагуна, captured live 2026-09-19): request path, query form,
// Accept header, and the result fields — the id-as-URL contract
// (animevost precedent), the absolutized protocol-relative poster.
func TestYummySearch(t *testing.T) {
	t.Parallel()

	p, rec := testYummy(t, func(w http.ResponseWriter, _ *http.Request) {
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

	p, _ := testYummy(t, func(w http.ResponseWriter, _ *http.Request) {
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

	p, _ := testYummy(t, func(w http.ResponseWriter, _ *http.Request) {
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
// URLs are absolutized.
func TestYummyGetEpisodes(t *testing.T) {
	t.Parallel()

	p, rec := testYummy(t, func(w http.ResponseWriter, r *http.Request) {
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
		if ep.RawID != ep.Num {
			t.Errorf("episodes[%d].RawID = %q, want %q", i, ep.RawID, ep.Num)
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

// itoa removed: strconv.Itoa covers the episode-order assertion.

// TestYummyGetEpisodesUnknownID pins the typed miss: a nonexistent id
// answers HTTP 200 {"response":[]} (live-verified) — surfaced as
// ErrNotFound instead of a silent empty list (animevost precedent:
// fail loud over the Python swallow).
func TestYummyGetEpisodesUnknownID(t *testing.T) {
	t.Parallel()

	p, _ := testYummy(t, func(w http.ResponseWriter, _ *http.Request) {
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
// iframe page → route-chunk JS → plapi playlist → plapi video. Every
// stage records its path/query so the tests pin the exact wire calls.
func yummyCVHWorld(t *testing.T) (*Yummy, string, *[]string) {
	t.Helper()

	var calls []string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		switch {
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
	p := newYummy(YummySiteBase, srv.URL, srv.URL, yummyTestUA, testClient(t, "yummy"))
	return p, srv.URL, &calls
}

// yummyCVHEpisode builds the resolve input: the same iframe URL shape
// the live /anime/1080/videos payload carries, host-swapped to the
// fixture server.
func yummyCVHEpisode(host string) contracts.Episode {
	return contracts.Episode{
		Num:   "1",
		RawID: "1",
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
// quality. The okcdn UA-echo header rides on every source.
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
		if got := src.Headers["User-Agent"]; got != yummyTestUA {
			t.Errorf("quality %s UA = %q, want the extraction UA %q (okcdn ties playback to it)", q, got, yummyTestUA)
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

	p, host, _ := yummyCVHWorld(t)

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

	var calls []string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch {
		case r.URL.Path == "/iframeCVH.html":
			_, _ = w.Write(fixture(t, "yummy_cvh_iframe.html"))
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			_, _ = w.Write([]byte("console.log('no constants here')"))
		default:
			t.Errorf("unexpected request past the JS stage: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	p := newYummy(YummySiteBase, srv.URL, srv.URL, yummyTestUA, testClient(t, "yummy"))

	_, err := p.ResolveStream(context.Background(), yummyCVHEpisode(srv.URL), "Озвучка MC Entertainment")
	if err == nil {
		t.Fatal("error = nil, want the shape failure")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("error = %v, want ErrExtractFailed", err)
	}
}

// TestYummyResolveStreamKodikRoundTrip pins the non-CVH delegation:
// links that are not iframeCVH iframes run through the shared
// extractor factory (the anilib kodik round-trip shape) — here the
// kodik branch, the workhorse dub player of the yummy catalog.
func TestYummyResolveStreamKodikRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ftor":
			_, _ = w.Write([]byte(`{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`))
		default:
			_, _ = w.Write([]byte(`<html><script>var hash = "h123"; var id = "456";</script></html>`))
		}
	}))
	t.Cleanup(srv.Close)

	p := newYummy(YummySiteBase, "https://api.yani.tv", yummyCDNVideoHubBase, yummyTestUA, testClient(t, "yummy"))
	episode := contracts.Episode{
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
