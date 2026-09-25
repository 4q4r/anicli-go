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

// [LIVE-VERIFIED 2026-09-25] animevib is NOT a port: www.animevib.ru is
// a DLE (DataLife Engine) catalog written against the live site. The
// search is the DLE GET form (index.php?do=search&subaction=search&story=…)
// rendering article.movie-item result cards server-side; the ?s= query
// the controller intel mentioned is silently ignored by DLE (it answers
// the homepage). Every post embeds ONE kodik player on
// iframe.player-shar: /serial/ embeds carry the translations list and
// per-episode seria hashes on the kodik page itself, /video/ embeds are
// single-episode movies. The stloadi.live ad player in the second tab
// is never classed player-shar.

// avKodikWorld serves the animevib post page and the kodik pages it
// references on ONE test server: the post fixture keeps its real
// kodikplayer.com src (capture fidelity) and the handler rewrites the
// host to the test server before serving, so the provider's embed-host
// derivation stays offline. Paths route by the kodik media id. The
// shared fixtureServer recorder is deliberately NOT used: GetEpisodes
// fires the translation pages bounded-parallel, and concurrent handler
// entries would race the single-slot recorder.
func avKodikWorld(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2937-n84b-dandadan-1.html":
			body := bytes.ReplaceAll(fixture(t, "animevib_post_serial.html"),
				[]byte("kodikplayer.com"), []byte(r.Host))
			_, _ = w.Write(body) //nolint:gosec // trusted testdata fixture, host-rewritten for offline use
		case strings.HasPrefix(r.URL.Path, "/serial/62118/"):
			_, _ = w.Write(fixture(t, "animevib_kodik_serial.html"))
		case strings.HasPrefix(r.URL.Path, "/serial/62137/"):
			_, _ = w.Write(fixture(t, "animevib_kodik_serial_anidub.html"))
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
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

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
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

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
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

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
// dead dub team must not take the episode table down (resolveEmbeds
// first-error semantics).
func TestAnimeVibGetEpisodesSerial(t *testing.T) {
	t.Parallel()

	world := avKodikWorld(t)
	p := newAnimeVib(world.URL, testClient(t, "animevib"), 4)

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
		t.Fatalf("ep1 dub keys = %d (%v), want exactly {JAM, AniDUB}", got, avDubKeys(first.RawEmbeds))
	}
	jam := first.RawEmbeds["JAM"]
	if len(jam) != 1 {
		t.Fatalf("JAM embeds = %v, want one seria embed", jam)
	}
	wantJAM := world.URL + "/seria/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p"
	if jam[0] != wantJAM {
		t.Errorf("JAM ep1 embed = %q, want %q", jam[0], wantJAM)
	}
	aniDub := first.RawEmbeds["AniDUB"]
	if len(aniDub) != 1 {
		t.Fatalf("AniDUB embeds = %v, want one seria embed", aniDub)
	}
	wantAniDUB := world.URL + "/seria/1362576/2be98362b81b395fdaaad1eb55d4763b/720p"
	if aniDub[0] != wantAniDUB {
		t.Errorf("AniDUB ep1 embed = %q, want %q", aniDub[0], wantAniDUB)
	}

	// Episode 12 keeps its own seria hash (from the live capture).
	last := episodes[11]
	if got := last.RawEmbeds["JAM"]; len(got) != 1 || got[0] != world.URL+"/seria/1396547/d5a71bb00c4d0ba58f5e822311e69ebb/720p" {
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
		_, _ = w.Write([]byte(`<html><body><div class="block-player">` +
			`<iframe title="Смотреть онлайн" class="player-shar" width="100%" height="100%" ` +
			`src="//kodikplayer.com/video/113757/15dee337f31b61f618be0c17b4b48b4e/720p" ` +
			`frameborder="0" allowfullscreen></iframe></div></body></html>`))
	})
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

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
	want := "http://kodikplayer.com/video/113757/15dee337f31b61f618be0c17b4b48b4e/720p"
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
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/post.html")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "stloadi") {
		t.Errorf("error = %v, must name the unsupported embed", err)
	}
}

// Pages without the player-shar iframe yield no episodes, no error
// (anidub precedent).
func TestAnimeVibGetEpisodesNoPlayerIsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>no player here</body></html>"))
	})
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/none.html")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

// TestAnimeVibResolveStream proves the synthesized seria embeds ride
// the shared extractor factory: the kodik extractor scrapes the seria
// page's vInfo (type/hash/id) and resolves the direct links.
func TestAnimeVibResolveStream(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kodik/seria/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p":
			_, _ = w.Write(fixture(t, "animevib_kodik_seria.html"))
		case "/ftor":
			// A plain https .m3u8 src passes through the kodik decode
			// verbatim (extractors.py:193-194) — a deterministic proof
			// the extractor ran against the seria page.
			_, _ = w.Write([]byte(`{"links": {"720": [{"src": "https://cdn.example/video/ep1.m3u8"}]}}`))
		default:
			http.NotFound(w, r)
		}
	})
	p := newAnimeVib(srv.URL, testClient(t, "animevib"), 4)

	episode := contracts.Episode{
		Num:   "1",
		Title: "1 серия",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"JAM": {srv.URL + "/kodik/seria/1362451/a899a9b6ef3945f70a33c6ba2cd52c70/720p"},
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

func TestAnimeVibResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newAnimeVib(AnimeVibBase, testClient(t, "animevib"), 4)

	stream, err := p.ResolveStream(context.Background(),
		contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for an unknown dub", stream.Links)
	}
}

func TestAnimeVibProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimeVib(AnimeVibBase, testClient(t, "animevib"), 4)
	if p.ID() != "animevib" || p.Name() != "AnimeVib" || p.BaseURL() != AnimeVibBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// RU dubs = wanted-language audio + video (PR23 semantics).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
	// The shared smoke probes (черная лагуна / black lagoon) surface
	// nothing on this catalog (live-verified 2026-09-25): the provider
	// speaks for itself.
	if got := p.SmokeQuery(); got != "дандадан" {
		t.Errorf("SmokeQuery = %q, want дандадан", got)
	}
	// RU and latin queries both match the DLE index (verified live):
	// deliberately NO NamePreference declaration (default routing).
	if _, declares := interface{}(p).(contracts.NamePreferenceProvider); declares {
		t.Error("animevib must not declare NamePreference (RU+latin index)")
	}
}

// avDubKeys lists the RawEmbeds keys in sorted order for failure output.
func avDubKeys(embeds map[string][]string) []string {
	keys := make([]string, 0, len(embeds))
	for k := range embeds {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
