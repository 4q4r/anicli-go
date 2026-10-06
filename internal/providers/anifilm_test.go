package providers

// The fixtures below are REAL captures of anifilm.pro (2026-09-23,
// plain curl with a browser UA — the site fronts no anti-bot wall):
//
//	anifilm_search.html       GET /releases?title=дьявол (4 cards)
//	anifilm_search_miss.html  GET /releases?title=<junk> (0 cards)
//	anifilm_release.html      GET /releases/1200-devil-may-cry
//	                          (player-component + torrent list + one
//	                          credited voice, «Боллектив Media»)
//	anifilm_online_kodik.json GET /releases/api:online:1200:kodik
//	                          (12 episodes, single-translation embeds)
//	anifilm_video.html        GET /releases/api:video:16231 (HTML
//	                          wrapping the kodikplayer.com iframe)
//
// The one synthetic payload in this file is the service-fallback
// player-component attribute in TestAniFilmEpisodesServiceFallback —
// no kodik-inactive release was found live (the catalog is
// kodik-dominant); its SHAPE mirrors the real attributes verbatim.
//
// PR135: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/anifilm/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shift
// forced by the fresh-sandbox Lua adapter (the sameband/animego
// precedent), documented here rather than hidden:
//
//   - streams(raw_id, dub) receives no dub set, so the Go provider's
//     unknown-dub caller-bug wall (ErrInvalidInput) has no surface —
//     the resolve re-derives everything from the playlist row id that
//     rides RawID; RawEmbeds keeps carrying the api:video URL for
//     consumers.
//   - the attribute parser (parseServicesProps/servableServices) was a
//     Go-internal unit; its observable behavior — kodik-first active
//     service preference, the trailer exclusion — stays pinned here
//     through TestAniFilmEpisodesServiceFallback and
//     TestAniFilmEpisodesTrailerNeverServed.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestAniFilmProviderMeta pins the service-level identity: registration
// identity, BOTH content semantics (RU voice-overs over present
// video), the RU content language, and the declared smoke probe — the
// shared «черная лагуна» probe lands 0 cards live (Black Lagoon is not
// on the catalog, verified 2026-09-23), so the script declares its own
// live-verified probe.
func TestAniFilmProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "anifilm")
	if p.ID() != "anifilm" || p.Name() != "AniFilm" {
		t.Errorf("identity = %q/%q, want anifilm/AniFilm", p.ID(), p.Name())
	}
	if p.BaseURL() != "https://anifilm.pro" {
		t.Errorf("BaseURL = %q, want the site root", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	// ContentLanguage is a duck-typed capability (the registry's
	// ContentLanguage surface), never part of contracts.Provider.
	cl, ok := p.(interface{ ContentLanguage() string })
	if !ok || cl.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage declared=%v, want the ru declaration", ok)
	}
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatalf("SmokeQueryProvider not declared: the shared RU probe lands 0 cards on this catalog")
	}
	if got := sq.SmokeQuery(); got != "дьявол" {
		t.Errorf("SmokeQuery = %q, want дьявол (the live-verified probe)", got)
	}
}

// TestAniFilmSearch pins the catalog search against the real captured
// response page (testdata/anifilm_search.html, GET
// /releases?title=дьявол, 2026-09-23): four rendered .releases__item
// cards, absolute URLs and posters, and the ride on the site's own GET
// form field (title=).
func TestAniFilmSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anifilm_search.html"))
	})
	p := luaProvider(t, "anifilm", srv.URL)

	results, err := p.Search(context.Background(), "дьявол")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4 (fixture anifilm_search.html)", len(results))
	}

	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET (the site's own header form is method=get)", rec.Method)
	}
	if !strings.Contains(rec.Query, "title=") {
		t.Errorf("search rode %q, want a title= query param (the site's own GET form field)", rec.Query)
	}

	first := results[0]
	if first.Title != "И дьявол может плакать" {
		t.Errorf("Title = %q, want the live card title", first.Title)
	}
	if !strings.HasSuffix(first.URL, "/releases/1200-devil-may-cry") {
		t.Errorf("URL = %q, want the release path", first.URL)
	}
	if !strings.HasPrefix(first.URL, "http://") {
		t.Errorf("URL = %q, want absolutized against the test server", first.URL)
	}
	if first.SourceID != "anifilm" {
		t.Errorf("SourceID = %q, want anifilm", first.SourceID)
	}
	if !strings.Contains(first.Poster, "/static/upload/releases/poster/thumb/1200-devil-may-cry-") {
		t.Errorf("Poster = %q, want the thumb path", first.Poster)
	}

	// Every surfaced URL must be absolute: the TUI feeds it straight
	// into GetEpisodes.
	for i, r := range results {
		if !strings.HasPrefix(r.URL, "http://") {
			t.Errorf("results[%d].URL = %q, want absolute", i, r.URL)
		}
	}
}

// TestAniFilmSearchMiss pins the no-results shape against the real
// captured junk-query page (testdata/anifilm_search_miss.html): HTTP
// 200 with zero .releases__item cards — an empty result, never an
// error.
func TestAniFilmSearchMiss(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anifilm_search_miss.html"))
	})
	p := luaProvider(t, "anifilm", srv.URL)

	results, err := p.Search(context.Background(), "несуществующий тайтл 12345")
	if err != nil {
		t.Fatalf("Search miss returned an error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

// TestAniFilmEpisodes pins the full episode reconstruction against the
// real release page + playlist capture (Devil May Cry, 2026-09-23):
// the player-component's releases_id drives the api:online playlist
// fetch, the single credited voice names the dub, and every episode's
// embed is the api:video URL of its playlist row (the site's own
// iframe source — the raw playlist iframe host is a kodik mirror the
// extractor gate does not cover).
func TestAniFilmEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/1200-devil-may-cry":
			_, _ = w.Write(fixture(t, "anifilm_release.html"))
		case "/releases/api:online:1200:kodik":
			_, _ = w.Write(fixture(t, "anifilm_online_kodik.json"))
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "anifilm", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1200-devil-may-cry")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12 (the live playlist row count)", len(episodes))
	}
	if got := rec.Path; got != "/releases/api:online:1200:kodik" {
		t.Errorf("playlist fetch = %q, want the releases_id from the player-component attribute", got)
	}

	first := episodes[0]
	if first.Num != "1" || first.Title != "Серия 1" {
		t.Errorf("first episode = %q/%q, want 1/Серия 1 (verbatim playlist fields)", first.Num, first.Title)
	}
	if first.RawID != "16231" {
		t.Errorf("RawID = %q, want the live playlist row id 16231", first.RawID)
	}
	embeds := first.RawEmbeds["Боллектив Media"]
	if len(embeds) != 1 {
		t.Fatalf("embeds = %v, want one under the credited voice", first.RawEmbeds)
	}
	if !strings.HasSuffix(embeds[0], "/releases/api:video:16231") {
		t.Errorf("embed = %q, want the api:video URL of the playlist row", embeds[0])
	}
	if !strings.HasPrefix(embeds[0], "http://") {
		t.Errorf("embed = %q, want absolutized", embeds[0])
	}

	// Numeric ascending order: the live playlist is ascending already,
	// the sort must not disturb it.
	if episodes[11].Num != "12" || episodes[11].RawID != "16249" {
		t.Errorf("last episode = %q/%q, want 12/16249 (live row order)", episodes[11].Num, episodes[11].RawID)
	}
}

// TestAniFilmEpisodes404 pins the deleted-but-indexed release wall:
// the search index still lists releases whose pages answer 404
// (observed live 2026-09-23 on the 1203/1204/1202 RSS entries). The
// typed mapping must surface, never an empty list.
func TestAniFilmEpisodes404(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	p := luaProvider(t, "anifilm", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1203-zetsuen-no-tempest-the-civilization-blaster")
	if err == nil {
		t.Fatal("GetEpisodes on a 404 page must fail loud")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (the transport 404 mapping)", err)
	}
}

// TestAniFilmEpisodesNoPlayer pins the walled-title shape: a release
// page without a player-component (or without a parsable releases_id)
// carries no anonymous episodes — typed wall, not an empty list.
func TestAniFilmEpisodesNoPlayer(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body><h1>release page without a player</h1></body></html>"))
	})
	p := luaProvider(t, "anifilm", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1200-devil-may-cry")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

// TestAniFilmEpisodesServiceFallback pins the service preference: the
// kodik playlist is tried first, and a release serving a different
// active service (sibnet here) falls back to it. The payload is
// SYNTHETIC (no live kodik-inactive release found — see the file
// header); the attribute shape mirrors the real player-component
// verbatim.
func TestAniFilmEpisodesServiceFallback(t *testing.T) {
	t.Parallel()

	page := strings.ReplaceAll(
		string(fixture(t, "anifilm_release.html")),
		`:services_props={"kodik":{"from":"kodik","active":true},"rutube":{"from":"rutube","active":false},"trailer":{"from":"trailer","active":false}}`,
		`:services_props={"kodik":{"from":"kodik","active":false},"sibnet":{"from":"sibnet","active":true}}`,
	)

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/1200-devil-may-cry":
			_, _ = w.Write([]byte(page))
		case "/releases/api:online:1200:sibnet":
			_, _ = w.Write([]byte(`[{"id":"99001","rid":"1200","episode":"1","title":"Серия 1","iframe":"https://video.sibnet.ru/shell.php?vid=1","from":"sibnet"}]`))
		default:
			t.Errorf("unexpected fetch: %s (inactive kodik must not be polled)", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "anifilm", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1200-devil-may-cry")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if got := rec.Path; got != "/releases/api:online:1200:sibnet" {
		t.Errorf("playlist fetch = %q, want the active sibnet service", got)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want the single sibnet row", episodes)
	}
}

// TestAniFilmEpisodesTrailerNeverServed pins the trailer exclusion:
// the trailer "service" is a promo source, never an episode surface —
// an active trailer entry alone is the typed wall.
func TestAniFilmEpisodesTrailerNeverServed(t *testing.T) {
	t.Parallel()

	page := strings.ReplaceAll(
		string(fixture(t, "anifilm_release.html")),
		`:services_props={"kodik":{"from":"kodik","active":true},"rutube":{"from":"rutube","active":false},"trailer":{"from":"trailer","active":false}}`,
		`:services_props={"kodik":{"from":"kodik","active":false},"trailer":{"from":"trailer","active":true}}`,
	)

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/1200-devil-may-cry" {
			_, _ = w.Write([]byte(page))
			return
		}
		t.Errorf("unexpected fetch: %s (trailer must never be polled)", r.URL.Path)
		http.NotFound(w, r)
	})
	p := luaProvider(t, "anifilm", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1200-devil-may-cry")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed (no servable episode service)", err)
	}
	if !strings.Contains(err.Error(), "trailer") {
		t.Errorf("err = %v, want the wall to name the serving service", err)
	}
}

// TestAniFilmEpisodesEmptyPlaylists pins the all-services-empty wall:
// a service answering 200 with an empty playlist is not an episode
// surface either — the wall names the services that were tried.
func TestAniFilmEpisodesEmptyPlaylists(t *testing.T) {
	t.Parallel()

	page := strings.ReplaceAll(
		string(fixture(t, "anifilm_release.html")),
		`:services_props={"kodik":{"from":"kodik","active":true},"rutube":{"from":"rutube","active":false},"trailer":{"from":"trailer","active":false}}`,
		`:services_props={"kodik":{"from":"kodik","active":false},"sibnet":{"from":"sibnet","active":true}}`,
	)

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/1200-devil-may-cry":
			_, _ = w.Write([]byte(page))
		case "/releases/api:online:1200:sibnet":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	})
	p := luaProvider(t, "anifilm", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/1200-devil-may-cry")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "sibnet") {
		t.Errorf("err = %v, want the wall to name the tried service", err)
	}
}

// TestAniFilmResolveStream pins the stream chain against the real
// api:video capture (testdata/anifilm_video.html) with the kodik leg
// served by the test server (kodik_test.go pattern): the embedded
// iframe src runs through the shared extractor factory and yields the
// quality-keyed links. The resolve state channel is RawID (the fresh-
// sandbox streams(raw_id, dub) contract); RawEmbeds rides along for
// consumers.
func TestAniFilmResolveStream(t *testing.T) {
	t.Parallel()

	var kodikPage string
	var base string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/api:video:16231":
			// The real capture points at kodikplayer.com; the offline
			// variant re-hosts the same /uv/ path under the test server
			// (absolute, keeping the "kodik" marker the extractor gate
			// requires — live iframes are absolute too).
			_, _ = w.Write([]byte(kodikHosted(t, kodikPage, base)))
		case strings.Contains(r.URL.Path, "/kodik/uv/637993/"):
			// The player page in the NEW vInfo shape (live-verified
			// 2026-09-23 on kodikplayer.com): vInfo.type/hash/id plus
			// the urlParams JSON blob carrying d/d_sign/pd/pd_sign/
			// ref_sign — the JSON-key template of scrapeParams covers it.
			_, _ = fmt.Fprintf(w, `<!doctype html><html><head><script>`+
				`var urlParams = '{"d":"%[1]s","d_sign":"ds1","pd":"%[1]s","pd_sign":"pds1","ref":"","ref_sign":"rs1"}';`+
				`</script></head><body><script>var vInfo = {};`+
				` vInfo.type = 'seria'; vInfo.hash = 'h-af'; vInfo.id = '1405179';</script>`+
				`<script src="/assets/js/app.player_single.deadbeef.js"></script></body></html>`, strings.TrimPrefix(base, "http://"))
		case r.URL.Path == "/ftor":
			_ = r.ParseForm()
			if got := r.PostForm.Get("hash"); got != "h-af" {
				t.Errorf("posted hash = %q, want h-af (the vInfo scrape must feed the signed POST)", got)
			}
			if got := r.PostForm.Get("d"); got == "" {
				t.Errorf("posted d is empty: the urlParams JSON blob must supply it")
			}
			_, _ = w.Write([]byte(`{"links":{"360":[{"src":"` + afKodikEncodeSrc("http://cdn.example/af360.mp4") + `"}],` +
				`"720":[{"src":"` + afKodikEncodeSrc("http://cdn.example/af720.mp4") + `"}]}}`))
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	base = srv.URL
	kodikPage = string(fixture(t, "anifilm_video.html"))
	p := luaProvider(t, "anifilm", srv.URL)

	episode := contracts.Episode{
		Num:   "1",
		RawID: "16231",
		RawEmbeds: map[string][]string{
			"Боллектив Media": {srv.URL + "/releases/api:video:16231"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Боллектив Media")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Боллектив Media" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	if _, ok := stream.Links["720"]; !ok {
		t.Fatalf("Links = %v, want the 720 key", stream.Links)
	}
	if got := stream.Links["720"].URL; got != "http://cdn.example/af720.mp4" {
		t.Errorf("720 URL = %q, want the decoded link", got)
	}
}

// kodikHosted re-hosts the api:video capture's kodik iframe under the
// test server on an absolute URL whose path keeps the extractor's
// "kodik" gate.
func kodikHosted(t *testing.T, videoPage, base string) string {
	t.Helper()
	return strings.ReplaceAll(videoPage, "https://kodikplayer.com", base+"/kodik")
}

// afKodikEncodeSrc mirrors the kodik wire encoding (extractors'
// kodikEncodeSrc): base64 of the URL, then the -18 caesar shift the
// player page's decoder inverts.
func afKodikEncodeSrc(u string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(u))
	b := []byte(b64)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = (c-'A'+8)%26 + 'A'
		case c >= 'a' && c <= 'z':
			b[i] = (c-'a'+8)%26 + 'a'
		}
	}
	return string(b)
}

// TestAniFilmResolveStreamNoIframe pins the video-page wall: an
// api:video response without the player iframe carries nothing to
// extract — typed ErrExtractFailed.
func TestAniFilmResolveStreamNoIframe(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>no player here</body></html>"))
	})
	p := luaProvider(t, "anifilm", srv.URL)

	episode := contracts.Episode{
		Num:       "1",
		RawID:     "16231",
		RawEmbeds: map[string][]string{"Боллектив Media": {srv.URL + "/releases/api:video:16231"}},
	}
	_, err := p.ResolveStream(context.Background(), episode, "Боллектив Media")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}
