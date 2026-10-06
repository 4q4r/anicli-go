package providers

// The hdrezka provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/hdrezka/main.lua) since the PR141
// Go→Lua migration — the roster's HYBRID: the script does the
// scraping, and the Anubis proof-of-work gate solves in pure Go
// through the anicli.solve_anubis SDK binding (internal/anubis, the
// solver the compiled provider owned since PR69). These tests pin the
// script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to (the 2026-09-19
// live captures; provenance in the git history of this file).
//
// The Anubis gate stays live [re-verified 2026-10-06 through the
// characterization proxy: the search leg answered the DLE listing
// while the anime page served a fresh challenge — algorithm "fast",
// difficulty 2, Anubis now at v1.27.0; the sha256 PoW contract is
// stable across the versions].
//
// Contract shifts forced by the fresh-sandbox Lua adapter, documented
// here rather than hidden (the kodik/anidub precedent):
//
//   - the per-(episode, dub) request payload rides episode RawID as
//     the JSON {[dub]: payload} map — the only state channel into the
//     per-invocation streams(raw_id, dub) call (the kodik {dub: url}
//     precedent); RawEmbeds keeps carrying the same payloads for
//     consumers;
//   - the mirror override rides anicli.provider_setting("base_url")
//     (the PR140 seam) with the serving-mirror literal as the
//     fallback; the literal pin is structural — every harness test
//     rewrites it onto the fixture server, and the flatten table is
//     pinned by TestProviderSettingsForFlattensConfig;
//   - typed walls keep their contracts sentinels but carry
//     ProviderError status 0 (the anicli.fail channel has no HTTP
//     status — the kodik precedent);
//   - the search card's span.info text trims (the sandbox html:text
//     contract trims every node text; the compiled provider trimmed
//     the link title but not the info line). Unobservable on the
//     fixtures — no captured card carries a span.info — and cosmetic
//     live (the composed title loses surrounding whitespace only).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fixtureNames used across the tests.
const (
	hdrezkaSearchFixture  = "hdrezka_search.html"
	hdrezkaSeriesFixture  = "hdrezka_anime_series.html"
	hdrezkaMovieFixture   = "hdrezka_anime_movie.html"
	hdrezkaNolinksFixture = "hdrezka_cdn_nolinks.json"
	hdrezkaSuccessFixture = "hdrezka_cdn_success.json"
	hdrezkaAnubisFixture  = "hdrezka_anubis_challenge.html"
	hdrezkaFakeFavs       = "00000000-1111-4222-8333-444444444444"
)

// decodeHDRezkaPayload decodes one raw payload JSON (the episode
// RawEmbeds entry) for the field pins below.
func decodeHDRezkaPayload(t *testing.T, raw string) map[string]string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode payload %q: %v", raw, err)
	}
	return payload
}

// hdrezkaAnubisChallengeRe extracts the challenge JSON from a capture
// (the local copy of the page-parse regex the compiled provider
// owned — the script re-derives it through anicli.regexp.match).
var hdrezkaAnubisChallengeRe = regexp.MustCompile(
	`(?s)<script id="anubis_challenge" type="application/json">(.*?)</script>`)

// decodeAnubisFixture extracts the challenge JSON from the capture.
func decodeAnubisFixture(t *testing.T, body []byte, v any) {
	t.Helper()
	m := hdrezkaAnubisChallengeRe.Find(body)
	if m == nil {
		t.Fatal("fixture does not carry an anubis_challenge script")
	}
	start := strings.Index(string(m), ">") + 1
	end := strings.LastIndex(string(m), "</script>")
	if err := json.Unmarshal(m[start:end], v); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
}

// hdrezkaTestPoW mirrors the server-side Anubis validation: the hex
// sha256 of challenge+nonce is the expected response.
func hdrezkaTestPoW(randomData, nonce string) string {
	sum := sha256.Sum256([]byte(randomData + nonce))
	return hex.EncodeToString(sum[:])
}

// TestHDRezkaSearch pins the SSR card parse [LIVE-VERIFIED]: the DLE
// search answers .b-content__inline_item cards, and ONLY cards whose
// data-url carries /animation/ are results (the site mixes films/series
// into the same listing).
func TestHDRezkaSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaSearchFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	results, err := p.Search(context.Background(), "наруто")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}
	if !strings.HasPrefix(rec.Query, "do=search&subaction=search&q=") {
		t.Errorf("request query = %q, want the DLE search params", rec.Query)
	}

	if len(results) != 6 {
		t.Fatalf("results = %d, want the 6 fixture cards", len(results))
	}
	first := results[0]
	if first.Title != "Наруто [OVA-8] / Пылающий Экзамен на Чуунина! Наруто против Конохамару! / Наруто против Конохомару. Экзамен на тюнина " {
		// The trailing space is reference behavior: title is composed as
		// title + " " + span.info, and these cards carry no span.info
		// (hdrezka.py: f"{data['title']} {data['season']}").
		t.Errorf("Title = %q, want the reference-composed card title (trailing space when span.info is absent)", first.Title)
	}
	if first.URL != "https://hdrezka-home.tv/animation/adventures/57085-naruto-ova-8-2011.html" &&
		first.URL != srv.URL+"/animation/adventures/57085-naruto-ova-8-2011.html" {
		t.Errorf("URL = %q, want the card link href", first.URL)
	}
	if first.SourceID != "hdrezka" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster == "" {
		t.Error("Poster = empty, want the card img src")
	}
}

// TestHDRezkaSearchFiltersNonAnimation pins the /animation/ filter: a
// card whose data-url lacks /animation/ (films, TV series) is NOT a
// result (hdrezka_parser.py: `"​/animation/" in i.get("data-url", "")`).
func TestHDRezkaSearchFiltersNonAnimation(t *testing.T) {
	t.Parallel()

	html := `<!doctype html><html><body>
<div class="b-content__inline_wrapper">
<div class="b-content__inline_item" data-url="https://hdrezka-home.tv/films/1234-some-film.html">
  <a href="https://hdrezka-home.tv/films/1234-some-film.html"><span>Фильм</span></a>
</div>
<div class="b-content__inline_item" data-url="https://hdrezka-home.tv/animation/adventures/1979-naruto.html">
  <div class="b-content__inline_item-link"><a href="https://hdrezka-home.tv/animation/adventures/1979-naruto.html">Наруто</a><span class="info">1 сезон, 293 серия</span></div>
  <img src="https://static.hdrezka.ac/i/x.jpg">
</div>
</div>
</body></html>`
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, html)
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want only the /animation/ card", len(results))
	}
	// Reference title composition: title + " " + span.info text.
	if results[0].Title != "Наруто 1 сезон, 293 серия" {
		t.Errorf("Title = %q, want title + space + span.info", results[0].Title)
	}
}

// TestHDRezkaSearchRussianQueryPercentEncoded pins the py_quote
// encoding (UTF-8 bytes uppercase %XX, spaces %20 — never the
// form-style +; the anidub script precedent, the SDK's query_escape
// is form-style).
func TestHDRezkaSearchRussianQueryPercentEncoded(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body></body></html>")
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	if _, err := p.Search(context.Background(), "наруто ру"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.HasPrefix(rec.Query, "do=search&subaction=search&q=%D0%BD%D0%B0%D1%80%D1%83%D1%82%D0%BE%20%D1%80%D1%83") {
		t.Errorf("request query = %q, want percent-encoded наруто ру", rec.Query)
	}
	if strings.Contains(rec.Query, "+") {
		t.Errorf("request query = %q, must not contain form-style +", rec.Query)
	}
}

// TestHDRezkaEpisodesSeries pins the series page parse [LIVE-VERIFIED]:
// translators → dub keys, SSR episode items → episodes, per-episode
// payloads carrying the get_stream form fields.
func TestHDRezkaEpisodesSeries(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaSeriesFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	animeURL := srv.URL + "/animation/adventures/1979-naruto-uragannye-hroniki-2007.html"
	episodes, err := p.GetEpisodes(context.Background(), animeURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if rec.Path != "/animation/adventures/1979-naruto-uragannye-hroniki-2007.html" {
		t.Errorf("request path = %q, want the anime page", rec.Path)
	}
	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want the 4 fixture items (1,2,3,500)", len(episodes))
	}

	first := episodes[0]
	if first.Num != "1" {
		t.Errorf("first Num = %q, want data-episode_id 1", first.Num)
	}
	if first.Title != "Серия 1" {
		t.Errorf("first Title = %q, want the item text", first.Title)
	}

	// Dub keys = translator title attributes, document order, verbatim
	// (the reference does NOT strip them — the live UA-translator title
	// carries a trailing space).
	wantDubs := []string{"2x2", "AniDUB", "Украинский многоголосый "}
	if len(first.RawEmbeds) != len(wantDubs) {
		t.Fatalf("RawEmbeds = %d dubs, want %d", len(first.RawEmbeds), len(wantDubs))
	}
	for i, want := range wantDubs {
		got := first.RawEmbeds[want]
		if got == nil {
			t.Fatalf("RawEmbeds missing dub %q", want)
		}
		if i == 0 {
			// 2x2 is the page-active translator; its payload carries the
			// get_stream form fields of the first episode.
			payload := decodeHDRezkaPayload(t, got[0])
			if payload["id"] != "1979" || payload["translator_id"] != "14" {
				t.Errorf("payload id/translator = %q/%q, want 1979/14", payload["id"], payload["translator_id"])
			}
			if payload["season"] != "1" || payload["episode"] != "1" {
				t.Errorf("payload season/episode = %q/%q, want 1/1", payload["season"], payload["episode"])
			}
			if payload["favs"] != hdrezkaFakeFavs {
				t.Errorf("payload favs = %q, want the page ctrl_favs", payload["favs"])
			}
			if payload["action"] != "get_stream" {
				t.Errorf("payload action = %q, want get_stream", payload["action"])
			}
			if payload["page_url"] != animeURL {
				t.Errorf("payload page_url = %q, want the anime page (Referer source)", payload["page_url"])
			}
		}
	}

	last := episodes[3]
	if last.Num != "500" {
		t.Errorf("last Num = %q, want data-episode_id 500", last.Num)
	}
	if last.RawEmbeds["AniDUB"] == nil {
		t.Error("last episode missing the AniDUB payload")
	}
	lastPayload := decodeHDRezkaPayload(t, last.RawEmbeds["AniDUB"][0])
	if lastPayload["episode"] != "500" || lastPayload["translator_id"] != "18" {
		t.Errorf("last payload episode/translator = %q/%q, want 500/18", lastPayload["episode"], lastPayload["translator_id"])
	}

	// The raw_id state channel carries the same per-dub payload map
	// (the fresh-sandbox streams(raw_id, dub) input — the kodik
	// precedent).
	var state map[string]string
	if err := json.Unmarshal([]byte(first.RawID), &state); err != nil {
		t.Fatalf("raw_id is not the {[dub]: payload} map: %v", err)
	}
	if state["2x2"] != first.RawEmbeds["2x2"][0] {
		t.Error("raw_id map diverges from the RawEmbeds payload for dub 2x2")
	}
}

// TestHDRezkaEpisodesMovie pins the movie flow [LIVE-VERIFIED]: no SSR
// episode items → ONE synthetic episode; payloads carry the get_movie
// form fields (no season/episode).
func TestHDRezkaEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaMovieFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/animation/adventures/2459-naruto-film-pervyy-2004.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want the single synthetic movie episode", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want 1", ep.Num)
	}
	if !strings.Contains(ep.Title, "Наруто (фильм первый)") {
		t.Errorf("Title = %q, want the page h1", ep.Title)
	}
	payload := decodeHDRezkaPayload(t, ep.RawEmbeds["Дубляж (неофициальный)"][0])
	if payload["id"] != "2459" || payload["translator_id"] != "489" {
		t.Errorf("payload id/translator = %q/%q, want 2459/489", payload["id"], payload["translator_id"])
	}
	if payload["action"] != "get_movie" {
		t.Errorf("payload action = %q, want get_movie", payload["action"])
	}
	if payload["season"] != "" || payload["episode"] != "" {
		t.Errorf("movie payload must not carry season/episode, got %q/%q", payload["season"], payload["episode"])
	}
	if payload["favs"] != hdrezkaFakeFavs {
		t.Errorf("payload favs = %q, want the page ctrl_favs", payload["favs"])
	}
}

// TestHDRezkaResolveStream pins the get_stream round-trip against the
// RECONSTRUCTED success fixture: quality parsing ("[1080p Ultra]…"),
// " or " alternates (first per quality wins the map key), mp4 vs m3u8
// by extension, Referer headers, and the exact POST wire shape
// [LIVE-VERIFIED against the site player JS].
func TestHDRezkaResolveStream(t *testing.T) {
	t.Parallel()

	var postSeen atomic.Bool
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ajax/get_cdn_series/" {
			postSeen.Store(true)
			// Reference: timestamp in SECONDS minus 40 (hdrezka.py:
			// ts = int(time() - 40)).
			if got := r.FormValue("action"); got != "get_stream" {
				t.Errorf("form action = %q, want get_stream", got)
			}
			if got := r.FormValue("id"); got != "1979" {
				t.Errorf("form id = %q, want 1979", got)
			}
			if got := r.FormValue("translator_id"); got != "14" {
				t.Errorf("form translator_id = %q, want 14", got)
			}
			if got := r.FormValue("season"); got != "1" || r.FormValue("episode") != "1" {
				t.Errorf("form season/episode = %q/%q, want 1/1", r.FormValue("season"), r.FormValue("episode"))
			}
			if got := r.FormValue("favs"); got != hdrezkaFakeFavs {
				t.Errorf("form favs = %q, want the page token", got)
			}
			if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				t.Error("missing X-Requested-With: XMLHttpRequest")
			}
			if r.Header.Get("Origin") == "" {
				t.Error("missing Origin")
			}
			if r.Header.Get("Referer") == "" {
				t.Error("missing Referer (the page_url from the payload)")
			}
			if ts := r.URL.Query().Get("t"); ts == "" {
				t.Error("missing ?t= cache-buster")
			}
			_, _ = w.Write(fixture(t, hdrezkaSuccessFixture))
			return
		}
		_, _ = w.Write(fixture(t, hdrezkaSeriesFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/animation/adventures/1979-naruto-uragannye-hroniki-2007.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), episodes[0], "2x2")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "2x2" {
		t.Errorf("DubName = %q, want 2x2", stream.DubName)
	}
	if rec.Method != "POST" || !postSeen.Load() {
		t.Errorf("last request = %s %s, want the CDN POST", rec.Method, rec.Path)
	}
	if rec.Path != "/ajax/get_cdn_series/" {
		t.Errorf("POST path = %q, want /ajax/get_cdn_series/", rec.Path)
	}

	// [480p] mp4 or m3u8 → mp4 wins the "480" key (first per quality);
	// [720p] m3u8; [1080p Ultra] m3u8.
	want := map[string]struct {
		url string
		typ string
		ref bool
	}{
		"480":  {"https://cdn.example.invalid/rezka/1979/s1e1-480.mp4", "mp4", true},
		"720":  {"https://cdn.example.invalid/rezka/1979/s1e1-720.m3u8", "m3u8", true},
		"1080": {"https://cdn.example.invalid/rezka/1979/s1e1-1080.m3u8", "m3u8", true},
	}
	if len(stream.Links) != len(want) {
		t.Fatalf("Links = %d entries, want %d", len(stream.Links), len(want))
	}
	for q, w := range want {
		got, ok := stream.Links[q]
		if !ok {
			t.Errorf("Links missing quality %q", q)
			continue
		}
		if got.URL != w.url {
			t.Errorf("Links[%q].URL = %q, want %q", q, got.URL, w.url)
		}
		if got.Type != w.typ {
			t.Errorf("Links[%q].Type = %q, want %q", q, got.Type, w.typ)
		}
		if got.Quality != q {
			t.Errorf("Links[%q].Quality = %q", q, got.Quality)
		}
		if w.ref && got.Headers["Referer"] == "" {
			t.Errorf("Links[%q] missing the Referer header", q)
		}
	}
}

// TestHDRezkaResolveStreamMovie pins the get_movie action on a movie
// payload (no season/episode fields on the wire).
func TestHDRezkaResolveStreamMovie(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ajax/get_cdn_series/" {
			if got := r.FormValue("action"); got != "get_movie" {
				t.Errorf("form action = %q, want get_movie", got)
			}
			if r.FormValue("season") != "" || r.FormValue("episode") != "" {
				t.Errorf("movie POST must not send season/episode, got %q/%q",
					r.FormValue("season"), r.FormValue("episode"))
			}
			_, _ = w.Write(fixture(t, hdrezkaSuccessFixture))
			return
		}
		_, _ = w.Write(fixture(t, hdrezkaMovieFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/animation/adventures/2459-naruto-film-pervyy-2004.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if _, err := p.ResolveStream(context.Background(), episodes[0], "Дубляж (неофициальный)"); err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if rec.Method != "POST" {
		t.Errorf("last request method = %q, want POST", rec.Method)
	}
}

// TestHDRezkaResolveStreamNoLinksGeoFenced pins the honest typed
// failure on the REAL url:false response [LIVE-VERIFIED 2026-09-19]:
// the site answers success:true but withholds stream links from
// stream-refusing exits (geo/premium decision). The upstream reference
// CRASHES on this shape (bool.split) — the port fails loud with
// ErrGeoBlocked instead.
func TestHDRezkaResolveStreamNoLinksGeoFenced(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ajax/get_cdn_series/" {
			_, _ = w.Write(fixture(t, hdrezkaNolinksFixture))
			return
		}
		_, _ = w.Write(fixture(t, hdrezkaSeriesFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/animation/adventures/1979-naruto-uragannye-hroniki-2007.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	_, err = p.ResolveStream(context.Background(), episodes[0], "2x2")
	if err == nil {
		t.Fatal("ResolveStream must fail on the url:false response")
	}
	if !errors.Is(err, contracts.ErrGeoBlocked) {
		t.Errorf("err = %v, want ErrGeoBlocked", err)
	}
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) || pe.Provider != "hdrezka" || pe.Op != contracts.OpResolveStream {
		t.Errorf("err = %v, want a hdrezka resolve_stream ProviderError", err)
	}
}

// TestHDRezkaResolveStreamSuccessFalse pins the success:false error
// branch (the server's explicit refusal carries a message).
func TestHDRezkaResolveStreamSuccessFalse(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ajax/get_cdn_series/" {
			_, _ = fmt.Fprint(w, `{"success":false,"message":"Переводчик недоступен"}`)
			return
		}
		_, _ = w.Write(fixture(t, hdrezkaSeriesFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/animation/adventures/1979-naruto-uragannye-hroniki-2007.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	_, err = p.ResolveStream(context.Background(), episodes[0], "2x2")
	if err == nil {
		t.Fatal("ResolveStream must fail on success:false")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "Переводчик недоступен") {
		t.Errorf("err = %v, want the server message inline", err)
	}
}

// TestHDRezkaResolveStreamMissingDub pins the missing/short payload
// guard: an unknown dub (and an empty raw_id state) fail loud
// (ErrNotFound), never a zero-value stream.
func TestHDRezkaResolveStreamMissingDub(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaSeriesFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	ep := contracts.Episode{Num: "1", RawID: "", RawEmbeds: map[string][]string{"2x2": {}}}
	if _, err := p.ResolveStream(context.Background(), ep, "2x2"); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for an empty payload state", err)
	}
	if _, err := p.ResolveStream(context.Background(), ep, "несуществующая озвучка"); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for an unknown dub", err)
	}
}

// TestHDRezkaAnubisGateSolvesChallengeAndRetries pins the Anubis
// ladder end-to-end on the REAL challenge fixture — the HYBRID proof:
// the Lua script detects the challenge, solves it through the
// anicli.solve_anubis Go binding (sha256, difficulty leading zero hex
// digits), calls pass-challenge with the exact parameter set, and
// RETRIES the original request — which then succeeds with the auth
// cookie in the wired netclient's jar.
func TestHDRezkaAnubisGateSolvesChallengeAndRetries(t *testing.T) {
	t.Parallel()

	challenge := fixture(t, hdrezkaAnubisFixture)
	var passCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.within.website/x/cmd/anubis/api/pass-challenge":
			passCalls.Add(1)
			q := r.URL.Query()
			if q.Get("id") == "" || q.Get("response") == "" || q.Get("nonce") == "" {
				t.Errorf("pass-challenge params incomplete: %v", q)
			}
			if q.Get("redir") == "" {
				t.Error("pass-challenge missing redir")
			}
			if q.Get("elapsedTime") == "" {
				t.Error("pass-challenge missing elapsedTime (the honest solve duration)")
			}
			// Validate the PoW exactly like Anubis
			// (lib/challenge/proofofwork): sha256(randomData+nonce) hex
			// must equal response and carry difficulty leading zeros.
			nonce := q.Get("nonce")
			response := q.Get("response")
			var challengeBody struct {
				Challenge struct {
					ID         string `json:"id"`
					RandomData string `json:"randomData"`
				} `json:"challenge"`
				Rules struct {
					Difficulty int `json:"difficulty"`
				} `json:"rules"`
			}
			decodeAnubisFixture(t, challenge, &challengeBody)
			want := hdrezkaTestPoW(challengeBody.Challenge.RandomData, nonce)
			if want != response {
				t.Errorf("response = %q, want sha256(%q+%s) = %q", response,
					challengeBody.Challenge.RandomData, nonce, want)
			}
			if !strings.HasPrefix(response, strings.Repeat("0", challengeBody.Rules.Difficulty)) {
				t.Errorf("response %q lacks %d leading zeros", response, challengeBody.Rules.Difficulty)
			}
			http.SetCookie(w, &http.Cookie{
				Name:     "techaro.lol-anubis-auth",
				Value:    "test-auth-cookie",
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			_, _ = fmt.Fprint(w, "<html><body>passed</body></html>")
		default:
			// Before the auth cookie: challenge page. After: the real
			// content (cookie jar flows through netclient).
			passed := false
			for _, c := range r.Cookies() {
				if c.Name == "techaro.lol-anubis-auth" {
					passed = true
				}
			}
			if passed {
				_, _ = w.Write(fixture(t, hdrezkaSearchFixture))
				return
			}
			_, _ = w.Write(challenge)
		}
	}))
	defer srv.Close()

	p := luaProvider(t, "hdrezka", srv.URL)
	results, err := p.Search(context.Background(), "наруто")
	if err != nil {
		t.Fatalf("Search through the gate: %v", err)
	}
	if passCalls.Load() != 1 {
		t.Errorf("pass-challenge called %d times, want exactly 1", passCalls.Load())
	}
	if len(results) != 6 {
		t.Errorf("results = %d, want the fixture cards after the gate", len(results))
	}
}

// TestHDRezkaAnubisGateFailureTyped pins the bounded-solver contract:
// an unsupported-algorithm challenge fails LOUD with a typed 403-class
// error (the SDK binding's anicli:provider_403: marker) instead of
// looping.
func TestHDRezkaAnubisGateFailureTyped(t *testing.T) {
	t.Parallel()

	html := `<!doctype html><html><head>` +
		`<script id="anubis_version" type="application/json">"1.25.0"</script>` +
		`<script id="anubis_challenge" type="application/json">{"rules":{"algorithm":"keccak-ridiculous","difficulty":2},` +
		`"challenge":{"id":"x","method":"keccak-ridiculous","randomData":"aa","issuedAt":"now","spent":false}}</script>` +
		`</head><body></body></html>`
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, html)
	})
	p := luaProvider(t, "hdrezka", srv.URL)

	_, err := p.Search(context.Background(), "naruto")
	if err == nil {
		t.Fatal("Search must fail on an unsupported Anubis algorithm")
	}
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Errorf("err = %v, want ErrProvider403 (anti-bot gate refusal)", err)
	}
}

// TestHDRezkaOffline pins the transport-failure path (dead listener):
// the error surfaces, nothing panics, nothing is swallowed.
func TestHDRezkaOffline(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	deadURL := "http://" + dead.Addr().String()
	p := luaProvider(t, "hdrezka", deadURL)

	if _, err := p.Search(context.Background(), "naruto"); err == nil {
		t.Error("Search on a dead endpoint must fail")
	}
	if _, err := p.GetEpisodes(context.Background(), deadURL+"/animation/x.html"); err == nil {
		t.Error("GetEpisodes on a dead endpoint must fail")
	}
}

// TestHDRezkaNamePreferenceRUGroup pins the search routing (PR42
// semantics): hdrezka indexes RU names, so it must NOT declare the
// latin preference — the RU group routes it Cyrillic-first variants
// via its content language.
func TestHDRezkaNamePreferenceRUGroup(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaSearchFixture))
	})
	p := luaProvider(t, "hdrezka", srv.URL)
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the hdrezka provider lost the ContentLanguage surface")
	}
	if got := lc.ContentLanguage(); got != "ru" {
		t.Errorf("ContentLanguage = %q, want ru (the script's declaration)", got)
	}
	// The Lua adapter's capability composite implements the optional
	// interfaces for every script (caps.go) — the observationally
	// honest check is the VALUE (the registry's duck check), not the
	// type assertion the compiled provider admitted.
	if np, ok := p.(contracts.NamePreferenceProvider); ok {
		if got := np.NamePreference(); got != contracts.NamePrefDefault {
			t.Errorf("NamePreference = %v, want the default (hdrezka must stay in the RU group — no latin preference)", got)
		}
	}
	if sq, ok := p.(contracts.SmokeQueryProvider); ok {
		if got := sq.SmokeQuery(); got != "" {
			t.Errorf("SmokeQuery = %q, want empty (hdrezka answers the shared smoke probes)", got)
		}
	}
}

// TestHDRezkaBaseURLSettingOverride pins the PR140 mirror seam end to
// end: a configured providers.hdrezka.base_url re-points every leg
// (the mirror-rotation contract), the unconfigured path keeps the
// serving-mirror literal the harness rewrote.
func TestHDRezkaBaseURLSettingOverride(t *testing.T) {
	t.Parallel()

	override, overrideRec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, hdrezkaSearchFixture))
	})
	p := luaProviderWithSettings(t, "hdrezka", "https://rezka-ua.tv", map[string]string{"base_url": override.URL})

	if got := p.BaseURL(); got != override.URL {
		t.Errorf("BaseURL = %q, want the configured override", got)
	}
	if _, err := p.Search(context.Background(), "наруто"); err != nil {
		t.Fatalf("Search through the override mirror: %v", err)
	}
	if overrideRec.Path != "/search/" {
		t.Errorf("the search landed on %q, want the override mirror's /search/", overrideRec.Path)
	}
}
