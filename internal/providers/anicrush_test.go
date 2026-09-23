package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// The fixtures these tests ride are RECONSTRUCTED from the reference
// wrapper sources, not live captures — the anicrush.to family was
// origin-dead (Cloudflare 521 for every vantage) through the whole
// implementation window (see anicrush.go's provenance block). The
// assertions below pin the documented wire shapes; nothing here can
// regress against the site until it returns.

func newTestAniCrush(t *testing.T, serverURL string) *AniCrush {
	t.Helper()
	// The fixture server stands in for the API host; the site root
	// stays the production constant (only headers reference it).
	return newAniCrush(serverURL, testClient(t, "anicrush"))
}

func TestAniCrushSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-site") != "anicrush" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(fixture(t, "anicrush_search.json"))
	})
	p := newTestAniCrush(t, srv.URL)

	results, err := p.Search(context.Background(), "one piece")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != "GET" {
		t.Errorf("request method = %q, want GET", rec.Method)
	}
	if rec.Path != "/shared/v2/movie/list" {
		t.Errorf("request path = %q, want /shared/v2/movie/list", rec.Path)
	}
	if rec.Query != "keyword=one+piece&limit=24&page=1" {
		t.Errorf("request query = %q, want the wrapper defaults", rec.Query)
	}
	if rec.Header.Get("x-site") != "anicrush" {
		t.Errorf("x-site header = %q, want anicrush", rec.Header.Get("x-site"))
	}
	if rec.Header.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest", rec.Header.Get("X-Requested-With"))
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 fixture rows", len(results))
	}
	first := results[0]
	if first.Title != "One Piece" {
		t.Errorf("Title = %q, want the english name", first.Title)
	}
	if first.URL != "vRPjMA" {
		t.Errorf("URL = %q, want the bare movie id", first.URL)
	}
	if first.SourceID != "anicrush" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster != anicrushPosterBase+"/posters/one-piece.jpg" {
		t.Errorf("Poster = %q, want the poster CDN prefixed path", first.Poster)
	}
	// name_english is empty on the second fixture row: the title falls
	// back to the romaji name.
	if results[1].Title != "Dandadan" {
		t.Errorf("results[1].Title = %q, want the romaji fallback", results[1].Title)
	}
}

func TestAniCrushSearchAPIRefusalFailsTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status": false, "message": "no keyword"}`))
	})
	p := newTestAniCrush(t, srv.URL)

	_, err := p.Search(context.Background(), "x")
	if err == nil {
		t.Fatal("Search must fail on {status: false}")
	}
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound wrapped", err)
	}
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) || pe.Provider != "anicrush" || pe.Op != contracts.OpSearch {
		t.Errorf("err = %v, want an anicrush search ProviderError", err)
	}
}

func TestAniCrushGetEpisodesFlattensGroups(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anicrush_episodes.json"))
	})
	p := newTestAniCrush(t, srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "vRPjMA")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if rec.Path != "/shared/v2/episode/list" {
		t.Errorf("request path = %q, want /shared/v2/episode/list", rec.Path)
	}
	if rec.Query != "_movieId=vRPjMA" {
		t.Errorf("request query = %q, want _movieId=vRPjMA", rec.Query)
	}

	// The wire result is a record of arrays keyed by an undocumented
	// group id: the fixture spreads five episodes over two groups in
	// scrambled order, and the provider must flatten + sort by number
	// (the mapper's sort). The fractional number formats without a
	// trailing ".0".
	wantNums := []string{"1", "2", "3", "4.5", "5"}
	if len(episodes) != len(wantNums) {
		t.Fatalf("episodes = %d, want %d", len(episodes), len(wantNums))
	}
	for i, num := range wantNums {
		if episodes[i].Num != num {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, num)
		}
		if episodes[i].RawID != "vRPjMA" {
			t.Errorf("episodes[%d].RawID = %q, want the movie id", i, episodes[i].RawID)
		}
	}
	// Empty name_english falls back to the native name.
	if episodes[2].Title != "第三話" {
		t.Errorf("episodes[2].Title = %q, want the name fallback", episodes[2].Title)
	}
	// Episodes hydrate dubs per episode (DubsHydrator), not here.
	for i, ep := range episodes {
		if len(ep.RawEmbeds) != 0 {
			t.Errorf("episodes[%d] carries %d raw embeds, want none (lazy hydration)",
				i, len(ep.RawEmbeds))
		}
	}
}

func TestAniCrushFetchDubs(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anicrush_servers.json"))
	})
	p := newTestAniCrush(t, srv.URL)

	episode := &contracts.Episode{
		Num:       "1",
		RawID:     "vRPjMA",
		RawEmbeds: map[string][]string{},
	}
	hydrated, err := p.FetchDubs(context.Background(), episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	if rec.Path != "/shared/v2/episode/servers" {
		t.Errorf("request path = %q, want /shared/v2/episode/servers", rec.Path)
	}
	if rec.Query != "_movieId=vRPjMA&ep=1" {
		t.Errorf("request query = %q, want _movieId=vRPjMA&ep=1", rec.Query)
	}

	// The fixture carries two sub servers (Megacloud=4, Southcloud=1)
	// and one dub row (Megacloud=4); the audio group prefixes the key
	// so the same server name survives on both sides.
	wantKeys := []string{"sub · Megacloud", "sub · Southcloud", "dub · Megacloud"}
	if len(hydrated.RawEmbeds) != len(wantKeys) {
		t.Fatalf("RawEmbeds keys = %v, want %v", hydrated.RawEmbeds, wantKeys)
	}
	for _, key := range wantKeys {
		links, ok := hydrated.RawEmbeds[key]
		if !ok || len(links) != 1 {
			t.Fatalf("RawEmbeds[%q] missing (have %v)", key, hydrated.RawEmbeds)
		}
		audio := "dub"
		if strings.HasPrefix(key, "sub") {
			audio = "sub"
		}
		sv := "4"
		if strings.Contains(key, "Southcloud") {
			sv = "1"
		}
		// url.Values.Encode sorts alphabetically (_movieId first).
		want := srv.URL + "/shared/v2/episode/sources?_movieId=vRPjMA&ep=1&sc=" + audio + "&sv=" + sv
		if links[0] != want {
			t.Errorf("RawEmbeds[%q][0] = %q, want %q", key, links[0], want)
		}
	}
}

func TestAniCrushResolveStreamUnresolvableEmbedFailsTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anicrush_sources.json"))
	})
	p := newTestAniCrush(t, srv.URL)

	episode := contracts.Episode{
		Num:   "1",
		RawID: "vRPjMA",
		RawEmbeds: map[string][]string{
			"sub · Megacloud": {srv.URL + "/shared/v2/episode/sources?ep=1&sc=sub&sv=4&_movieId=vRPjMA"},
		},
	}
	_, err := p.ResolveStream(context.Background(), episode, "sub · Megacloud")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed wrapped", err)
	}
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) || pe.Op != contracts.OpResolveStream {
		t.Errorf("err = %v, want a resolve_stream ProviderError", err)
	}
}

func TestAniCrushResolveStreamDirectMediaLink(t *testing.T) {
	t.Parallel()

	// The handler needs the server URL before fixtureServer returns,
	// so it reads through this variable (set before the first request
	// flies — ResolveStream runs after the assignment below).
	var srvURL string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shared/v2/episode/sources" {
			_, _ = w.Write([]byte(`{"status": true, "result": {"type": "file", "link": "` +
				srvURL + `/media/episode.m3u8", "server": 4}}`))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
	})
	srvURL = srv.URL
	p := newTestAniCrush(t, srv.URL)

	episode := contracts.Episode{
		Num:   "1",
		RawID: "vRPjMA",
		RawEmbeds: map[string][]string{
			"sub · Megacloud": {srv.URL + "/shared/v2/episode/sources?ep=1&sc=sub&sv=4&_movieId=vRPjMA"},
		},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "sub · Megacloud")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	link, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want the direct 720 fallback", stream.Links)
	}
	if link.URL != srv.URL+"/media/episode.m3u8" || link.Quality != "720" {
		t.Errorf("link = %+v, want the raw m3u8 as 720", link)
	}
}

func TestAniCrushResolveStreamLazilyHydratesDubs(t *testing.T) {
	t.Parallel()

	// The sources fixture's embed link is a megacloud player (not
	// resolvable by the registered extractors — see the provider
	// provenance), so this end-to-end proof answers with a direct
	// media link: hydration must happen BEFORE the sources fetch.
	sourcesServed := false
	var srvURL string
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shared/v2/episode/servers":
			_, _ = w.Write(fixture(t, "anicrush_servers.json"))
		case "/shared/v2/episode/sources":
			sourcesServed = true
			_, _ = w.Write([]byte(`{"status": true, "result": {"type": "file", "link": "` +
				srvURL + `/media/episode.m3u8", "server": 4}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	srvURL = srv.URL
	p := newTestAniCrush(t, srv.URL)

	// Empty RawEmbeds: ResolveStream must hydrate through FetchDubs
	// (the kaa lazy pattern) before resolving.
	episode := contracts.Episode{Num: "1", RawID: "vRPjMA"}
	stream, err := p.ResolveStream(context.Background(), episode, "sub · Megacloud")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if !sourcesServed {
		t.Error("the sources endpoint was never fetched: hydration skipped")
	}
	if stream.DubName != "sub · Megacloud" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	if link, ok := stream.Links["720"]; !ok || link.URL != srvURL+"/media/episode.m3u8" {
		t.Errorf("Links = %v, want the hydrated 720 m3u8", stream.Links)
	}
	if rec.Query != "_movieId=vRPjMA&ep=1&sc=sub&sv=4" {
		t.Errorf("sources query = %q, want the hydrated server row's query", rec.Query)
	}
}

func TestAniCrushResolveStreamUnknownDubFailsTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shared/v2/episode/servers" {
			_, _ = w.Write(fixture(t, "anicrush_servers.json"))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
	})
	p := newTestAniCrush(t, srv.URL)

	episode := contracts.Episode{Num: "1", RawID: "vRPjMA"}
	_, err := p.ResolveStream(context.Background(), episode, "dub ·不存在")
	if err == nil || !strings.Contains(err.Error(), "carries no mirrors") {
		t.Fatalf("err = %v, want the no-mirrors failure", err)
	}
}

func TestAniCrushNamePreference(t *testing.T) {
	t.Parallel()

	p := newTestAniCrush(t, "http://127.0.0.1:1")
	if p.NamePreference() != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", p.NamePreference())
	}
}

func TestAniCrushSmokeQuery(t *testing.T) {
	t.Parallel()

	p := newTestAniCrush(t, "http://127.0.0.1:1")
	if p.SmokeQuery() == "" {
		t.Error("SmokeQuery must not be empty (the smoke would fall back)")
	}
}
