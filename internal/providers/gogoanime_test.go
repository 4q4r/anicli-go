package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/crypto"
)

func TestGogoAnimeSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_search.html"))
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	results, err := p.Search(context.Background(), "naruto shipuuden")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/search.html" {
		t.Errorf("request path = %q", rec.Path)
	}
	// Python requests params={"keyword": query} encodes spaces as form-style
	// "+" (gogoanime.py:23); url.Values.Encode matches that behavior.
	if want := "keyword=naruto+shipuuden"; rec.Query != want {
		t.Errorf("request query = %q, want %q", rec.Query, want)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (fourth li has no anchor)", len(results))
	}
	if results[0].Title != "Naruto" {
		t.Errorf("Title = %q, want anchor title attribute", results[0].Title)
	}
	// Python keeps the relative category href verbatim (gogoanime.py:38).
	if results[0].URL != "/category/naruto" {
		t.Errorf("URL = %q, want the relative href untouched", results[0].URL)
	}
	if results[0].SourceID != "gogoanime" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[0].Poster != "https://img.example/naruto.jpg" {
		t.Errorf("Poster = %q, want img src", results[0].Poster)
	}
}

func TestGogoAnimeSearchSendsReferer(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html></html>")
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// PR5 task ruling: Python sent only the user agent (already applied by
	// the netclient); the embed-heavy site additionally needs the Referer.
	if got := rec.Header.Get("Referer"); got != srv.URL {
		t.Errorf("Referer = %q, want the site root", got)
	}
}

func TestGogoAnimeGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/category/naruto":
			_, _ = w.Write(fixture(t, "gogoanime_anime.html"))
		case "/ajax/load-list-episode":
			_, _ = w.Write(fixture(t, "gogoanime_episodes.html"))
		default:
			http.NotFound(w, r)
		}
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), "/category/naruto")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3 (anchor-less li skipped)", len(episodes))
	}

	// Ajax request shape (gogoanime.py:63-71).
	if rec.Path != "/ajax/load-list-episode" {
		t.Fatalf("request path = %q, want the ajax endpoint", rec.Path)
	}
	gotQuery, err := url.ParseQuery(rec.Query)
	if err != nil {
		t.Fatalf("parse ajax query %q: %v", rec.Query, err)
	}
	wantQuery := url.Values{
		"ep_start":   {"0"},
		"ep_end":     {"10000"},
		"id":         {"123"},
		"default_ep": {"0"},
		"alias":      {"naruto"},
	}
	if gotQuery.Encode() != wantQuery.Encode() {
		t.Errorf("ajax query = %q, want %q", rec.Query, wantQuery.Encode())
	}

	// Python reverses the ajax order verbatim, no numeric sort
	// (gogoanime.py:95): input EP 3, EP 1, nameless → [nameless, 1, 3].
	wantNums := []string{"0", "1", "3"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	if episodes[1].Title != "Episode 1" {
		t.Errorf("Title = %q, want \"Episode 1\" (gogoanime.py:89)", episodes[1].Title)
	}
	if episodes[1].RawID != "/naruto-episode-1" {
		t.Errorf("RawID = %q, want the episode slug", episodes[1].RawID)
	}
	if len(episodes[1].RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (dubs are fetched lazily)", episodes[1].RawEmbeds)
	}
}

func TestGogoAnimeGetEpisodesNoMovieID(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no player form</body></html>")
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/category/none")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without #movie_id (gogoanime.py:56)", len(episodes))
	}
}

func TestGogoAnimeGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/category/x")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestGogoAnimeFetchDubs(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_episode.html"))
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	episode := contracts.Episode{Num: "1", RawID: "/naruto-episode-1"}
	got, err := p.FetchDubs(context.Background(), &episode)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	embeds := got.RawEmbeds
	if len(embeds) != 3 {
		t.Fatalf("RawEmbeds = %v, want 3 servers (empty/no data-video skipped)", embeds)
	}
	if v := embeds["Vidstreaming"]; len(v) != 1 || v[0] != "https://goload.pro/streaming.php?id=x123abc" {
		t.Errorf("Vidstreaming = %v, want protocol-relative data-video prefixed with https:", v)
	}
	if v := embeds["Streamtape"]; len(v) != 1 || v[0] != "https://streamtape.com/e/abc123/" {
		t.Errorf("Streamtape = %v", v)
	}
	if _, ok := embeds["Broken"]; ok {
		t.Error("Broken (empty data-video) must be skipped")
	}
}

func TestGogoAnimeResolveStreamLazyFetchesDubs(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "gogoanime_episode.html"))
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	// Python resolve_stream fetches dubs on demand when raw_embeds is
	// empty (gogoanime.py:124-125); the CDN server carries a direct mp4
	// that resolves through the factory fallback.
	episode := contracts.Episode{Num: "1", RawID: "/naruto-episode-1"}
	stream, err := p.ResolveStream(context.Background(), episode, "CDN")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["720"]
	if !ok {
		t.Fatalf("Links = %v, want a 720 entry via the lazy dub fetch", stream.Links)
	}
	if src.URL != "https://cdn.example/direct.mp4" {
		t.Errorf("URL = %q", src.URL)
	}
	if stream.DubName != "CDN" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

// TestGogoAnimeResolveStreamGogoPlayRoundTrip is the PR7 round trip for
// the lazy-dub path: episode page server list -> gogoplay embed ->
// AES keys + data-value -> encrypt-ajax.php -> decrypted sources
// (gogoanime.py:122-134 with the ported gogoplay extractor).
func TestGogoAnimeResolveStreamGogoPlayRoundTrip(t *testing.T) {
	t.Parallel()

	const (
		keyEnc = "3947103857291746"
		keyIV  = "1029384756102938"
		keyDec = "5647382910473829"
	)
	gogo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embedplus":
			enc, err := crypto.AESEncrypt("id=content123&alias=naruto", []byte(keyEnc), []byte(keyIV))
			if err != nil {
				t.Errorf("encrypt data-value: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(w, `<div class="container-%s" id="videocontent-%s" data-value=%q></div><script>var videocontent-%s; var container-%s;</script>`,
				keyEnc, keyIV, enc, keyDec, keyDec)
		case "/encrypt-ajax.php":
			enc, err := crypto.AESEncrypt(
				`{"source":[{"file":"https://h.example/hls/master.m3u8","label":"1080 P"}]}`,
				[]byte(keyDec), []byte(keyIV))
			if err != nil {
				t.Errorf("encrypt ajax payload: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":%q}`, enc)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gogo.Close)

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<div class="anime_muti_link"><a href="javascript:void(0)" data-video="%s/embedplus?id=content123">Choose this server Vidstreaming</a></div>`, gogo.URL)
	})
	p := newGogoAnime(srv.URL, srv.URL+"/ajax/load-list-episode", testClient(t, "gogoanime"))

	episode := contracts.Episode{Num: "1", RawID: "/naruto-episode-1"}
	stream, err := p.ResolveStream(context.Background(), episode, "Vidstreaming")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Vidstreaming" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry from the gogoplay extraction", stream.Links)
	}
	if src.URL != "https://h.example/hls/master.m3u8" || src.Type != "m3u8" {
		t.Errorf("1080 = %+v, want the decrypted master.m3u8", src)
	}
}

func TestGogoAnimeProviderMeta(t *testing.T) {
	t.Parallel()

	p := newGogoAnime(GogoAnimeBase, GogoAnimeAjaxBase, testClient(t, "gogoanime"))
	if p.ID() != "gogoanime" || p.Name() != "GogoAnime" || p.BaseURL() != GogoAnimeBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeVideo {
		t.Errorf("SourceType = %q, want video", p.SourceType())
	}
}
