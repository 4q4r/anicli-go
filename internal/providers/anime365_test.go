package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// testAnime365 builds the provider against one httptest fixture server
// (single-mirror list) and returns it with the request recorder.
func testAnime365(t *testing.T, token string, handle func(w http.ResponseWriter, r *http.Request)) (*Anime365, *recordedRequest) {
	t.Helper()
	srv, rec := fixtureServer(t, handle)
	return newAnime365([]string{srv.URL}, token, testClient(t, "anime365")), rec
}

// anime365JSON writes an envelope with the JSON content type.
func anime365JSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// TestAnime365Meta pins the service-level identity: registration
// identity, BOTH content semantics (RU voice-overs over present video),
// the RU content language of the translation catalog, the DubsHydrator
// capability and NO NamePreference declaration (the index matches RU
// titles; default query routing).
func TestAnime365Meta(t *testing.T) {
	t.Parallel()

	p := newAnime365([]string{"https://smotret-anime.app"}, "tok", testClient(t, "anime365"))
	if p.ID() != "anime365" || p.Name() != "Anime365" {
		t.Errorf("identity = %q/%q, want anime365/Anime365", p.ID(), p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// Compile-time capability pins: dropping the DubsHydrator
// implementation fails the build; *Anime365 deliberately does NOT
// declare NamePreference (RU-index default routing), which the
// compiler also enforces — a latin-only declaration would be a
// visible type change.
var _ contracts.DubsHydrator = (*Anime365)(nil)

// TestAnime365Search pins the series search against the real captured
// Dandadan response: the open catalog endpoint, its query form, the
// required User-Agent and the per-series fields including the
// Shikimori mapping surfaced from links[] (controller ruling).
func TestAnime365Search(t *testing.T) {
	t.Parallel()

	p, rec := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, fixture(t, "anime365_search.json"))
	})

	results, err := p.Search(context.Background(), "дандадан")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/api/series" {
		t.Errorf("request path = %q, want /api/series", rec.Path)
	}
	if q := rec.Query; !containsAll(q, "query=%D0%B4%D0%B0%D0%BD%D0%B4%D0%B0%D0%B4%D0%B0%D0%BD", "limit=20") {
		t.Errorf("query = %q, want the encoded RU query and limit=20", q)
	}
	if rec.Header.Get("User-Agent") == "" {
		t.Error("the anime365 API requires a User-Agent header, got none")
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	first := results[0]
	if first.Title != "Дандадан / Dandadan" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "https://smotret-anime.org/catalog/dandadan-35439" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Poster != "https://smotret-anime.app/posters/35439.3963503694.jpg" {
		t.Errorf("Poster = %q", first.Poster)
	}
	if first.SourceID != "anime365" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if got := first.Meta["shikimori"]; got != "https://shikimori.io/animes/57334" {
		t.Errorf("Meta[shikimori] = %#v, want the links[] Шикимори entry", got)
	}
	if got, ok := first.Meta["mal_id"].(int64); !ok || got != 57334 {
		t.Errorf("Meta[mal_id] = %#v, want 57334", first.Meta["mal_id"])
	}
	if results[1].Title != "Дандадан 2 сезон / Dandadan 2nd Season" {
		t.Errorf("second Title = %q", results[1].Title)
	}
}

// TestAnime365GetEpisodes pins the episode listing: the series id is
// parsed from the search URL's trailing -<id>, the open /api/episodes
// endpoint is queried with a large page, and ONLY content episode types
// survive — previews (trailers), openings, endings and "other" singles
// are junk for the dub-watch flow (live capture: 27 entries → 24 tv).
// Inactive-but-real episodes (Dandadan ep24 is isActive=0 live) stay.
func TestAnime365GetEpisodes(t *testing.T) {
	t.Parallel()

	p, rec := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, fixture(t, "anime365_episodes.json"))
	})

	episodes, err := p.GetEpisodes(context.Background(), "https://smotret-anime.org/catalog/dandadan-35439")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/api/episodes" {
		t.Errorf("request path = %q, want /api/episodes", rec.Path)
	}
	if q := rec.Query; !containsAll(q, "seriesId=35439", "limit=2500") {
		t.Errorf("query = %q, want seriesId=35439 and limit=2500", q)
	}

	if len(episodes) != 24 {
		t.Fatalf("episodes = %d, want 24 (27 entries minus 3 trailers)", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" || first.RawID != "317886" {
		t.Errorf("first episode = Num %q RawID %q, want 1/317886", first.Num, first.RawID)
	}
	if first.RawEmbeds == nil || len(first.RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (dubs hydrate per episode)", first.RawEmbeds)
	}
	for _, ep := range episodes {
		if ep.Num == "3" && ep.Title == "Трейлер 3" {
			t.Error("preview episode survived the type filter")
		}
	}
	last := episodes[len(episodes)-1]
	if last.Num != "24" || last.RawID != "367990" {
		t.Errorf("last episode = Num %q RawID %q, want the real (inactive-flagged) ep 24", last.Num, last.RawID)
	}
}

// TestAnime365GetEpisodesBadURL: a URL without a parseable trailing
// series id fails loud with a typed error instead of requesting
// seriesId=0.
func TestAnime365GetEpisodesBadURL(t *testing.T) {
	t.Parallel()

	p, _ := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request must leave the process for an unparseable URL")
	})

	_, err := p.GetEpisodes(context.Background(), "https://example.com/no-id-here")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

// TestAnime365FetchDubs pins the per-episode dub hydration: only
// voice-kind translations become dubs (raw/subs are not audio
// options), the name is the joined authorsList with first-found-wins
// dedup, the embed entry is the API embed PATH resolved later (mirror
// fallback applies at resolve time), and inactive translations are
// dead dubs that never surface.
func TestAnime365FetchDubs(t *testing.T) {
	t.Parallel()

	p, rec := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, fixture(t, "anime365_translations.json"))
	})

	episode := contracts.Episode{Num: "5", RawID: "342089", RawEmbeds: map[string][]string{}}
	if _, err := p.FetchDubs(context.Background(), &episode); err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	if rec.Path != "/api/translations" {
		t.Errorf("request path = %q, want /api/translations", rec.Path)
	}
	if q := rec.Query; !containsAll(q, "episodeId=342089", "limit=500") {
		t.Errorf("query = %q, want episodeId=342089 and limit=500", q)
	}

	want := map[string]string{
		"AniLibria":                "/api/translations/embed/5452308",
		"DubyDuby":                 "/api/translations/embed/5521171",
		"Flarrow Films":            "/api/translations/embed/5452307",
		"Bang Zoom! Entertainment": "/api/translations/embed/5452310",
		"KOMOREBI":                 "/api/translations/embed/5376663",
		"Озвучка":                  "/api/translations/embed/5999999",
	}
	if len(episode.RawEmbeds) != len(want) {
		t.Fatalf("RawEmbeds = %d entries %v, want exactly %d", len(episode.RawEmbeds), keysOf(episode.RawEmbeds), len(want))
	}
	for name, path := range want {
		got := episode.RawEmbeds[name]
		if len(got) != 1 || got[0] != path {
			t.Errorf("RawEmbeds[%q] = %v, want [%s]", name, got, path)
		}
	}
	for junk := range map[string]bool{"Original": true, "AniLibria BD": true, "Архив": true} {
		if _, ok := episode.RawEmbeds[junk]; ok {
			t.Errorf("RawEmbeds carries non-voice or inactive entry %q", junk)
		}
	}
}

// TestAnime365FetchDubsAPIError pins the anime365 error convention:
// errors ride HTTP 200 as {"error":{code,...}} (wrapper + OpenAPI
// ruling, verified live for the embed endpoint) — the body wins over
// the transport status, and a 404 code maps to ErrNotFound.
func TestAnime365FetchDubsAPIError(t *testing.T) {
	t.Parallel()

	p, _ := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, fixture(t, "anime365_error.json")) // {"error":{"code":404}}
	})

	episode := contracts.Episode{Num: "5", RawID: "342089", RawEmbeds: map[string][]string{}}
	_, err := p.FetchDubs(context.Background(), &episode)
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for the HTTP-200 404 error body", err)
	}
}

// TestAnime365ResolveStream pins the tokened embed resolution: the
// access_token rides the query string (OpenAPI accessTokenQuery), the
// stream[] entries become quality-keyed video sources with the URL
// extension as the stream type, and the chosen dub name is kept.
func TestAnime365ResolveStream(t *testing.T) {
	t.Parallel()

	p, rec := testAnime365(t, "sekrit-token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "sekrit-token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Authorization required."}}`))
			return
		}
		anime365JSON(w, fixture(t, "anime365_embed.json"))
	})

	episode := contracts.Episode{
		Num:   "5",
		RawID: "342089",
		RawEmbeds: map[string][]string{
			"AniLibria": {"/api/translations/embed/5452308"},
		},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "AniLibria")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	if rec.Path != "/api/translations/embed/5452308" {
		t.Errorf("request path = %q, want the embed endpoint", rec.Path)
	}
	if stream.DubName != "AniLibria" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	// stream[] wins; first URL of each height; extension → type.
	q720 := stream.Links["720"]
	if q720.URL != "https://smotret-anime.app/stream/5452308/720.m3u8" || q720.Type != "m3u8" {
		t.Errorf("Links[720] = {%q %q}", q720.URL, q720.Type)
	}
	q1080 := stream.Links["1080"]
	if q1080.URL != "https://smotret-anime.app/stream/5452308/1080.m3u8" || q1080.Quality != "1080" {
		t.Errorf("Links[1080] = {%q %q}", q1080.URL, q1080.Quality)
	}
	if len(stream.Links) != 2 {
		t.Errorf("Links = %d entries, want the two stream heights", len(stream.Links))
	}
}

// TestAnime365ResolveStreamDownloadFallback: a translation with an
// empty stream[] resolves through its download[] entries (progressive
// mp4), keyed by the same quality labels.
func TestAnime365ResolveStreamDownloadFallback(t *testing.T) {
	t.Parallel()

	p, _ := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, []byte(`{"data":{"embedUrl":"https://x/embed/1","download":[{"height":720,"url":"https://x/dl/720.mp4"}],"stream":[],"subtitlesUrl":"","subtitlesVttUrl":""}}`))
	})

	episode := contracts.Episode{RawEmbeds: map[string][]string{"Dub": {"/api/translations/embed/1"}}}
	stream, err := p.ResolveStream(context.Background(), episode, "Dub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	got := stream.Links["720"]
	if got.URL != "https://x/dl/720.mp4" || got.Type != "mp4" {
		t.Errorf("Links[720] = {%q %q}, want the download fallback mp4", got.URL, got.Type)
	}
}

// TestAnime365ResolveStreamTokenMissing: the embed data is the ONE
// credential-gated resource of the API (verified live 2026-09-18: the
// tokenless embed answers {"error":{"code":404}}). An empty configured
// token fails loud before any request — kodik parity (PR24).
func TestAnime365ResolveStreamTokenMissing(t *testing.T) {
	t.Parallel()

	p, rec := testAnime365(t, "", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request must leave the process without a token")
	})

	episode := contracts.Episode{RawEmbeds: map[string][]string{"Dub": {"/api/translations/embed/1"}}}
	_, err := p.ResolveStream(context.Background(), episode, "Dub")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || !strings.Contains(perr.Error(), "providers.anime365.token") {
		t.Errorf("error must name the settings key, got %v", err)
	}
	if rec.Path != "" {
		t.Errorf("a request reached %q despite the empty token", rec.Path)
	}
}

// TestAnime365ResolveStreamNoSubscription: the API gates embed data
// behind an account with an active subscription (OpenAPI: 403 = "Не
// выполнен вход или нет активной подписки") — config-flavored typed
// error, kodik 401 parity.
func TestAnime365ResolveStreamNoSubscription(t *testing.T) {
	t.Parallel()

	p, _ := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Authorization required."}}`))
	})

	episode := contracts.Episode{RawEmbeds: map[string][]string{"Dub": {"/api/translations/embed/1"}}}
	_, err := p.ResolveStream(context.Background(), episode, "Dub")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput for the 403 embed gate", err)
	}
}

// TestAnime365UnknownDub: resolving an unhydrated dub key fails with
// ErrNotFound instead of an empty stream.
func TestAnime365UnknownDub(t *testing.T) {
	t.Parallel()

	p, _ := testAnime365(t, "tok", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unknown dub must not trigger a request")
	})

	episode := contracts.Episode{RawEmbeds: map[string][]string{}}
	if _, err := p.ResolveStream(context.Background(), episode, "Ghost"); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAnime365MirrorFallback pins the first-found-wins mirror
// fallback: a transport failure on one mirror moves to the next, and
// an ANSWERING mirror wins even when it reports an API error — the
// wrapper's ruling that domain answers never trigger fallback.
func TestAnime365MirrorFallback(t *testing.T) {
	t.Parallel()

	dead := newDeadListener(t)
	srvErr, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		anime365JSON(w, []byte(`{"error":{"code":404}}`))
	})
	hits := 0
	srvOK, recOK := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		anime365JSON(w, fixture(t, "anime365_search.json"))
	})

	p := newAnime365([]string{
		"http://" + dead.Addr().String(), // refused instantly
		srvErr.URL,                       // answers with an API error
		srvOK.URL,                        // never reached
	}, "tok", testClient(t, "anime365"))

	if _, err := p.Search(context.Background(), "dandadan"); err == nil {
		t.Fatal("the answering mirror's API error must surface, not the fallback")
	}
	if hits != 0 {
		t.Fatalf("fallback mirror was consulted %d times after an ANSWER came in", hits)
	}

	// The dead mirror moves the flow to the next base; the API-error
	// mirror answers and wins there too.
	p2 := newAnime365([]string{
		"http://" + dead.Addr().String(),
		srvOK.URL,
	}, "tok", testClient(t, "anime365"))
	results, err := p2.Search(context.Background(), "dandadan")
	if err != nil {
		t.Fatalf("Search through the dead mirror: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if recOK.Query == "" {
		t.Error("the live mirror must have received the query")
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
