package providers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// animevib fixtures are real captures (2026-09-25). PR116: the
// provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animevib/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. The resolve leg
// re-derives from the state JSON ({n, u}) per the fresh-sandbox
// contract, so the resolve worlds serve the full post → serial →
// seria chain offline; the fixture host rewrite appends the /kodik
// path segment so the shared kodik extractor's URL gate matches
// (live embeds carry it on the host, kodikplayer.com).

// avKodikWorld serves the animevib post page and the kodik pages it
// references on ONE test server: the post fixture keeps its real
// kodikplayer.com src (capture fidelity) and the handler rewrites the
// host to the test server (+ the /kodik segment) before serving, so
// the provider's embed-host derivation and the extractor gate stay
// offline. Paths route by the kodik media id. The shared fixtureServer
// recorder is deliberately NOT used: GetEpisodes fires the translation
// pages bounded-parallel, and concurrent handler entries would race
// the single-slot recorder.
func avKodikWorld(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2937-n84b-dandadan-1.html":
			body := bytes.ReplaceAll(fixture(t, "animevib_post_serial.html"),
				[]byte("//kodikplayer.com/"), []byte("//"+r.Host+"/kodik/"))
			_, _ = w.Write(body) //nolint:gosec // trusted testdata fixture, host-rewritten for offline use
		case strings.HasPrefix(r.URL.Path, "/kodik/serial/62118/"):
			_, _ = w.Write(fixture(t, "animevib_kodik_serial.html"))
		case strings.HasPrefix(r.URL.Path, "/kodik/serial/62137/"):
			_, _ = w.Write(fixture(t, "animevib_kodik_serial_anidub.html"))
		case strings.HasPrefix(r.URL.Path, "/kodik/seria/"):
			_, _ = w.Write(fixture(t, "animevib_kodik_seria.html"))
		case r.URL.Path == "/ftor":
			_, _ = w.Write([]byte(`{"links": {"720": [{"src": "https://cdn.example/video/ep1.m3u8"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnimeVibSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animevib_search.html"))
	})
	p := luaProvider(t, "animevib", srv.URL)

	results, err := p.Search(context.Background(), "дандадан")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The DLE search form is a GET with the story parameter; the server
	// does the matching (both RU and latin queries surface results —
	// verified live with дандадан and dandadan).
	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}
	if rec.Path != "/index.php" {
		t.Errorf("request path = %q, want /index.php", rec.Path)
	}
	if want := "do=search&subaction=search&story=%D0%B4%D0%B0%D0%BD%D0%B4%D0%B0%D0%B4%D0%B0%D0%BD"; rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 fixture cards", len(results))
	}
	first := results[0]
	if first.Title != "Дандадан 2" {
		t.Errorf("Title = %q, want the div.color-text text", first.Title)
	}
	if first.URL != "https://www.animevib.ru/7773-n84b-dandadan-2.html" {
		t.Errorf("URL = %q, want the a.short__title href verbatim", first.URL)
	}
	if first.SourceID != "animevib" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster != "https://www.animevib.ru/uploads/posts/2025-07/dandadan-2.webp" {
		t.Errorf("Poster = %q, want the figure background-image url", first.Poster)
	}
	if results[1].Title != "Дандадан" {
		t.Errorf("results[1].Title = %q", results[1].Title)
	}
}

func TestAnimeVibSearchNoResultsIsEmpty(t *testing.T) {
	t.Parallel()

	// A junk query answers the same shell with zero movie-item cards
	// (verified live: story=zxqjunknothing -> HTTP 200, 0 cards).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body><article class=\"side_ongoing\"></article></body></html>"))
	})
	p := luaProvider(t, "animevib", srv.URL)

	results, err := p.Search(context.Background(), "zxqjunknothing")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAnimeVibSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := luaProvider(t, "animevib", srv.URL)

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

// TestAnimeVibGetEpisodesSerial walks the full serial flow: post page →
// iframe.player-shar embed → the default translation's serial page
// (fetched once; it doubles as the JAM entry of the translations list)
// → every translation's serial page → the merged (episode × dub) embed
// table. The fixture serves only JAM (62118) and AniDUB (62137); the
// other 46 translations of the real list 404 and must soft-fail — a
// dead dub team must not take the episode table down.
func TestAnimeVibGetEpisodesSerial(t *testing.T) {
	t.Parallel()

	world := avKodikWorld(t)
	p := luaProvider(t, "animevib", world.URL)

	episodes, err := p.GetEpisodes(context.Background(), world.URL+"/2937-n84b-dandadan-1.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The kodik page carries 24 per-episode options (12 in the visible
	// .serial-series-box select + 12 in the hidden div.season-1 group):
	// the parse must scope to the visible select only or the table
	// doubles.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12 (visible select only)", len(episodes))
	}
	for i, ep := range episodes {
		if ep.Num != strconv.Itoa(i+1) {
			t.Errorf("episodes[%d].Num = %q, want %d (numeric order)", i, ep.Num, i+1)
		}
		if ep.Title != strconv.Itoa(i+1)+" серия" {
			t.Errorf("episodes[%d].Title = %q", i, ep.Title)
		}
	}

	// Episode 1 exists under BOTH resolved translations with their own
	// seria hashes; the 46 soft-failed translations contribute nothing.
	first := episodes[0]
	if got := len(first.RawEmbeds); got != 2 {
		t.Fatalf("ep1 dub keys = %d (%v), want exactly {JAM, AniDUB}", got, keys(first.RawEmbeds))
	}
	jam := first.RawEmbeds["JAM"]
	if len(jam) != 1 {
		t.Fatalf("JAM embeds = %v, want one seria embed", jam)
	}
	wantJAM := world.URL + "/kodik/seria/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p"
	if jam[0] != wantJAM {
		t.Errorf("JAM ep1 embed = %q, want %q", jam[0], wantJAM)
	}
	aniDub := first.RawEmbeds["AniDUB"]
	if len(aniDub) != 1 {
		t.Fatalf("AniDUB embeds = %v, want one seria embed", aniDub)
	}
	wantAniDUB := world.URL + "/kodik/seria/1362576/2be98362b81b395fdaaad1eb55d4763b/720p"
	if aniDub[0] != wantAniDUB {
		t.Errorf("AniDUB ep1 embed = %q, want %q", aniDub[0], wantAniDUB)
	}

	// Episode 12 keeps its own seria hash (from the live capture).
	last := episodes[11]
	if got := last.RawEmbeds["JAM"]; len(got) != 1 || got[0] != world.URL+"/kodik/seria/1396547/d5a71bb00c4d0ba58f5e822311e69ebb/720p" {
		t.Errorf("JAM ep12 embed = %v, want the captured seria", got)
	}
}

// TestAnimeVibGetEpisodesVideoSingle pins the /video/ embed shape
// (movies): no serial page fetch at all — the embed URL itself becomes
// the single episode's raw embed under the AnimeVib dub key
// (amdServiceDub precedent for unnamed single-source pages).
func TestAnimeVibGetEpisodesVideoSingle(t *testing.T) {
	t.Parallel()

	fetches := 0
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`<html><body><div class="block-player">` + //nolint:gosec // test-server host echo, trusted offline fixture shape
			`<iframe title="Смотреть онлайн" class="player-shar" width="100%" height="100%" ` +
			`src="//` + r.Host + `/kodik/video/113757/15dee337f31b61f618be0c17b4b48b4e/720p" ` +
			`frameborder="0" allowfullscreen></iframe></div></body></html>`))
	})
	p := luaProvider(t, "animevib", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/6983-klinok.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1 (video embeds are not dereferenced)", fetches)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.Num != "1" {
		t.Errorf("Num = %q, want 1", ep.Num)
	}
	embeds := ep.RawEmbeds["AnimeVib"]
	// The protocol-relative src absolutizes with the post page's
	// scheme — http here (httptest), https on the production site.
	want := "http://" + srv.Listener.Addr().String() + "/kodik/video/113757/15dee337f31b61f618be0c17b4b48b4e/720p"
	if len(embeds) != 1 || embeds[0] != want {
		t.Errorf("RawEmbeds = %v, want %q", ep.RawEmbeds, want)
	}
}

// TestAnimeVibGetEpisodesUnsupportedEmbed: a player-shar iframe that is
// neither a kodik /serial/ nor /video/ embed cannot be split — typed
// ErrExtractFailed naming the URL (no silent invention).
func TestAnimeVibGetEpisodesUnsupportedEmbed(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>` +
			`<iframe class="player-shar" src="https://rearm-as.stloadi.live/?kp=5458831&token=abc"></iframe>` +
			`</body></html>`))
	})
	p := luaProvider(t, "animevib", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/post.html")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "stloadi") {
		t.Errorf("error = %v, must name the unsupported embed", err)
	}
}

// Pages without the player-shar iframe are a typed wall (wave A
// review F2): a silent empty would fake a healthy title with no
// episodes.
func TestAnimeVibGetEpisodesNoPlayerIsTypedWall(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>no player here</body></html>"))
	})
	p := luaProvider(t, "animevib", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/none.html")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (the typed no-player wall)", err)
	}
}

// TestAnimeVibGetEpisodesSerialZeroEpisodesIsTypedWall pins the second
// silent-empty wall (wave A review F2): a serial page whose visible
// series select carries no hash-bearing options builds an empty table —
// typed ErrNotFound, never (nil, nil).
func TestAnimeVibGetEpisodesSerialZeroEpisodesIsTypedWall(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2937-empty-serial.html":
			//nolint:gosec // test-server host echo, trusted offline fixture shape
			_, _ = w.Write([]byte(`<html><body>` +
				`<iframe class="player-shar" src="http://` + r.Host + `/serial/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p"></iframe>` +
				`</body></html>`))
		case strings.HasPrefix(r.URL.Path, "/serial/1362451/"):
			// Translations select present, but the visible series
			// select carries no data-id/data-hash options at all.
			_, _ = w.Write([]byte(`<html><body>` +
				`<div class="serial-translations-box"><option data-media-id="1362451" data-media-hash="a899a9b6ef3945f70a33c6ba2cd52c70" data-title="JAM">JAM (0 эп.)</option></div>` +
				`<div class="serial-series-box"><select></select></div>` +
				`</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p := luaProvider(t, "animevib", srv.URL)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/2937-empty-serial.html")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (the empty-table wall)", err)
	}
}

// TestAnimeVibResolveStream proves the re-derived resolve leg: post →
// embed → the dub's serial page → the episode's seria embed → the
// kodik extractor (vInfo scrape) → the direct link.
func TestAnimeVibResolveStream(t *testing.T) {
	t.Parallel()

	world := avKodikWorld(t)
	p := luaProvider(t, "animevib", world.URL)

	rawID, err := luaStateJSON(world.URL+"/2937-n84b-dandadan-1.html", "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	episode := contracts.Episode{
		Num:   "1",
		Title: "1 серия",
		RawID: rawID,
		RawEmbeds: map[string][]string{
			"JAM": {world.URL + "/kodik/seria/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "JAM")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "JAM" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry from the kodik extractor", stream.Links)
	}
	if src.URL != "https://cdn.example/video/ep1.m3u8" {
		t.Errorf("720 URL = %q, want the extractor's resolved link", src.URL)
	}
}

// TestAnimeVibResolveStreamUnknownDubIsTypedWall pins the caller-bug
// wall (wave A review F2, animedia precedent): a dub the serial page
// does not carry is typed ErrInvalidInput. The fresh-sandbox resolve
// re-derives before the dub lookup — the world serves the flow.
func TestAnimeVibResolveStreamUnknownDubIsTypedWall(t *testing.T) {
	t.Parallel()

	world := avKodikWorld(t)
	p := luaProvider(t, "animevib", world.URL)

	rawID, err := luaStateJSON(world.URL+"/2937-n84b-dandadan-1.html", "1")
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	_, err = p.ResolveStream(context.Background(),
		contracts.Episode{Num: "1", RawID: rawID, RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// TestAnimeVibResolveStreamGarbageStateIsTypedWall pins the typed
// wall for ANY raw_id byte sequence (the owner-reported crash class,
// issue #157): the merged-session convention bytes —
// "animevib:{...}", what a caller that composes the prov:id prefix
// without decomposing it hands the provider, first byte 'a' — and
// shape-drifted state JSON must surface as the typed ErrInvalidInput
// wall, never the raw json.decode VM error through to the user.
func TestAnimeVibResolveStreamGarbageStateIsTypedWall(t *testing.T) {
	t.Parallel()

	world := avKodikWorld(t)
	p := luaProvider(t, "animevib", world.URL)

	cases := []struct {
		name  string
		rawID string
	}{
		{"merged-convention prefix bytes", `animevib:{"n":"1","u":"` + world.URL + `/2937-n84b-dandadan-1.html"}`},
		{"plain non-json text", "about:blank"},
		{"state json missing the u leg", `{"n":"1"}`},
		{"state json of the wrong shape", `[1,2,3]`},
		{"empty raw id", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.ResolveStream(context.Background(),
				contracts.Episode{Num: "1", RawID: tc.rawID, RawEmbeds: map[string][]string{}}, "JAM")
			if !errors.Is(err, contracts.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestAnimeVibSmokeQuery pins the declared live probe (PR51 mechanism):
// both shared probes miss this catalog — Black Lagoon is not on
// animevib [LIVE-VERIFIED 2026-09-25].
func TestAnimeVibSmokeQuery(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animevib")
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("animevib must declare contracts.SmokeQueryProvider")
	}
	if got := sq.SmokeQuery(); got != "дандадан" {
		t.Errorf("SmokeQuery = %q, want дандадан", got)
	}
}

// TestAnimeVibIdentity pins the registration-card values the factory
// roster and the README table render.
func TestAnimeVibIdentity(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animevib")
	if p.ID() != "animevib" {
		t.Errorf("ID = %q", p.ID())
	}
	if p.Name() != "AnimeVib" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.BaseURL() != "https://www.animevib.ru" {
		t.Errorf("BaseURL = %q", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if lc := p.(interface{ ContentLanguage() string }); lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", lc.ContentLanguage())
	}
}
