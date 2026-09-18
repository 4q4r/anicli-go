package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// clampReleaseLastPage decodes a release-page capture, pins last_page to
// bound and re-encodes it: the verbatim One Piece capture carries the
// live last_page=40, and the pagination test wants a two-page walk. The
// map round-trip keeps every other field verbatim (episode literals
// survive: integers and one-decimal fractions are float64-exact).
func clampReleaseLastPage(body []byte, bound int) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	doc["last_page"] = bound
	return json.Marshal(doc)
}

// The fixtures are real captures of animepahe.pw behind its Cloudflare
// challenge (cleared 2026-09-18, see testdata/README.md): the provider
// itself only needs the netclient to replay a solved clearance.

func TestAnimePaheSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animepahe_search.json"))
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	results, err := p.Search(context.Background(), "black lagoon")
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
	if got.Get("m") != "search" || got.Get("q") != "black lagoon" {
		t.Errorf("query = %q, want m=search q=\"black lagoon\"", rec.Query)
	}

	// Real capture: the "black lagoon" search returns 8 results on page
	// one (per_page=8; the provider consumes page one only, like the
	// Python original).
	if len(results) != 8 {
		t.Fatalf("results = %d, want 8", len(results))
	}
	if results[0].Title != "Black Lagoon" {
		t.Errorf("Title = %q", results[0].Title)
	}
	// The session id doubles as the result URL (animepahe.py:40).
	if results[0].URL != "f903fca6-42ca-c7f2-d631-0dc0f1605ba5" {
		t.Errorf("URL = %q, want the session id", results[0].URL)
	}
	if results[0].Poster != "https://i.animepahe.pw/uploads/posters/dec/dec28adff13eb5321b68f215758a236b93e1a448b94991f29ffdcb1a08a97d58.webp" {
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

// PR49 revival ruling: the Python-parity silent-empty quirk
// (transport/decode failures returning nil, nil) is retired. A dead
// API must surface as a typed provider error — the silent empty set is
// what hid the domain death that killed this provider.
func TestAnimePaheSearchTypedErrors(t *testing.T) {
	t.Parallel()

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		p := newAnimePahe("http://"+newDeadListener(t).Addr().String(), testClient(t, "animepahe"))
		results, err := p.Search(context.Background(), "q")
		if err == nil {
			t.Fatal("Search err = nil, want the transport error")
		}
		if results != nil {
			t.Errorf("results = %v, want nil", results)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "not json at all")
		})
		p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
		results, err := p.Search(context.Background(), "q")
		if err == nil {
			t.Fatal("Search err = nil, want a decode error")
		}
		if results != nil {
			t.Errorf("results = %v, want nil", results)
		}
		var pe *contracts.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a *contracts.ProviderError", err)
		}
		if pe.Provider != "animepahe" || pe.Op != contracts.OpSearch {
			t.Errorf("ProviderError = %+v, want provider=animepahe op=search", pe)
		}
	})
}

func TestAnimePaheGetEpisodesPagination(t *testing.T) {
	t.Parallel()

	// Real captures: One Piece (1178 episodes, live last_page=40). The
	// walk bound is read from page one's last_page, so page one is
	// clamped to 2 in-test (all other fields ride along verbatim) and
	// page two is served verbatim; the pagination assertion stays two
	// round trips.
	p1, err := clampReleaseLastPage(fixture(t, "animepahe_episodes_p1.json"), 2)
	if err != nil {
		t.Fatalf("clamp p1 last_page: %v", err)
	}

	var pages []string
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "1" {
			pages = append(pages, "1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(p1)
			return
		}
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "animepahe_episodes_p2.json"))
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	const animeSession = "76d59a16-e57d-4ad1-7ec6-e88f0fe9469b"
	episodes, err := p.GetEpisodes(context.Background(), animeSession)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("fetched pages = %v, want [1 2]", pages)
	}
	// 30 (p1) + 31 (p2: 30 verbatim + 1 modeled fractional entry, see
	// testdata/README.md).
	if len(episodes) != 61 {
		t.Fatalf("episodes = %d, want 61 across both pages", len(episodes))
	}
	// First page order preserved, second page appended (animepahe.py:77-87).
	if episodes[0].Num != "1" || episodes[29].Num != "30" || episodes[30].Num != "31" {
		t.Errorf("boundary nums = %q/%q/%q, want 1/30/31",
			episodes[0].Num, episodes[29].Num, episodes[30].Num)
	}
	// raw_id is anime_session|episode_session (animepahe.py:85).
	if episodes[30].RawID != animeSession+"|f021277d5003719c0b486a84d11b04eeb5a61c009a8a61c037fca94a6a353fdf" {
		t.Errorf("RawID = %q", episodes[30].RawID)
	}
	// The modeled fractional entry pins the wire-literal rendering
	// (pythonStr keeps "2.5" verbatim, like Python str(2.5)).
	last := episodes[len(episodes)-1]
	if last.Num != "2.5" || last.Title != "Episode 2.5" {
		t.Errorf("fractional episode = %q/%q, want 2.5/Episode 2.5", last.Num, last.Title)
	}
	// Divergence (task ruling): Python stored raw_embeds={} and derived
	// the play URL inside resolve_stream; the Go contract enumerates dubs
	// from RawEmbeds keys, so GetEpisodes stashes the deterministic play
	// URL under the fixed dub name resolve_stream would have used.
	raw := last.RawEmbeds["Original (Pahe)"]
	want := srv.URL + "/play/" + animeSession + "/0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"
	if len(raw) != 1 || raw[0] != want {
		t.Errorf("RawEmbeds = %v, want the play URL %q under the fixed dub", raw, want)
	}
}

// A last_page=1 listing answers in one request and never enters the
// pagination loop (real Black Lagoon BD capture shape).
func TestAnimePaheGetEpisodesSinglePage(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"total":1,"per_page":30,"current_page":1,"last_page":1,"data":[`+
			`{"id":8370,"episode":1,"episode2":0,"session":"3b736dd0beb4caf0b1b28e9937755f10eb3626c5b6b107ccb45602f2463f697f"}]}`)
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))

	episodes, err := p.GetEpisodes(context.Background(), "f903fca6-42ca-c7f2-d631-0dc0f1605ba5")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("requests = %d, want 1 (last_page=1)", hits.Load())
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want one episode 1", episodes)
	}
}

func TestAnimePaheGetEpisodesTypedErrors(t *testing.T) {
	t.Parallel()

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		p := newAnimePahe("http://"+newDeadListener(t).Addr().String(), testClient(t, "animepahe"))
		episodes, err := p.GetEpisodes(context.Background(), "s")
		if err == nil {
			t.Fatal("GetEpisodes err = nil, want the transport error")
		}
		if episodes != nil {
			t.Errorf("episodes = %v, want nil", episodes)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()
		srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "not json at all")
		})
		p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
		_, err := p.GetEpisodes(context.Background(), "s")
		var pe *contracts.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a *contracts.ProviderError", err)
		}
		if pe.Op != contracts.OpGetEpisodes {
			t.Errorf("op = %q, want %q", pe.Op, contracts.OpGetEpisodes)
		}
	})
}

// TestAnimePaheParsePlayPage runs the play-page parser over the verbatim
// live capture: the #resolutionMenu buttons carry the kwik embeds in
// data-src with the quality in data-resolution. The episode dropdown on
// the same page also uses .dropdown-item anchors — those must not leak in.
func TestAnimePaheParsePlayPage(t *testing.T) {
	t.Parallel()

	links := animePahePlayLinks(fixture(t, "animepahe_play.html"))
	if len(links) != 2 {
		t.Fatalf("links = %v, want exactly the two kwik qualities", links)
	}
	if links["720"] != "https://kwik.cx/e/zYMpempjBVFT" {
		t.Errorf("720 = %q", links["720"])
	}
	if links["1080"] != "https://kwik.cx/e/40puQMXQfMCO" {
		t.Errorf("1080 = %q", links["1080"])
	}
}

// paheKwikEncode is the test-side inverse of crypto.KwikDecrypt (same
// oracle as the extractors package tests): ord(r)+v1 in base v2 over the
// key alphabet, segments delimited by key[v2].
func paheKwikEncode(plaintext, key string, v1, v2 int) string {
	var b strings.Builder
	for _, r := range plaintext {
		n := int(r) + v1
		var digits []int
		for n > 0 {
			digits = append(digits, n%v2)
			n /= v2
		}
		if len(digits) == 0 {
			digits = []int{0}
		}
		for i := len(digits) - 1; i >= 0; i-- {
			b.WriteByte(key[digits[i]])
		}
		b.WriteByte(key[v2])
	}
	return b.String()
}

// paheKwikPage packs the form HTML into the kwik embed page shape the
// extractor scrapes (extractors.py:604).
func paheKwikPage(actionURL string) string {
	const (
		key = "zpcmtfvk"
		v1  = 30
		v2  = 4
	)
	plaintext := `<form method="POST" action="` + actionURL + `">` +
		`<input type="hidden" name="_token" value="tok123"/></form>`
	return `<!DOCTYPE html><html><body><script>eval(f("` +
		paheKwikEncode(plaintext, key, v1, v2) +
		`",9,"` + key + `",30,4,0)</script></body></html>`
}

// pahePlayPage renders the live #resolutionMenu button shape the parser
// consumes (animepahe.pw play page, 2026-09-18 capture).
func pahePlayPage(embedURL, quality string) string {
	return `<div class="dropdown-menu" id="resolutionMenu">` +
		`<button type="button" data-src="` + embedURL + `" data-url="` + embedURL + `" ` +
		`data-fansub="OZC" data-resolution="` + quality + `" data-audio="jpn" data-av1="0" ` +
		`class="dropdown-item">OZC · ` + quality + `p <span class="badge badge-primary">BD</span></button></div>`
}

// TestAnimePaheResolveStreamKwikRoundTrip is the PR7 round trip, PR49
// markup: play page data-src button -> kwik embed page -> packed params
// -> KwikDecrypt -> token POST -> 302 -> m3u8 link in the stream
// (animepahe.py:93-150 with the ported kwik extractor).
func TestAnimePaheResolveStreamKwikRoundTrip(t *testing.T) {
	t.Parallel()

	// kwik fake: embed page, /dl POST redirects to the playlist.
	var playlistHits atomic.Int32
	kwik := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kwik/e/abc123XYZ":
			if got := r.Header.Get("Referer"); got != "https://animepahe.pw" {
				t.Errorf("kwik embed Referer = %q, want https://animepahe.pw (the serving origin)", got)
			}
			_, _ = w.Write([]byte(paheKwikPage("http://" + r.Host + "/dl"))) //nolint:gosec // test-owned fixture writer
		case "/dl":
			//nolint:gosec // test-owned redirect target
			http.Redirect(w, r, "http://"+r.Host+"/f/master.m3u8", http.StatusFound)
		case "/f/master.m3u8":
			playlistHits.Add(1)
			_, _ = w.Write([]byte("#EXTM3U\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kwik.Close)

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, pahePlayPage(kwik.URL+"/kwik/e/abc123XYZ", "1080")) //nolint:gosec // test-owned fixture writer
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "f903fca6-42ca-c7f2-d631-0dc0f1605ba5|3b736dd0beb4caf0",
		RawEmbeds: map[string][]string{
			"Original (Pahe)": {srv.URL + "/play/f903fca6-42ca-c7f2-d631-0dc0f1605ba5/3b736dd0beb4caf0"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Original (Pahe)" {
		t.Errorf("DubName = %q, want the fixed dub name (animepahe.py:150)", stream.DubName)
	}
	if playlistHits.Load() != 1 {
		t.Errorf("playlist hits = %d, want 1", playlistHits.Load())
	}
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry from the kwik extraction", stream.Links)
	}
	if src.URL != kwik.URL+"/f/master.m3u8" {
		t.Errorf("1080 URL = %q, want the kwik redirect Location on the fake host", src.URL)
	}
	if src.Type != "m3u8" || src.Headers["Referer"] != "https://kwik.cx/" {
		t.Errorf("1080 source = %+v, want m3u8 with the kwik.cx Referer", src)
	}
}

// A data-src button carrying direct media resolves through the factory
// fallback even while kwik embeds stay unresolved (animepahe.py:142-144).
func TestAnimePaheResolveStreamDirectMedia(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, pahePlayPage("https://cdn.animepahe.example/file.mp4", "720"))
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

// A play page without any resolvable button is a typed error, not a
// silently empty stream (the dropdown is server-rendered; empty means
// the shape drifted or the challenge intercepted the body).
func TestAnimePaheResolveStreamNoButtons(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>Just a moment...</body></html>")
	})
	p := newAnimePahe(srv.URL, testClient(t, "animepahe"))
	episode := contracts.Episode{
		RawID:     "s|e",
		RawEmbeds: map[string][]string{"Original (Pahe)": {srv.URL + "/play/s/e"}},
	}

	_, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *contracts.ProviderError for the empty dropdown", err)
	}
	if pe.Op != contracts.OpResolveStream {
		t.Errorf("op = %q, want %q", pe.Op, contracts.OpResolveStream)
	}
}

func TestAnimePaheProviderMeta(t *testing.T) {
	t.Parallel()

	p := newAnimePahe(AnimePaheBase, testClient(t, "animepahe"))
	if p.ID() != "animepahe" || p.Name() != "AnimePahe" || p.BaseURL() != AnimePaheBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// JA audio + EN subs + video (PR23: wanted-language audio → BOTH).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ja" {
		t.Errorf("ContentLanguage = %q, want ja", p.ContentLanguage())
	}
}
