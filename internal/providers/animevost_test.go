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
//
// PR119: the provider runs as the BUNDLED LUA SCRIPT
// (internal/luaproviders/scripts/animevost/main.lua) — these tests pin
// the script through the same contracts.Provider surface and the same
// fixtures the compiled Go implementation was held to. Contract shifts
// forced by the fresh-sandbox Lua adapter (the animedia/anitokyo
// precedent), documented here rather than hidden:
//
//   - the hd/std links payload rides in episode RawID (the only state
//     channel into the per-invocation streams(raw_id, dub) call — the
//     compiled Go provider read it from RawEmbeds, which the adapter
//     does not pass), and RawEmbeds keeps carrying the same payload
//     for consumers;
//   - the HTTP 404 search miss keeps its typed ErrNotFound class
//     through the transport marker, but the server's miss TEXT is not
//     recoverable (netclient drops error bodies before the SDK sees
//     them) — the 200 fail-envelope keeps the full text.

import (
	"context"
	"encoding/json"
	"errors"
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
	p := luaProvider(t, "animevost", srv.URL)

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
	// The fronting is fingerprint-sensitive: requests keep a
	// browser-grade UA (the netclient profile default).
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
	// str(id) semantics: the numeric id stringified (pythonStr parity —
	// a missing id would surface as "None").
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
	// list. The typed class survives the transport marker; the miss
	// text does not (netclient drops error bodies — see the file
	// header).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(fixture(t, "animevost_search_miss.json"))
	})
	p := luaProvider(t, "animevost", srv.URL)

	results, err := p.Search(context.Background(), "черная лагуна")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %T, want *contracts.ProviderError", err)
	}
	if perr.Op != contracts.OpSearch {
		t.Errorf("ProviderError = op %q, want search", perr.Op)
	}
	if results != nil {
		t.Errorf("results = %#v, want nil alongside the error", results)
	}
}

func TestAnimevostSearchMalformedJSONIsTypedError(t *testing.T) {
	t.Parallel()

	// The Python original swallowed decode errors (except Exception:
	// return []); the port surfaces them instead of faking an empty
	// surface.
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	})
	p := luaProvider(t, "animevost", srv.URL)

	results, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("error = nil, want a typed decode failure")
	}
	if !strings.Contains(err.Error(), "decode search response") {
		t.Errorf("error = %v, want the decode-failure context", err)
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
	p := luaProvider(t, "animevost", srv.URL)

	_, err := p.Search(context.Background(), "q")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpSearch {
		t.Fatalf("ProviderError.Op = %v, want %q", err, contracts.OpSearch)
	}
}

func TestAnimevostGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist.json"))
	})
	p := luaProvider(t, "animevost", srv.URL)

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
	if first.Num != "1" {
		t.Errorf("episode 1 Num = %q", first.Num)
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
	// The Lua state contract: RawID carries the SAME payload so the
	// fresh-sandbox streams(raw_id, dub) call resolves offline.
	if first.RawID != raw[0] {
		t.Errorf("RawID = %q, want the links payload %q", first.RawID, raw[0])
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
	// miss with the server's text, never a silent empty list (the
	// Python original returned []).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist_error.json"))
	})
	p := luaProvider(t, "animevost", srv.URL)

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

	// The full offline resolve round trip: episodes() produced the
	// raw_id state on the fixture above, streams(raw_id, dub) decodes
	// it with no network leg (anicli-py animevost.py:66-76 parity).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animevost_playlist.json"))
	})
	p := luaProvider(t, "animevost", srv.URL)

	episodes, err := p.GetEpisodes(context.Background(), "326")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), episodes[0], "AnimeVost")
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

func TestAnimevostResolveStreamEmptyPayloadIsEmpty(t *testing.T) {
	t.Parallel()

	// Python defaults the payload to "{}", yielding an empty
	// MediaStream (animevost.py:67); the dub is echoed but never
	// selects content (the single fixed AnimeVost dub carries both
	// qualities).
	p := luaProvider(t, "animevost", productionBase(t, "animevost"))

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{RawID: "{}"}, "NoSuchDub")
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

	p := luaProviderAtProduction(t, "animevost")
	if p.ID() != "animevost" || p.Name() != "AnimeVost" || p.BaseURL() != "https://api.animevost.org/v1" {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok || lc.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %v, want ru", lc)
	}
}

// TestAnimevostNamePreferenceLatin pins the search routing (PR42
// semantics): the 2026-09-18 index reliably matches canonical latin
// names (black lagoon, naruto) while Cyrillic phrases only hit in their
// exact inflected site-title form — the fan-out must send the
// romaji/english variant.
func TestAnimevostNamePreferenceLatin(t *testing.T) {
	t.Parallel()

	p := luaProviderAtProduction(t, "animevost")
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the capability adapter must stay assertions-stable")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
}

// productionBase returns the pinned production base_url literal of a
// bundled script (the meta tests resolve against the real domain).
func productionBase(t testing.TB, id string) string {
	t.Helper()
	bases, known := luaProductionBases[id]
	if !known {
		t.Fatalf("no production base pinned for lua script %q", id)
	}
	return bases[0]
}
