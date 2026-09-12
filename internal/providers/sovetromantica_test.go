package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestSovetRomanticaSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "sovetromantica_search.html"))
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))

	results, err := p.Search(context.Background(), "твоё имя")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Python quote() encodes spaces as %20, not "+" (sovetromantica.py:30).
	if rec.Path != "/anime" {
		t.Errorf("request path = %q", rec.Path)
	}
	if !strings.HasPrefix(rec.Query, "query=") || strings.Contains(rec.Query, "+") {
		t.Errorf("request query = %q, want percent-encoding without +", rec.Query)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (third block lacks the name node)", len(results))
	}
	if results[0].Title != "Твоё имя" {
		t.Errorf("Title = %q, want .anime--block__name text", results[0].Title)
	}
	if results[0].URL != srv.URL+"/anime/123" {
		t.Errorf("URL = %q, want relative href prefixed with the site root", results[0].URL)
	}
	if results[0].SourceID != "sovetromantica" {
		t.Errorf("SourceID = %q", results[0].SourceID)
	}
	if results[1].URL != "https://sovetromantica.com/anime/456" {
		t.Errorf("URL = %q, want absolute href untouched", results[1].URL)
	}
}

func TestSovetRomanticaSearchSendsReferer(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html></html>")
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := rec.Header.Get("Referer"); got != srv.URL {
		t.Errorf("Referer = %q, want the site root (sovetromantica.py:27)", got)
	}
}

func TestSovetRomanticaGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "sovetromantica_anime.html"))
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/123")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want 4", len(episodes))
	}
	// Numeric ascending sort: 1, 2, 3, 10 (sovetromantica.py:82).
	wantNums := []string{"1", "2", "3", "10"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	// The "Эпизод" label is stripped from the span text (py :74).
	if episodes[0].Title != "" {
		t.Errorf("episodes[0].Title = %q, want empty (Python sets no title)", episodes[0].Title)
	}
	if episodes[0].RawID != srv.URL+"/watch/123/1" {
		t.Errorf("RawID = %q, want the absolute episode URL", episodes[0].RawID)
	}
	raw := episodes[0].RawEmbeds["SovetRomantica"]
	if len(raw) != 1 || raw[0] != srv.URL+"/watch/123/1" {
		t.Errorf("RawEmbeds = %v, want the episode URL", raw)
	}
}

func TestSovetRomanticaGetEpisodesListFallback(t *testing.T) {
	t.Parallel()

	// Pages without the slick carousel fall back to .episodes-list
	// (sovetromantica.py:60).
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<div class="episodes-list">
  <div class="episode"><a href="/watch/9/1"><span>Эпизод 1</span></a></div>
</div>`)
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/9")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want one episode via the list fallback", episodes)
	}
}

func TestSovetRomanticaGetEpisodesProvider403(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))

	_, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/1")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestSovetRomanticaResolveStream(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "sovetromantica_episode.html"))
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: srv.URL + "/watch/123/1",
		RawEmbeds: map[string][]string{
			"SovetRomantica": {srv.URL + "/watch/123/1"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "SovetRomantica")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry", stream.Links)
	}
	if src.URL != "https://sm.sovetromantica.com/videos/kimi_no_na_wa_1/m3u8/master.m3u8" {
		t.Errorf("URL = %q, want the file: regex capture", src.URL)
	}
	if src.Quality != "1080" {
		t.Errorf("Quality = %q", src.Quality)
	}
	// Task ruling: the m3u8 must carry the site Referer for mpv (Python
	// left stream headers empty).
	if src.Headers["Referer"] != srv.URL {
		t.Errorf("Referer = %q, want the site root", src.Headers["Referer"])
	}
	if stream.DubName != "SovetRomantica" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestSovetRomanticaResolveStreamRelativeM3U8(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<script>jwplayer("p").setup({ file: "/videos/x_1/m3u8/master.m3u8" });</script>`)
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{"SovetRomantica": {srv.URL + "/watch/x/1"}},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "SovetRomantica")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v", stream.Links)
	}
	if src.URL != srv.URL+"/videos/x_1/m3u8/master.m3u8" {
		t.Errorf("URL = %q, want base-prefixed relative m3u8", src.URL)
	}
}

func TestSovetRomanticaResolveStreamNoFileIsEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no player here</body></html>")
	})
	p := newSovetRomantica(srv.URL, testClient(t, "sovetromantica"))
	episode := contracts.Episode{
		RawEmbeds: map[string][]string{"SovetRomantica": {srv.URL + "/watch/y/1"}},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "SovetRomantica")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty without a file: entry", stream.Links)
	}
}

func TestSovetRomanticaResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newSovetRomantica(SovetRomanticaBase, testClient(t, "sovetromantica"))

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty (Python defaults the url to \"\")", stream.Links)
	}
}

func TestSovetRomanticaProviderMeta(t *testing.T) {
	t.Parallel()

	p := newSovetRomantica(SovetRomanticaBase, testClient(t, "sovetromantica"))
	if p.ID() != "sovetromantica" || p.Name() != "SovetRomantica" || p.BaseURL() != SovetRomanticaBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
