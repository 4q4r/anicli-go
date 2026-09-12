package providers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestDreamCastSearch(t *testing.T) {
	t.Parallel()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"releases": [
			{"russian": "Мастер меча онлайн", "original": "Sword Art Online", "url": "/anime/sao", "image": "https://img.example/sao.jpg"},
			{"russian": null, "original": "Absolute Duo", "url": "https://abs.example/anime/duo", "image": "https://img.example/duo.jpg"},
			{"russian": "Без ссылки", "original": "No Link", "url": null, "image": ""}
		]}`)
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	results, err := p.Search(context.Background(), "меч")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Path != "/" {
		t.Errorf("request path = %q", rec.Path)
	}
	// Form payload verbatim from dreamcast.py:27 (ints rendered as strings).
	formVal := func(key string) string {
		if v, ok := rec.Form[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if formVal("search") != "меч" || formVal("status") != "" ||
		formVal("pageSize") != "16" || formVal("pageNumber") != "1" {
		t.Errorf("form = %v, want search/status=\"\"/pageSize=16/pageNumber=1", rec.Form)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].Title != "Мастер меча онлайн" {
		t.Errorf("Title = %q, want russian preferred (dreamcast.py:37)", results[0].Title)
	}
	if results[0].URL != srv.URL+"/anime/sao" {
		t.Errorf("URL = %q, want relative href prefixed with the site root", results[0].URL)
	}
	if results[1].Title != "Absolute Duo" {
		t.Errorf("Title = %q, want original fallback for a null russian", results[1].Title)
	}
	if results[1].URL != "https://abs.example/anime/duo" {
		t.Errorf("URL = %q, want absolute href untouched", results[1].URL)
	}
	// Python appends releases without a url verbatim (dreamcast.py:39-47);
	// the missing link stays empty here.
	if results[2].URL != "" {
		t.Errorf("URL = %q, want empty for a missing url field", results[2].URL)
	}
}

// Python wraps only json.loads in the bare except (dreamcast.py:30-32):
// transport errors stay loud, decode failures return [].
func TestDreamCastSearchDecodeFailureSilent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>not json</html>")
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v, want silent empty", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestDreamCastGetEpisodes(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write(fixture(t, "dreamcast_anime.html"))
		case "/js/playerjs/player.v12.9.js":
			_, _ = w.Write(fixture(t, "dreamcast_player.js"))
		default:
			http.NotFound(w, r)
		}
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3 (fixture decodes through the full crypto chain)", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[1].Num != "2" || episodes[2].Num != "3" {
		t.Errorf("nums = %q %q %q, want 1..3", episodes[0].Num, episodes[1].Num, episodes[2].Num)
	}
	if episodes[0].Title != "Серия 1" {
		t.Errorf("Title = %q, want the playlist title", episodes[0].Title)
	}
	raw := episodes[2].RawEmbeds["DreamCast"]
	if len(raw) != 1 || raw[0] != "https://cdn.dreamerscast.example/hls/3/master.m3u8" {
		t.Errorf("RawEmbeds = %v, want the playlist file url", raw)
	}
}

func TestDreamCastGetEpisodesNoPlayer(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>plain page without playerjs</body></html>")
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0 without the Playerjs markers (dreamcast.py:67)", len(episodes))
	}
}

// A playerjs whose crypto chain cannot decode yields no episodes: the
// Python whole-flow except-block returns [] (dreamcast.py:75-88).
func TestDreamCastGetEpisodesDecodeFailureSilent(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = fmt.Fprint(w, `<script>new Playerjs("ZZnotdecodable");</script><script src="/js/playerjs/x.js"></script>`)
		case "/js/playerjs/x.js":
			_, _ = fmt.Fprint(w, "var nothing = true;")
		default:
			http.NotFound(w, r)
		}
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("GetEpisodes: %v, want silent empty", err)
	}
	if len(episodes) != 0 {
		t.Errorf("episodes = %d, want 0", len(episodes))
	}
}

func TestDreamCastResolveStream(t *testing.T) {
	t.Parallel()

	p := newDreamCast("https://dreamerscast.com", testClient(t, "dreamcast"))
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"DreamCast": {"https://a.example/1/master.m3u8, relative/path/only https://b.example/2/stream.mpd notaurl"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "DreamCast")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	// Commas become spaces before splitting (dreamcast.py:94); only
	// http(s) URLs ending in .m3u8/.mpd count and the LAST match wins the
	// single 1080 slot.
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry", stream.Links)
	}
	if src.URL != "https://b.example/2/stream.mpd" {
		t.Errorf("URL = %q, want the mpd (last match wins)", src.URL)
	}
	if src.Quality != "1080" {
		t.Errorf("Quality = %q", src.Quality)
	}
	// Task ruling (PR5): direct CDN media carries the site Referer for
	// mpv; the Python original left stream headers empty.
	if src.Headers["Referer"] != "https://dreamerscast.com" {
		t.Errorf("Referer = %q, want the site root", src.Headers["Referer"])
	}
	if stream.DubName != "DreamCast" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestDreamCastResolveStreamUnknownDubIsEmpty(t *testing.T) {
	t.Parallel()

	p := newDreamCast("https://dreamerscast.com", testClient(t, "dreamcast"))
	stream, err := p.ResolveStream(context.Background(), contracts.Episode{
		RawEmbeds: map[string][]string{"DreamCast": {"https://a.example/x.m3u8"}},
	}, "NoSuchDub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Errorf("Links = %v, want empty (Python defaults the url to \"\")", stream.Links)
	}
}

// The crypto helpers are pinned against vectors produced by the frozen
// Python implementation (anicli-py dreamcast.py, oracle-verified
// 2026-09-12): the same inputs must decode identically in Go.
func TestDreamCastDecodeVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "#0 salt path",
			in:   "#0RyATIZJrMCI6IZEVLCJVNzEVbVJVIn0=",
			want: `{  "bk0":"a","bk1":"b"}`,
		},
		{
			name: "#1 pepper+salt path",
			in:   "#1GNYKCOPKqKY6GKXhXbYgoKX6Xca5Xc0=",
			want: `{"bk0":"zz","bk2":"yy"}`,
		},
		{
			name: "no salt marker passes through",
			in:   "nosaltmarker-just-json",
			want: "nosaltmarker-just-json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := dreamDecode(tt.in)
			if err != nil {
				t.Fatalf("dreamDecode(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("dreamDecode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDreamCastUnpackPlayerJS(t *testing.T) {
	t.Parallel()

	// Canonical (p,a,c,k,e,d) packer shape; expected output verified with
	// the frozen Python _unpack_playerjs (oracle, 2026-09-12).
	packed := "eval(function(p,a,c,k,e,d){e=function(c){return(c<a?'':e(parseInt(c/a)))" +
		"+((c=c%a)>35?String.fromCharCode(c+29):c.toString(36))};while(c--)" +
		"{if(k[c]){d[e(c)]=k[c]}}return p}('var greet=function(){return 0 1 2}" +
		"',62,3,'base|hello|world'.split('|'),0,{}))"
	want := "var greet=function(){return base hello world}"

	if got := unpackPlayerJS(packed); got != want {
		t.Errorf("unpackPlayerJS = %q, want %q", got, want)
	}

	// No packer marker: returned unchanged (dreamcast.py:143).
	plain := "var a = 1;"
	if got := unpackPlayerJS(plain); got != plain {
		t.Errorf("unpackPlayerJS(plain) = %q, want unchanged", got)
	}
}

func TestDreamCastProviderMeta(t *testing.T) {
	t.Parallel()

	p := newDreamCast(DreamCastBase, testClient(t, "dreamcast"))
	if p.ID() != "dreamcast" || p.Name() != "DreamCast" || p.BaseURL() != DreamCastBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
}
