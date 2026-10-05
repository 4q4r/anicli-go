package providers

// SHIZA Project (PR57) runs as the BUNDLED LUA SCRIPT (PR125:
// internal/luaproviders/scripts/shiza/main.lua) — these tests pin the
// script through the same contracts.Provider surface and the same
// live-captured GraphQL fixtures (testdata/shiza_*.json, captured
// 2026-09-18 from shizaproject.com/graphql with the provider's exact
// query documents) the compiled Go implementation was held to.
//
// The fresh-sandbox state contract adapts one pin: streams(raw_id,
// dub) receives only RawID, so raw_id carries the {s, n} state JSON
// (release slug + episode number) and the resolve leg re-fetches the
// release detail before extracting (the animedia/anitokyo
// precedent). The request-body assertions pin the two GraphQL query
// documents verbatim: a projection change that silently drifts from
// the captured shape must fail here.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// shizaSearchQuery is the exact search document the fixtures were
// captured with (2026-09-18). Changing it invalidates the fixtures.
// The script carries the same literal; the test owns the canonical
// text since the compiled provider is gone.
const shizaSearchQuery = `query fetchReleases($first: Int, $query: String) { releases(first: $first, query: $query) { edges { node { slug name posters { preview: resize(width: 360, height: 500) { url } } } } } }`

// shizaReleaseQuery is the exact release-detail document the fixtures
// were captured with (2026-09-18).
const shizaReleaseQuery = `query fetchRelease($slug: String!) { release(slug: $slug) { viewerInBlockedCountry episodes { number name videos { embedUrl } } } }`

// shizaDub is the single dub key of the provider: a release is one
// team's dub, whichever embed host carries it. The script's DUB
// literal must stay identical.
const shizaDub = "SHIZA Project"

// shizaGraphQLBody mirrors the JSON request the script must POST.
type shizaGraphQLBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// shizaServeGraphQL serves one canned GraphQL response body on a test
// server whose handler records the parsed request body into want.
func shizaServeGraphQL(t *testing.T, response []byte, body **shizaGraphQLBody) (*httptest.Server, string) {
	t.Helper()
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read graphql request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var parsed shizaGraphQLBody
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Errorf("decode graphql request body %q: %v", raw, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body != nil {
			*body = &parsed
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	})
	return srv, srv.URL
}

// shizaStateJSON builds the {e} state JSON the script encodes into
// raw_id (the fresh-sandbox streams() state carrier: the episode's
// embed list — the sandbox's only channel for the data the compiled
// provider read from its in-memory RawEmbeds).
func shizaStateJSON(embeds []string) (string, error) {
	b, err := json.Marshal(map[string][]string{"e": embeds})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestShizaMeta pins the service-level identity: registration
// identity, the site root the roster renders, source type, the RU
// content language and the declared live probe.
func TestShizaMeta(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "shiza")
	if p.ID() != "shiza" || p.Name() != shizaDub {
		t.Errorf("identity = %q/%q, want shiza/%q", p.ID(), p.Name(), shizaDub)
	}
	if p.BaseURL() != "https://shizaproject.com" {
		t.Errorf("BaseURL = %q, want https://shizaproject.com", p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if lc := p.(interface{ ContentLanguage() string }); lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", lc.ContentLanguage())
	}
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("shiza lost the SmokeQueryProvider surface")
	}
	if got := sq.SmokeQuery(); got != "черная лагуна" {
		t.Errorf("SmokeQuery = %q, want черная лагуна (the live-matrix probe)", got)
	}
}

func TestShizaSearch(t *testing.T) {
	t.Parallel()

	var body *shizaGraphQLBody
	srv, _ := shizaServeGraphQL(t, fixture(t, "shiza_search.json"), &body)
	p := luaProvider(t, "shiza", srv.URL)

	results, err := p.Search(context.Background(), "черная лагуна")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The fixture is a verbatim live capture (shizaproject.com/graphql,
	// query "черная лагуна", 2026-09-18): two Black Lagoon releases.
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (fixture shiza_search.json)", len(results))
	}
	first := results[0]
	if first.Title != "Пираты «Черной лагуны»" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != srv.URL+"/releases/black-lagoon-tv-1" {
		t.Errorf("URL = %q, want the release page URL", first.URL)
	}
	if first.SourceID != "shiza" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if !strings.HasPrefix(first.Poster, "https://cdn.shizaproject.com/resize/") {
		t.Errorf("Poster = %q, want the cdn resize URL", first.Poster)
	}

	// The request must be the pinned GraphQL document with the query
	// routed into the variables (fixture fidelity pin).
	if body == nil {
		t.Fatal("no graphql request body recorded")
	}
	if body.Query != shizaSearchQuery {
		t.Errorf("Query = %q, want the captured search document", body.Query)
	}
	if body.Variables["query"] != "черная лагуна" {
		t.Errorf("Variables[query] = %v, want the user query", body.Variables["query"])
	}
	if _, ok := body.Variables["first"]; !ok {
		t.Errorf("Variables[first] missing, want the page size")
	}
}

func TestShizaSearchEmptyQueryRejected(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"releases":{"edges":[]}}}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	results, err := p.Search(context.Background(), "   ")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
}

func TestShizaSearchNoResults(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"releases":{"edges":[]}}}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	results, err := p.Search(context.Background(), "несуществующее")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestShizaSearchGraphQLError(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"errors":[{"message":"boom"}]}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	results, err := p.Search(context.Background(), "черная лагуна")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want a graphql error carrying the message", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
}

func TestShizaSearchHTTPStatus(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := luaProvider(t, "shiza", srv.URL)

	if _, err := p.Search(context.Background(), "черная лагуна"); err == nil {
		t.Fatal("err = nil, want a transport status error")
	}
}

func TestShizaGetEpisodes(t *testing.T) {
	t.Parallel()

	var body *shizaGraphQLBody
	srv, _ := shizaServeGraphQL(t, fixture(t, "shiza_release.json"), &body)
	p := luaProvider(t, "shiza", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/neon-genesis-evangelion-tv")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// Live capture (2026-09-18): the TV series ships 26 episodes, each
	// carrying the SHIZA kodik dub embed plus the sibnet mirror.
	if len(episodes) != 26 {
		t.Fatalf("episodes = %d, want 26 (fixture shiza_release.json)", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" {
		t.Errorf("Num = %q, want 1", first.Num)
	}
	if first.Title != "Нападение покемонов" {
		t.Errorf("Title = %q", first.Title)
	}
	// The raw_id state carrier must round-trip as JSON the streams
	// call can decode: the episode's embed list rides WITH the id
	// (the sandbox's only state channel — the compiled provider's
	// ResolveStream read the same list from memory, making zero
	// shiza requests per resolve; the site tarpits the 4th GraphQL
	// POST inside a minute window, so the resolve leg must not
	// re-fetch).
	if !strings.Contains(first.RawID, `"e":`) || !strings.Contains(first.RawID, "kodikplayer.com") {
		t.Errorf("RawID = %q, want the {e} embed-list state JSON", first.RawID)
	}
	embeds := first.RawEmbeds[shizaDub]
	if len(embeds) != 2 {
		t.Fatalf("RawEmbeds[%s] = %v, want the kodik + sibnet pair", shizaDub, embeds)
	}
	if !strings.HasPrefix(embeds[0], "https://kodikplayer.com/") {
		t.Errorf("embed[0] = %q, want the kodik player URL", embeds[0])
	}
	if !strings.HasPrefix(embeds[1], "https://video.sibnet.ru/shell.php") {
		t.Errorf("embed[1] = %q, want the sibnet shell URL", embeds[1])
	}

	if body == nil {
		t.Fatal("no graphql request body recorded")
	}
	if body.Query != shizaReleaseQuery {
		t.Errorf("Query = %q, want the captured release document", body.Query)
	}
	if body.Variables["slug"] != "neon-genesis-evangelion-tv" {
		t.Errorf("Variables[slug] = %v", body.Variables["slug"])
	}
}

func TestShizaGetEpisodesBadURL(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "shiza")

	if _, err := p.GetEpisodes(context.Background(), "https://example.com/anime/xyz"); !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput for a foreign URL", err)
	}
}

func TestShizaGetEpisodesNotFound(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":null}}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	if _, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/no-such-slug"); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestShizaGetEpisodesGeoBlocked(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":{"viewerInBlockedCountry":true,"episodes":[]}}}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	if _, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/geo-locked"); !errors.Is(err, contracts.ErrGeoBlocked) {
		t.Fatalf("err = %v, want ErrGeoBlocked", err)
	}
}

func TestShizaGetEpisodesNullNumberSkipped(t *testing.T) {
	t.Parallel()

	// A null episode number is not a consumable episode (the episode
	// keys off its number); such entries are skipped instead of
	// surfacing a null-keyed episode.
	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":{"viewerInBlockedCountry":false,"episodes":[
		{"number":null,"name":"Анонс","videos":[]},
		{"number":1,"name":"Эпизод","videos":[{"embedUrl":"https://kodikplayer.com/uv/1/ab/720p"}]}
	]}}}`), nil)
	p := luaProvider(t, "shiza", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/some-slug")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want only the numbered one", episodes)
	}
}

func TestShizaResolveStream(t *testing.T) {
	t.Parallel()

	// The streams() leg resolves the embed list carried in the raw_id
	// state through the shared extractor factory — NO release
	// re-fetch (the compiled provider's ResolveStream read its
	// in-memory RawEmbeds without a shiza request; the state JSON is
	// the sandbox translation of exactly that). The fake shell page
	// carries "sibnet" so the extractor's substring gate matches
	// locally.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`player = new Playerjs({src: "https://video.sibnet.ru/videos/5228112/ep1.mp4"});`))
	})
	p := luaProvider(t, "shiza", srv.URL)
	shellURL := srv.URL + "/sibnet/shell.php?videoid=5228112"
	rawID, err := shizaStateJSON([]string{shellURL})
	if err != nil {
		t.Fatalf("state json: %v", err)
	}
	episode := contracts.Episode{
		Num:       "1",
		RawID:     rawID,
		RawEmbeds: map[string][]string{},
	}

	stream, err := p.ResolveStream(context.Background(), episode, shizaDub)
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links = %v, want a 480 sibnet entry", stream.Links)
	}
	if src.URL != "https://video.sibnet.ru/videos/5228112/ep1.mp4" {
		t.Errorf("URL = %q, want the extracted sibnet mp4", src.URL)
	}
	if stream.DubName != shizaDub {
		t.Errorf("DubName = %q, want %q", stream.DubName, shizaDub)
	}
}

func TestShizaResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "shiza")
	rawID, err := shizaStateJSON([]string{"https://kodikplayer.com/uv/1/ab/720p"})
	if err != nil {
		t.Fatalf("state json: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(),
		contracts.Episode{RawID: rawID, RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for an unknown dub", stream.Links)
	}
}
