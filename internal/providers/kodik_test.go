package providers

// The kodik provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/kodik/main.lua) since the PR140
// Go→Lua migration — these tests pin the script through the same
// contracts.Provider surface and the same fixtures the compiled Go
// implementation was held to (kodik_search.json, the three player-page
// captures). The token rides the PR140 provider_setting seam; the
// harness injects it the way the factory flattens
// providers.kodik.token.
//
// LIVE VERIFICATION IS IMPOSSIBLE for this provider: no owner token
// exists (the API answers 401 without one, verified 2026-09-12), so
// the live legs stay unproven — the fixtures carry the whole proof and
// the smoke gate keeps kodik in its credential-gated SKIP roster.
//
// Contract shifts forced by the fresh-sandbox Lua adapter, documented
// here rather than hidden (the sameband/anifilm precedent):
//
//   - the per-dub embed state rides episode RawID as the JSON
//     {dub: url} map — the only state channel into the per-invocation
//     streams(raw_id, dub) call (the Go provider read RawEmbeds back,
//     which the adapter does not pass into streams); RawEmbeds keeps
//     carrying the same embeds for consumers;
//   - a JSON-null search year drops the meta key entirely (Lua tables
//     hold no nil slot — the Go provider's "present-but-null" mirrors
//     item.get() was a Pythonism; consumers key-check either way);
//   - the rejected-token 401 keeps the typed ErrInvalidInput failure,
//     but the Lua anicli.fail channel carries no HTTP status, so the
//     ProviderError status is 0 (the Go wrap carried 401).

import (
	"context"
	"encoding/json"
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
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "test-token"})

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST (the tokenled search form, kodik.py:48-54)", rec.Method)
	}
	if rec.Path != "/search" {
		t.Errorf("request path = %q", rec.Path)
	}
	// The token must ride on the form (kodik.py:48-54).
	formVal := func(key string) string {
		if v, ok := rec.Form[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if formVal("token") != "test-token" {
		t.Errorf("form token = %q, want the provider_setting token", formVal("token"))
	}
	if formVal("title") != "naruto" || formVal("limit") != "20" ||
		formVal("with_material_data") != "true" || formVal("types") != "anime,anime-serial" {
		t.Errorf("form = %v, want the kodik.py payload", rec.Form)
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want the form encoding", ct)
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
	// The Lua contract shift: a JSON-null year has no meta slot (Lua
	// tables hold no nil — see the header).
	if y, present := results[1].Meta["year"]; present {
		t.Errorf("Meta[year] = %#v, want the key ABSENT for the JSON-null year (the Lua adapter drops present-but-null)", y)
	}
}

// The API answers 401 without a valid token (verified 2026-09-12); an
// empty token must fail loud BEFORE any request leaves the process —
// the Go port's error policy verbatim, now via anicli.fail.
func TestKodikSearchTokenMissing(t *testing.T) {
	t.Parallel()

	// A dead endpoint proves the guard short-circuits: were a request
	// attempted, it would fail into the silent-empty path and this test
	// would see no error. No settings map at all — the provider_setting
	// read yields nil.
	p := luaProviderWithSettings(t, "kodik",
		"http://"+newDeadListener(t).Addr().String(), nil)

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

// An empty-string token (the configured-but-blank section) is the same
// loud failure: provider_setting returns "" and the script treats it
// exactly like nil.
func TestKodikSearchTokenBlankIsLoud(t *testing.T) {
	t.Parallel()

	p := luaProviderWithSettings(t, "kodik",
		"http://"+newDeadListener(t).Addr().String(),
		map[string]string{"token": ""})

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput for the blank token", err)
	}
}

// A rejected token (HTTP 401) maps onto a loud config-flavored error
// instead of the silent-empty path the Python failover loop produced.
// The Lua anicli.fail channel carries no HTTP status (header note), so
// the pin is the sentinel and the message.
func TestKodikSearch401(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "bad-token"})

	_, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search with a rejected token must fail loud")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
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
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})

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

	p := luaProviderWithSettings(t, "kodik",
		"http://"+newDeadListener(t).Addr().String(),
		map[string]string{"token": "tok"})

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
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/serial/450-abc/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(episodes))
	}
	if episodes[0].Num != "1" {
		t.Errorf("episodes[0] num = %q, want the option text", episodes[0].Num)
	}
	embeds := episodes[1].RawEmbeds
	if len(embeds) != 2 {
		t.Fatalf("RawEmbeds = %v, want 2 translations (the no-media-id option skipped)", embeds)
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
	// The fresh-sandbox state channel: RawID carries the JSON
	// {dub: url} map streams() re-derives the embed from.
	var state map[string]string
	if err := json.Unmarshal([]byte(episodes[1].RawID), &state); err != nil {
		t.Fatalf("RawID state = %q: %v", episodes[1].RawID, err)
	}
	if state["Original"] != embeds["Original"][0] || state["Dub Two"] != embeds["Dub Two"][0] {
		t.Errorf("RawID state = %v, want the embed map verbatim", state)
	}
}

func TestKodikGetEpisodesMovie(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "kodik_movie.html"))
	})
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/video/998/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want a single film entry", len(episodes))
	}
	film := episodes[0]
	if film.Num != "1" || film.Title != "Фильм" {
		t.Errorf("film num/title = %q/%q", film.Num, film.Title)
	}
	v := film.RawEmbeds["Оригинал"]
	if len(v) != 1 || v[0] != "https://kodik.info/video/998/hash998/720p?min_age=16&first_url=false" {
		t.Errorf("embed = %v, want the movie URL pattern (kodik.py:159-163)", v)
	}
	var state map[string]string
	if err := json.Unmarshal([]byte(film.RawID), &state); err != nil {
		t.Fatalf("RawID state = %q: %v", film.RawID, err)
	}
	if state["Оригинал"] != v[0] {
		t.Errorf("RawID state = %v, want the embed map verbatim", state)
	}
}

func TestKodikGetEpisodesPromoError(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="promo-error">Контент недоступен в вашем регионе</div>`)
	})
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})

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
	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})

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

	p := luaProviderWithSettings(t, "kodik",
		"http://"+newDeadListener(t).Addr().String(),
		map[string]string{"token": "tok"})

	episodes, err := p.GetEpisodes(context.Background(), "http://nope.invalid/video/1/720p")
	if err != nil {
		t.Fatalf("GetEpisodes: %v, want silent empty", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

func TestKodikResolveStreamRoundTrip(t *testing.T) {
	t.Parallel()

	// Local kodik player fake: hash/id vars on the page, /ftor API with
	// a plain passthrough src (extractors.py:193-194 — bare https .m3u8
	// srcs are not encoded). The episode state is hand-built the way
	// GetEpisodes leaves it (the raw_id JSON embed map).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ftor" {
			_, _ = fmt.Fprint(w, `{"links": {"720": [{"src": "https://plain.example/x/720.m3u8"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `<html><script>var hash = "h123"; var id = "456";</script></html>`)
	})

	p := luaProviderWithSettings(t, "kodik", srv.URL, map[string]string{"token": "tok"})
	state, err := json.Marshal(map[string]string{
		"Original": srv.URL + "/kodik/serial/450123/abc123def/720p",
	})
	if err != nil {
		t.Fatal(err)
	}
	episode := contracts.Episode{
		Num:   "1",
		RawID: string(state),
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Original" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry from the kodik extractor", stream.Links)
	}
	if src.URL != "https://plain.example/x/720.m3u8" {
		t.Errorf("720 URL = %q", src.URL)
	}
}

// An unknown dub keeps the Go parity: an empty stream, no error (the
// Go provider resolved a nil embed slice into an empty link set).
func TestKodikResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := luaProviderWithSettings(t, "kodik", "https://kodik-api.com", map[string]string{"token": "tok"})
	state, err := json.Marshal(map[string]string{"Original": "https://kodik.info/video/1/h/720p"})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{RawID: string(state)}, "Nope")
	if err != nil {
		t.Fatalf("ResolveStream: %v, want the empty-stream parity", err)
	}
	if len(stream.Links) != 0 || stream.DubName != "Nope" {
		t.Errorf("stream = %+v, want {DubName Nope, no links}", stream)
	}
}

func TestKodikProviderMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "kodik")
	if p.ID() != "kodik" || p.Name() != "Kodik" || p.BaseURL() != "https://kodik-api.com" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if lc, ok := p.(interface{ ContentLanguage() string }); !ok || lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %v, want ru", lc)
	}
}
