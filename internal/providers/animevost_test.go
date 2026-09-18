package providers

// Fixture provenance: animevost_search.json, animevost_playlist.json,
// animevost_search_miss.json and animevost_playlist_error.json are
// VERBATIM live captures from api.animevost.org taken on 2026-09-18:
//
//	POST /v1/search  name=black%20lagoon            -> 200 (3 entries)
//	POST /v1/playlist id=326                        -> 200 (12 entries)
//	POST /v1/search  name=<phrase with no match>    -> 404 {"error":"Ничего не найдено"}
//	POST /v1/playlist id=999999                     -> 200 {"status":"fail","error":"Тайтл с таким id не найден"}
//
// The 2026-09-18 search envelope is {"state":{...},"data":[...]}; the
// state.count field is stale metadata (it stays 0 while data carries
// hits) and is deliberately not consulted.

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

func TestAnimevostSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_search.json"))
	})
	p := newAnimevost(srv.URL, nil)

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Search is a POST with form field name=<query> (anicli-py
	// animevost.py:20-22), answered by the live 2026-09-18 API.
	if rec.Method != "POST" {
		t.Errorf("request method = %q, want POST", rec.Method)
	}
	if rec.Path != "/search" {
		t.Errorf("request path = %q", rec.Path)
	}
	if got := rec.Form["name"]; len(got) != 1 || got[0] != "black lagoon" {
		t.Errorf("form name = %v, want [black lagoon]", got)
	}
	// The API is transport-fingerprint sensitive (see animevost.go):
	// requests must keep browser-grade headers.
	if rec.Header.Get("User-Agent") == "" {
		t.Error("User-Agent header is empty, want a browser-grade UA")
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	first := results[0]
	if first.Title != "Пираты «Черной лагуны» / Black Lagoon [1-12 из 12]" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.URL != "326" {
		t.Errorf("URL = %q, want str(id) = 326", first.URL)
	}
	if first.SourceID != "animevost" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if !strings.HasPrefix(first.Poster, "https://static.openni.ru/") {
		t.Errorf("Poster = %q, want a static.openni.ru preview", first.Poster)
	}
}

func TestAnimevostSearchMissIsTypedError(t *testing.T) {
	t.Parallel()

	// A phrase with no index match is HTTP 404 + {"error":"Ничего не
	// найдено"} on the live API: an explicit miss, never a silent empty
	// list.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(fixture(t, "animevost_search_miss.json"))
	})
	p := newAnimevost(srv.URL, nil)

	results, err := p.Search(context.Background(), "черная лагуна")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Ничего не найдено") {
		t.Errorf("error = %v, want the server's miss text carried through", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %T, want *contracts.ProviderError", err)
	}
	if perr.Op != contracts.OpSearch || perr.StatusCode != http.StatusNotFound {
		t.Errorf("ProviderError = op %q status %d, want search/404", perr.Op, perr.StatusCode)
	}
	if results != nil {
		t.Errorf("results = %#v, want nil alongside the error", results)
	}
}

func TestAnimevostSearchMalformedJSONIsTypedError(t *testing.T) {
	t.Parallel()

	// The Python original swallowed decode errors (except Exception:
	// return []); the revival surfaces them instead of faking an empty
	// surface.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>not json</html>")
	})
	p := newAnimevost(srv.URL, nil)

	results, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("error = nil, want a typed decode failure")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpSearch {
		t.Fatalf("error = %v, want a ProviderError tagged search", err)
	}
	if results != nil {
		t.Errorf("results = %#v, want nil alongside the error", results)
	}
}

func TestAnimevostSearchProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newAnimevost(srv.URL, nil)

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpSearch {
		t.Fatalf("ProviderError.Op = %v, want %q", perr, contracts.OpSearch)
	}
}

func TestAnimevostGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist.json"))
	})
	p := newAnimevost(srv.URL, nil)

	episodes, err := p.GetEpisodes(context.Background(), "326")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/playlist" {
		t.Errorf("request = %s %s, want POST /playlist", rec.Method, rec.Path)
	}
	if got := rec.Form["id"]; len(got) != 1 || got[0] != "326" {
		t.Errorf("form id = %v, want [326]", got)
	}

	// The live Black Lagoon (id 326) playlist: 12 episodes, every one
	// carrying hd + std on video.animetop.info.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12", len(episodes))
	}
	first := episodes[0]
	if first.Num != "1" || first.RawID != "1" {
		t.Errorf("episode 1 Num/RawID = %q/%q", first.Num, first.RawID)
	}
	if first.Title != "1 серия" {
		t.Errorf("episode 1 Title = %q", first.Title)
	}

	raw := first.RawEmbeds["AnimeVost"]
	if len(raw) != 1 {
		t.Fatalf("RawEmbeds[AnimeVost] = %#v, want one JSON payload", raw)
	}
	var links map[string]string
	if err := json.Unmarshal([]byte(raw[0]), &links); err != nil {
		t.Fatalf("decode links payload: %v", err)
	}
	if links["hd"] != "http://video.animetop.info/720/1927476833.mp4" {
		t.Errorf("hd = %q", links["hd"])
	}
	if links["std"] != "http://video.animetop.info/1927476833.mp4" {
		t.Errorf("std = %q", links["std"])
	}
	last := episodes[11]
	if last.Num != "12" || last.Title != "12 серия" {
		t.Errorf("episode 12 = %q/%q", last.Num, last.Title)
	}
}

func TestAnimevostGetEpisodesFailEnvelopeIsTypedError(t *testing.T) {
	t.Parallel()

	// An unknown id answers HTTP 200 with {"status":"fail","error":
	// "Тайтл с таким id не найден"} on the live API: surfaced as a typed
	// miss, never a silent empty list (the Python original returned []).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist_error.json"))
	})
	p := newAnimevost(srv.URL, nil)

	episodes, err := p.GetEpisodes(context.Background(), "999999")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Тайтл с таким id не найден") {
		t.Errorf("error = %v, want the server's fail text carried through", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpGetEpisodes {
		t.Fatalf("error = %v, want a ProviderError tagged episodes", err)
	}
	if episodes != nil {
		t.Errorf("episodes = %#v, want nil alongside the error", episodes)
	}
}

func TestAnimevostResolveStream(t *testing.T) {
	t.Parallel()

	// ResolveStream is offline (anicli-py animevost.py:66-76).
	p := newAnimevost(AnimeVostBase, nil)
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"AnimeVost": {`{"hd":"http://video.animetop.info/720/1927476833.mp4","std":"http://video.animetop.info/1927476833.mp4"}`},
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
	if hd.URL != "http://video.animetop.info/720/1927476833.mp4" || hd.Quality != "720" || hd.Type != "mp4" {
		t.Errorf("720 source = %+v", hd)
	}
	sd, ok := stream.Links["480"]
	if !ok {
		t.Fatalf("Links missing 480 (std maps to 480): %v", stream.Links)
	}
	if sd.URL != "http://video.animetop.info/1927476833.mp4" || sd.Quality != "480" || sd.Type != "mp4" {
		t.Errorf("480 source = %+v", sd)
	}
}

func TestAnimevostResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	// Python defaults the payload to "{}" for a missing dub, yielding an
	// empty MediaStream (animevost.py:67).
	p := newAnimevost(AnimeVostBase, nil)

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

	p := newAnimevost(AnimeVostBase, nil)
	if p.ID() != "animevost" || p.Name() != "AnimeVost" || p.BaseURL() != AnimeVostBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	// The 2026-09-18 index reliably matches canonical latin names
	// (black lagoon, naruto) while Cyrillic phrases only hit in their
	// exact inflected site-title form; declare the latin routing so the
	// search fan-out sends romaji/english queries.
	if pref := p.NamePreference(); pref != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", pref)
	}
}
