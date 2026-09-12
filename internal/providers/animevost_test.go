package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestAnimevostSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_search.json"))
	})
	p := newAnimevost(srv.URL, testClient(t, "animevost"))

	results, err := p.Search(context.Background(), "bibop")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Search is a POST with form field name=<query> (anicli-py
	// animevost.py:20-22: data={"name": query}; the PR brief's "q=" is a
	// shorthand, the Python param is ported verbatim).
	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/search" {
		t.Errorf("request path = %q", rec.Path)
	}
	if got := rec.Form["name"]; len(got) != 1 || got[0] != "bibop" {
		t.Errorf("form name = %v, want [bibop]", got)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Ковбой Бибоп" {
		t.Errorf("Title = %q", results[0].Title)
	}
	if results[0].URL != "7" {
		t.Errorf("URL = %q, want str(id) = 7", results[0].URL)
	}
	if results[0].SourceID != "animevost" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[1].Poster != "https://cdn.animevost.org/preview/re_zero.jpg" {
		t.Errorf("Poster = %q", results[1].Poster)
	}
}

func TestAnimevostSearchMalformedJSONReturnsEmpty(t *testing.T) {
	t.Parallel()

	// Python swallows decode errors on the search path (animevost.py:23-26:
	// except Exception: return []); the port must match.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>not json</html>")
	})
	p := newAnimevost(srv.URL, testClient(t, "animevost"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search on malformed JSON = %v, want nil (Python returns [])", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAnimevostSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newAnimevost(srv.URL, testClient(t, "animevost"))

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestAnimevostGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist.json"))
	})
	p := newAnimevost(srv.URL, testClient(t, "animevost"))

	episodes, err := p.GetEpisodes(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/playlist" {
		t.Errorf("request = %s %s, want POST /playlist", rec.Method, rec.Path)
	}
	if got := rec.Form["id"]; len(got) != 1 || got[0] != "7" {
		t.Errorf("form id = %v, want [7]", got)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(episodes))
	}
	// 1-based enumerate supplies num and raw_id; missing name falls back
	// to the index string (animevost.py:51-52).
	if episodes[0].Num != "1" || episodes[0].RawID != "1" {
		t.Errorf("episode 1 Num/RawID = %q/%q", episodes[0].Num, episodes[0].RawID)
	}
	if episodes[0].Title != "Серия 1" {
		t.Errorf("episode 1 Title = %q", episodes[0].Title)
	}
	if episodes[1].Title != "2" {
		t.Errorf("episode 2 Title = %q, want index fallback 2", episodes[1].Title)
	}
	if episodes[2].Num != "3" || episodes[2].Title != "Серия 3 (без std)" {
		t.Errorf("episode 3 = %q/%q", episodes[2].Num, episodes[2].Title)
	}

	// The raw embed is a JSON object of hd/std links.
	raw := episodes[0].RawEmbeds["AnimeVost"]
	if len(raw) != 1 {
		t.Fatalf("RawEmbeds[AnimeVost] = %#v, want one JSON payload", raw)
	}
	var links map[string]string
	if err := json.Unmarshal([]byte(raw[0]), &links); err != nil {
		t.Fatalf("decode links payload: %v", err)
	}
	if links["hd"] != "https://video.animevost.org/cowboy_bebop/1_hd.mp4" ||
		links["std"] != "https://video.animevost.org/cowboy_bebop/1_std.mp4" {
		t.Errorf("links payload = %v", links)
	}
	// Truthy check: episode 3 has no std.
	var links3 map[string]string
	if err := json.Unmarshal([]byte(episodes[2].RawEmbeds["AnimeVost"][0]), &links3); err != nil {
		t.Fatalf("decode links payload: %v", err)
	}
	if _, has := links3["std"]; has {
		t.Errorf("episode 3 links = %v, std must be dropped", links3)
	}
}

func TestAnimevostGetEpisodesErrorObjectReturnsEmpty(t *testing.T) {
	t.Parallel()

	// The playlist endpoint answers {"error": ...} for unknown ids
	// (animevost.py:47-48).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"error": "not found"}`)
	})
	p := newAnimevost(srv.URL, testClient(t, "animevost"))

	episodes, err := p.GetEpisodes(context.Background(), "99999")
	if err != nil {
		t.Fatalf("GetEpisodes on error object = %v, want nil", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

func TestAnimevostResolveStream(t *testing.T) {
	t.Parallel()

	// ResolveStream is offline (animevost.py:66-76).
	p := newAnimevost(AnimeVostBase, testClient(t, "animevost"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"AnimeVost": {`{"hd":"https://video.animevost.org/x/1_hd.mp4","std":"https://video.animevost.org/x/1_std.mp4"}`},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "AnimeVost")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "AnimeVost" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("Links = %v, want 2", stream.Links)
	}
	hd, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links missing 720 (hd maps to 720): %v", stream.Links)
	}
	if hd.URL != "https://video.animevost.org/x/1_hd.mp4" || hd.Quality != "720" || hd.Type != "mp4" {
		t.Errorf("720 source = %+v", hd)
	}
	sd, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links missing 480 (std maps to 480): %v", stream.Links)
	}
	if sd.URL != "https://video.animevost.org/x/1_std.mp4" || sd.Quality != "480" || sd.Type != "mp4" {
		t.Errorf("480 source = %+v", sd)
	}
}

func TestAnimevostResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	// Python defaults the payload to "{}" for a missing dub, yielding an
	// empty MediaStream (animevost.py:67).
	p := newAnimevost(AnimeVostBase, testClient(t, "animevost"))

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty", stream.Links)
	}
	if stream.DubName != "NoSuchDub" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestAnimevostProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimevost(AnimeVostBase, testClient(t, "animevost"))
	if p.ID() != "animevost" || p.Name() != "AnimeVost" || p.BaseURL() != AnimeVostBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
