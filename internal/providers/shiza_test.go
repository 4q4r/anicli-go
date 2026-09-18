package providers

// SHIZA Project (PR57) — shape tests against the live-captured GraphQL
// fixtures (testdata/shiza_*.json, captured 2026-09-18 from
// shizaproject.com/graphql with the provider's exact query documents).
// The request-body assertions pin those query documents: a projection
// change that silently drifts from the captured shape must fail here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// shizaGraphQLBody mirrors the JSON request the provider must POST.
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

// shizaSearchQuery is the exact search document the fixtures were
// captured with (2026-09-18). Changing it invalidates the fixtures.
// The canonical constants live in shiza.go.

func TestShizaSearch(t *testing.T) {
	t.Parallel()

	var body *shizaGraphQLBody
	srv, _ := shizaServeGraphQL(t, fixture(t, "shiza_search.json"), &body)
	p := newShiza(srv.URL, testClient(t, "shiza"))

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
	p := newShiza(srv.URL, testClient(t, "shiza"))

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
	p := newShiza(srv.URL, testClient(t, "shiza"))

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
	p := newShiza(srv.URL, testClient(t, "shiza"))

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
	p := newShiza(srv.URL, testClient(t, "shiza"))

	if _, err := p.Search(context.Background(), "черная лагуна"); err == nil {
		t.Fatal("err = nil, want a transport status error")
	}
}

func TestShizaGetEpisodes(t *testing.T) {
	t.Parallel()

	var body *shizaGraphQLBody
	srv, _ := shizaServeGraphQL(t, fixture(t, "shiza_release.json"), &body)
	p := newShiza(srv.URL, testClient(t, "shiza"))

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
	if first.Num != "1" || first.RawID != "1" {
		t.Errorf("Num/RawID = %q/%q, want 1/1", first.Num, first.RawID)
	}
	if first.Title != "Нападение покемонов" {
		t.Errorf("Title = %q", first.Title)
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

	p := newShiza(ShizaBase, testClient(t, "shiza"))

	if _, err := p.GetEpisodes(context.Background(), "https://example.com/anime/xyz"); !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput for a foreign URL", err)
	}
}

func TestShizaGetEpisodesNotFound(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":null}}`), nil)
	p := newShiza(srv.URL, testClient(t, "shiza"))

	if _, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/no-such-slug"); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestShizaGetEpisodesGeoBlocked(t *testing.T) {
	t.Parallel()

	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":{"viewerInBlockedCountry":true,"episodes":[]}}}`), nil)
	p := newShiza(srv.URL, testClient(t, "shiza"))

	if _, err := p.GetEpisodes(context.Background(), srv.URL+"/releases/geo-locked"); !errors.Is(err, contracts.ErrGeoBlocked) {
		t.Fatalf("err = %v, want ErrGeoBlocked", err)
	}
}

func TestShizaGetEpisodesNullNumberSkipped(t *testing.T) {
	t.Parallel()

	// A null episode number is not a consumable episode (the Go side
	// keys episodes by their number string); such entries are skipped
	// instead of surfacing a "None" episode.
	srv, _ := shizaServeGraphQL(t, []byte(`{"data":{"release":{"viewerInBlockedCountry":false,"episodes":[
		{"number":null,"name":"Анонс","videos":[]},
		{"number":1,"name":"Эпизод","videos":[{"embedUrl":"https://kodikplayer.com/uv/1/ab/720p"}]}
	]}}}`), nil)
	p := newShiza(srv.URL, testClient(t, "shiza"))

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

	// The sibnet embed of the fixture shape resolves through the shared
	// extractor factory (anidub pattern): the fake shell page carries
	// "sibnet" so the extractor's substring gate matches locally.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `player = new Playerjs({src: "https://video.sibnet.ru/videos/5228112/ep1.mp4"});`)
	})
	p := newShiza(srv.URL, testClient(t, "shiza"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			shizaDub: {srv.URL + "/sibnet/shell.php?videoid=5228112"},
		},
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

	p := newShiza(ShizaBase, testClient(t, "shiza"))

	stream, err := p.ResolveStream(context.Background(),
		contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty for an unknown dub", stream.Links)
	}
}
