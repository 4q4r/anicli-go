package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestAnimePaheSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animepahe_search.json"))
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	results, err := p.Search(context.Background(), "spirited away")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/api" {
		t.Errorf("request path = %q", rec.Path)
	}
	got, err := url.ParseQuery(rec.Query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	// ParseQuery decodes the form-style "+" back to a space; the raw
	// query above carries the encoding (requests sends the same shape).
	if got.Get("m") != "search" || got.Get("q") != "spirited away" {
		t.Errorf("query = %q, want m=search q=\"spirited away\"", rec.Query)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Sen to Chihiro no Kamikakushi" {
		t.Errorf("Title = %q", results[0].Title)
	}
	// The session id doubles as the result URL (animepahe.py:40).
	if results[0].URL != "e3f9a1c2d4b5" {
		t.Errorf("URL = %q, want the session id", results[0].URL)
	}
	if results[0].Poster != "https://i.animepahe.example/successors/cover.jpg" {
		t.Errorf("Poster = %q", results[0].Poster)
	}
}

func TestAnimePaheSearchSendsRefererAndUA(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data": []}`)
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := rec.Header.Get("Referer"); got != srv.URL {
		t.Errorf("Referer = %q, want the site root (animepahe.py:24)", got)
	}
	if got := rec.Header.Get("User-Agent"); got == "" {
		t.Error("User-Agent = empty, want the netclient default (Python re-asserts it)")
	}
}

// Python wraps http.get and json.loads of search in one except-block and
// returns [] (animepahe.py:31-46): transport and decode failures both
// surface as an empty result set.
func TestAnimePaheSearchSilentEmptyOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		p := newAnimePahe("http://"+newDeadListener(t).Addr().String(), testClient(t, "animepahe"))
		results, err := p.Search(context.Background(), "q")
		if err != nil {
			t.Fatalf("Search: %v, want silent empty", err)
		}
		if len(results) != 0 {
			t.Errorf("results = %d, want 0", len(results))
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "not json at all")
		})
		p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
		results, err := p.Search(context.Background(), "q")
		if err != nil {
			t.Fatalf("Search: %v, want silent empty", err)
		}
		if len(results) != 0 {
			t.Errorf("results = %d, want 0", len(results))
		}
	})
}

func TestAnimePaheGetEpisodesPagination(t *testing.T) {
	t.Parallel()

	var pages []string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			pages = append(pages, "2")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "animepahe_episodes_p2.json"))
			return
		}
		pages = append(pages, "1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animepahe_episodes_p1.json"))
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	episodes, err := p.GetEpisodes(context.Background(), "e3f9a1c2d4b5")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("fetched pages = %v, want [1 2] (last_page=2)", pages)
	}
	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3 across both pages", len(episodes))
	}
	// First page order preserved, second page appended (animepahe.py:77-87).
	wantNums := []string{"1", "2", "2.5"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
		if episodes[i].Title != "Episode "+want {
			t.Errorf("episodes[%d].Title = %q", i, episodes[i].Title)
		}
	}
	// raw_id is anime_session|episode_session (animepahe.py:85).
	if episodes[2].RawID != "e3f9a1c2d4b5|f00dcafe" {
		t.Errorf("RawID = %q", episodes[2].RawID)
	}
	// Divergence (task ruling): Python stored raw_embeds={} and derived
	// the play URL inside resolve_stream; the Go contract enumerates dubs
	// from RawEmbeds keys, so GetEpisodes stashes the deterministic play
	// URL under the fixed dub name resolve_stream would have used.
	raw := episodes[2].RawEmbeds["Original (Pahe)"]
	if len(raw) != 1 || raw[0] != srv.URL+"/play/e3f9a1c2d4b5/f00dcafe" {
		t.Errorf("RawEmbeds = %v, want the play URL under the fixed dub", raw)
	}
}

// The live site bounces between the .ru and .su domains; the client must
// follow the redirect chain and parse the final 200 (PR5 intel,
// 2026-09-12). Python relies on requests doing the same.
func TestAnimePaheSearchFollowsRedirects(t *testing.T) {
	t.Parallel()

	final, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data": [{"title": "Redirected", "session": "s1", "poster": "p.jpg"}]}`)
	})
	redirector, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Both endpoints are throwaway test fixtures; the redirect target
		// is the test's own second server, not user input.
		http.Redirect(w, r, final.URL+r.URL.RequestURI(), http.StatusFound) //nolint:gosec // test-only redirect chain
	})
	p := newAnimePahe(redirector.URL, testClient(t, "animepahe"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Redirected" {
		t.Fatalf("results = %+v, want the final-server payload", results)
	}
}

func TestAnimePaheResolveStreamKwikLinks(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "animepahe_play.html"))
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "e3f9a1c2d4b5|abc123",
		RawEmbeds: map[string][]string{
			"Original (Pahe)": {srv.URL + "/play/e3f9a1c2d4b5/abc123"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	// Kwik is explicitly disabled in the Python ExtractorFactory
	// (extractors.py:664-665), so kwik.cx embed URLs resolve to nothing:
	// Python returns an empty MediaStream, not an error.
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty (kwik extractor disabled upstream)", stream.Links)
	}
	if stream.DubName != "Original (Pahe)" {
		t.Errorf("DubName = %q, want the fixed dub name (animepahe.py:150)", stream.DubName)
	}
}

// A dropdown href that IS direct media resolves through the factory
// fallback even while kwik embeds stay unresolved (animepahe.py:142-144).
func TestAnimePaheResolveStreamDirectMedia(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<a href="https://cdn.animepahe.example/file.mp4" class="dropdown-item" target="_blank">720p</a>`)
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
	episode := contracts.Episode{
		RawID:     "s|e",
		RawEmbeds: map[string][]string{"Original (Pahe)": {srv.URL + "/play/s/e"}},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry for the direct mp4", stream.Links)
	}
	if src.URL != "https://cdn.animepahe.example/file.mp4" {
		t.Errorf("URL = %q", src.URL)
	}
}

func TestAnimePaheProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimePahe(AnimePaheBase, testClient(t, "animepahe"))
	if p.ID() != "animepahe" || p.Name() != "AnimePahe" || p.BaseURL() != AnimePaheBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeVideo {
		t.Errorf("SourceType = %q, want video", p.SourceType())
	}
}
