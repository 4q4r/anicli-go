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

func TestKodikSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "kodik_search.json"))
	})
	p := newKodik(srv.URL, "test-token", testClient(t, "kodik"))

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/search" {
		t.Errorf("request path = %q", rec.Path)
	}
	// The token must ride along on the form (kodik.py:48-54).
	formVal := func(key string) string {
		if v, ok := rec.Form[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if formVal("token") != "test-token" {
		t.Errorf("form token = %q, want the configured token", formVal("token"))
	}
	if formVal("title") != "naruto" || formVal("limit") != "20" ||
		formVal("with_material_data") != "true" || formVal("types") != "anime,anime-serial" {
		t.Errorf("form = %v, want the kodik.py payload", rec.Form)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (null-link entry skipped)", len(results))
	}
	if results[0].Title != "Наруто" {
		t.Errorf("Title = %q", results[0].Title)
	}
	// Protocol-relative links gain the https scheme (kodik.py:73 via
	// _common.normalize_protocol_url).
	if results[0].URL != "https://kodik.info/serial/123-456/720p?season=1" {
		t.Errorf("URL = %q, want the normalized link", results[0].URL)
	}
	if results[0].Poster != "https://img.example/naruto.jpg" {
		t.Errorf("Poster = %q, want material_data.poster_url", results[0].Poster)
	}
	if y, ok := results[0].Meta["year"]; !ok || y == nil {
		t.Errorf("Meta[year] = %#v, want the year present", results[0].Meta["year"])
	}
	if results[0].Meta["type"] != "anime-serial" {
		t.Errorf("Meta[type] = %#v", results[0].Meta["type"])
	}
	// title_orig fallback + anime_poster_url fallback (kodik.py:69-80).
	if results[1].Title != "No Title Anime" {
		t.Errorf("Title = %q, want title_orig fallback", results[1].Title)
	}
	if results[1].Poster != "https://img.example/no-title.jpg" {
		t.Errorf("Poster = %q, want anime_poster_url fallback", results[1].Poster)
	}
	if y, ok := results[1].Meta["year"]; !ok || y != nil {
		t.Errorf("Meta[year] = %#v, want a present-but-null year", y)
	}
}

// The API answers 401 without a valid token (verified 2026-09-12); an
// empty token must fail loud BEFORE any request leaves the process.
func TestKodikSearchTokenMissing(t *testing.T) {
	t.Parallel()

	// A dead endpoint proves the guard short-circuits: were a request
	// attempted, it would fail into the silent-empty path and this test
	// would see no error.
	p := newKodik("http://"+newDeadListener(t).Addr().String(), "", testClient(t, "kodik"))

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search with an empty token must fail loud")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	for _, want := range []string{"providers.kodik.token", "ANICLI_KODIK_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, must mention %q", err, want)
		}
	}
}

// A rejected token (HTTP 401) maps onto a loud config-flavored error
// instead of the silent-empty path the Python failover loop produced.
func TestKodikSearch401(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := newKodik(srv.URL, "bad-token", testClient(t, "kodik"))

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search with a rejected token must fail loud")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %v, want a ProviderError with status 401", err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %v, must mention the token", err)
	}
}

// Python safe_json_loads + the per-host except-block swallow decode
// failures into an empty result (kodik.py:63-95).
func TestKodikSearchDecodeFailureSilent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "not-json")
	})
	p := newKodik(srv.URL, "tok", testClient(t, "kodik"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v, want silent empty", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

// Transport failures are swallowed like decode failures (the Python
// per-host RequestException catch, kodik.py:93-94).
func TestKodikSearchTransportFailureSilent(t *testing.T) {
	t.Parallel()

	p := newKodik("http://"+newDeadListener(t).Addr().String(), "tok", testClient(t, "kodik"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v, want silent empty", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestKodikGetEpisodesSerial(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "kodik_serial.html"))
	})
	p := newKodik(srv.URL, "tok", testClient(t, "kodik"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/serial/450-abc/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[0].RawID != "1" {
		t.Errorf("episodes[0] num/rawID = %q/%q", episodes[0].Num, episodes[0].RawID)
	}
	embeds := episodes[1].RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("RawEmbeds = %v, want 2 translations", embeds)
	}
	// URL construction pattern (kodik.py:143-147): season hardcoded to 1.
	if v := embeds["Original"]; len(v) != 1 ||
		v[0] != "https://kodik.info/serial/450123/abc123def/720p?min_age=16&first_url=false&season=1&episode=2" {
		t.Errorf("Original embed = %v", v)
	}
	if v := embeds["Dub Two"]; len(v) != 1 ||
		v[0] != "https://kodik.info/serial/450124/fed321cba/720p?min_age=16&first_url=false&season=1&episode=2" {
		t.Errorf("Dub Two embed = %v", v)
	}
}

func TestKodikGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "kodik_movie.html"))
	})
	p := newKodik(srv.URL, "tok", testClient(t, "kodik"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/video/998/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want a single film entry", len(episodes))
	}
	film := episodes[0]
	if film.Num != "1" || film.Title != "Фильм" || film.RawID != "movie" {
		t.Errorf("film = {%q %q %q}", film.Num, film.Title, film.RawID)
	}
	v := film.RawEmbeds["Оригинал"]
	if len(v) != 1 || v[0] != "https://kodik.info/video/998/hash998/720p?min_age=16&first_url=false" {
		t.Errorf("embed = %v, want the movie URL pattern (kodik.py:159-163)", v)
	}
}

func TestKodikGetEpisodesPromoError(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="promo-error">Контент недоступен в вашем регионе</div>`)
	})
	p := newKodik(srv.URL, "tok", testClient(t, "kodik"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/video/x/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 on a promo-error page (kodik.py:107-108)", len(episodes))
	}
}

// Without a translations select box, the media id/hash pair is scraped
// off inline scripts into a "Default" translation (kodik.py:219-247).
func TestKodikGetEpisodesDefaultTranslation(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "kodik_default.html"))
	})
	p := newKodik(srv.URL, "tok", testClient(t, "kodik"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/serial/777-h777/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	v := episodes[0].RawEmbeds["Default"]
	if len(v) != 1 ||
		v[0] != "https://kodik.info/serial/777/h777/720p?min_age=16&first_url=false&season=1&episode=1" {
		t.Errorf("Default embed = %v", v)
	}
}

// The page fetch sits inside the Python try-block (kodik.py:100-104):
// transport failures yield an empty episode list.
func TestKodikGetEpisodesTransportFailureSilent(t *testing.T) {
	t.Parallel()

	p := newKodik("http://"+newDeadListener(t).Addr().String(), "tok", testClient(t, "kodik"))

	episodes, err := p.GetEpisodes(context.Background(), "http://nope.invalid/video/1/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v, want silent empty", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

func TestKodikResolveStreamPendingExtractor(t *testing.T) {
	t.Parallel()

	p := newKodik("https://kodik-api.com", "tok", testClient(t, "kodik"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"Original": {"https://kodik.info/serial/450123/abc123def/720p?min_age=16&first_url=false&season=1&episode=1"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want pending ErrExtractFailed for the kodik player link", err)
	}
	if !strings.Contains(err.Error(), "kodik") {
		t.Errorf("error = %v, want the kodik extractor name", err)
	}
	if stream.DubName != "Original" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestKodikProviderMeta(t *testing.T) {
	t.Parallel()

	p := newKodik(KodikAPIBase, "tok", testClient(t, "kodik"))
	if p.ID() != "kodik" || p.Name() != "Kodik" || p.BaseURL() != KodikAPIBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
