package providers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestSameBandSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "sameband_search.html"))
	})
	p := newSameBand(srv.URL, testClient(t, "sameband"))

	results, err := p.Search(context.Background(), "ван")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/index.php" {
		t.Errorf("request path = %q, rec=%+v", rec.Path, rec)
	}
	formVal := func(key string) string {
		if v, ok := rec.Form[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if formVal("do") != "search" || formVal("subaction") != "search" || formVal("story") != "ван" {
		t.Errorf("form = %v, want do/subaction=search story=ван", rec.Form)
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (third col-auto lacks the poster title)", len(results))
	}
	if results[0].Title != "Ван Пис" {
		t.Errorf("Title = %q, want the .poster[title] attribute", results[0].Title)
	}
	// Python keeps the raw href verbatim, relative or not (sameband.py:42).
	if results[0].URL != "/anime/van-pis" {
		t.Errorf("URL = %q, want the raw relative href untouched", results[0].URL)
	}
	if results[1].URL != "https://mirror.example/anime/duo" {
		t.Errorf("URL = %q, want the absolute href untouched", results[1].URL)
	}
	// Python always prefixes the site root onto the img src, even when
	// the src is already absolute (sameband.py:44) — quirk preserved.
	if results[1].Poster != srv.URL+"/covers/duo.jpg" {
		t.Errorf("Poster = %q, want base-prefixed relative src", results[1].Poster)
	}
}

func TestSameBandGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anime/van-pis":
			_, _ = w.Write(fixture(t, "sameband_anime.html"))
		case "/player/123":
			_, _ = fmt.Fprint(w, `var p = new Playerjs({id:"container", file:"/playlist/123.json"});`)
		case "/playlist/123.json":
			_, _ = w.Write(fixture(t, "sameband_playlist.json"))
		default:
			http.NotFound(w, r)
		}
	})
	p := newSameBand(srv.URL, testClient(t, "sameband"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/van-pis")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[1].Num != "2" {
		t.Errorf("nums = %q/%q, want 1/2", episodes[0].Num, episodes[1].Num)
	}
	if episodes[0].Title != "Серия 1" {
		t.Errorf("Title = %q, want the playlist entry title", episodes[0].Title)
	}
	// Python item.get("title", f"Episode {i}") — missing key falls back.
	if episodes[1].Title != "Episode 2" {
		t.Errorf("Title = %q, want the index fallback", episodes[1].Title)
	}
	raw := episodes[0].RawEmbeds["SameBand"]
	if len(raw) != 1 || raw[0] != "[1080p]/hls/1/1080/index.m3u8,[720p]/hls/1/720/index.m3u8" {
		t.Errorf("RawEmbeds = %v, want the raw quality-prefixed file string", raw)
	}
}

func TestSameBandGetEpisodesNoIframe(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>no player here</body></html>")
	})
	p := newSameBand(srv.URL, testClient(t, "sameband"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/none")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without the iframe (sameband.py:53)", len(episodes))
	}
}

// Python wraps only json.loads of the playlist in a bare except
// (sameband.py:68-71): a non-JSON playlist yields no episodes while the
// page and player fetches stay loud.
func TestSameBandGetEpisodesPlaylistDecodeSilent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anime/x":
			_, _ = fmt.Fprint(w, `<div class="player"><div class="player-content"><iframe src="/player/9"></iframe></div></div>`)
		case "/player/9":
			_, _ = fmt.Fprint(w, `Playerjs({file:"/playlist/9.txt"})`)
		case "/playlist/9.txt":
			_, _ = fmt.Fprint(w, "not-json")
		default:
			http.NotFound(w, r)
		}
	})
	p := newSameBand(srv.URL, testClient(t, "sameband"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/x")
	if err != nil {
		t.Fatalf("GetEpisodes: %v, want silent empty", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

func TestSameBandResolveStream(t *testing.T) {
	t.Parallel()

	p := newSameBand("https://sameband.studio", testClient(t, "sameband"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"SameBand": {"[1080p]/hls/1/1080/index.m3u8,[720p]https://cdn.sameband.example/hls/1/720/index.m3u8,junk"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "SameBand")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 2 {
		t.Fatalf("Links = %v, want 1080 and 720", stream.Links)
	}
	src1080, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry", stream.Links)
	}
	if src1080.URL != "https://sameband.studio/hls/1/1080/index.m3u8" {
		t.Errorf("URL = %q, want the base-prefixed relative path", src1080.URL)
	}
	if src1080.Type != "m3u8" {
		t.Errorf("Type = %q, want m3u8 (sameband.py:95)", src1080.Type)
	}
	// Task ruling (PR5): direct site media carries the Referer for mpv;
	// the Python original left stream headers empty.
	if src1080.Headers["Referer"] != "https://sameband.studio" {
		t.Errorf("Referer = %q, want the site root", src1080.Headers["Referer"])
	}
	src720 := stream.Links["720"]
	if src720.URL != "https://cdn.sameband.example/hls/1/720/index.m3u8" {
		t.Errorf("URL = %q, want the absolute href untouched", src720.URL)
	}
	if stream.DubName != "SameBand" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestSameBandResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newSameBand("https://sameband.studio", testClient(t, "sameband"))
	stream, err := p.ResolveStream(context.Background(), contracts.Episode{
		RawEmbeds: map[string][]string{"SameBand": {"[720p]/x.m3u8"}},
	}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty (Python defaults the file to \"\")", stream.Links)
	}
}

func TestSameBandProviderMeta(t *testing.T) {
	t.Parallel()

	p := newSameBand(SameBandBase, testClient(t, "sameband"))
	if p.ID() != "sameband" || p.Name() != "SameBand" || p.BaseURL() != SameBandBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
