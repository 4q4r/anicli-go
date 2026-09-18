package providers

// Fixture provenance (REAL captures, 2026-09-18, direct connection,
// desktop UA — the proxy egress route is Cloudflare-404ed by the site):
//   - dreamcast_search.json — POST / (search=мао&status=&pageSize=16&pageNumber=1),
//     response verbatim. "мао" is the provider's live smoke query: the
//     catalog is the team's OWN dubs (~535 releases, strict prefix
//     search), so the shared smoke probes (черная лагуна / black
//     lagoon) can never surface.
//   - dreamcast_release.html — GET /home/release/541-yomi-no-tsugai,
//     page chrome trimmed; the inline player script (new Playerjs +
//     vlc()) and the playerjs.min.js script tag are byte-verbatim.
//     The #2 blob decodes through the live library's baked-in key to
//     the 23-episode playlist.
//   - dreamcast_release_film.html — GET /home/release/527-kobayashi-san
//     (a FILM release: the playlist `file` is a single URL string, not
//     an episode array; the frozen port silently returned [] here).
//   - dreamcast_release_blocked.html — GET /home/release/111-… (Bleach
//     TYBW), the license-blocked variant served to this network: the
//     playlist is a single …/dash/block,… URL string labelled
//     "111-block".
//   - dreamcast_playerjs.js — the crypt-key assignment of the LIVE
//     packer-unpacked /js/playerjs.min.js (Playerjs 20.7.1); library
//     code trimmed, the u value (the #1 salt+pepper config carrying
//     bk0..bk4) byte-verbatim.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// serveRelease wires a fixture server the way the live site behaves:
// the catalog path POSTs back JSON, release pages carry the inline
// player script, and the versioned /js/playerjs.min.js URL serves the
// (unpacked-fragment) library fixture.
func serveRelease(t *testing.T, pageFixture string) (string, *recordedRequest) {
	t.Helper()

	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "dreamcast_search.json"))
		case r.Method == http.MethodGet && (r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/home/release/")):
			_, _ = w.Write(fixture(t, pageFixture))
		case strings.HasPrefix(r.URL.Path, "/js/playerjs"):
			_, _ = w.Write(fixture(t, "dreamcast_playerjs.js"))
		default:
			http.NotFound(w, r)
		}
	})
	return srv.URL, rec
}

func TestDreamCastSearch(t *testing.T) {
	t.Parallel()

	base, rec := serveRelease(t, "dreamcast_release.html")
	p := newDreamCast(base, testClient(t, "dreamcast"))

	results, err := p.Search(context.Background(), "мао")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if rec.Method != http.MethodPost || rec.Path != "/" {
		t.Errorf("request = %s %s, want POST /", rec.Method, rec.Path)
	}
	// Form payload verbatim from dreamcast.py:27 (ints rendered as strings).
	formVal := func(key string) string {
		if v, ok := rec.Form[key]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if formVal("search") != "мао" || formVal("status") != "" ||
		formVal("pageSize") != "16" || formVal("pageNumber") != "1" {
		t.Errorf("form = %v, want search/status=\"\"/pageSize=16/pageNumber=1", rec.Form)
	}

	// The fixture is the verbatim live response for "мао": one release
	// (Мао, id 542).
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (live capture)", len(results))
	}
	if results[0].Title != "Мао" {
		t.Errorf("Title = %q, want russian preferred (dreamcast.py:37)", results[0].Title)
	}
	if results[0].URL != base+"/home/release/542-mao" {
		t.Errorf("URL = %q, want relative href prefixed with the site root", results[0].URL)
	}
	if results[0].Poster != "//cache.dreamerscast.com/releases/542/e478bfc8-3433-4afb-86c3-026abce75582.webp" {
		t.Errorf("Poster = %q, want the verbatim protocol-relative capture", results[0].Poster)
	}
}

// The site always answers the search POST with JSON; a non-JSON body is
// protocol drift and must fail LOUD (typed) instead of the frozen
// Python-parity silent [] (task ruling, PR51).
func TestDreamCastSearchDecodeFailureTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	results, err := p.Search(context.Background(), "q")
	if err == nil {
		t.Fatalf("Search: %v, %d results — want a typed error", err, len(results))
	}
	if !errors.Is(err, contracts.ErrExtractFailed) && !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want a contracts sentinel cause", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpSearch {
		t.Errorf("err = %v, want a *ProviderError tagged op=search", err)
	}
}

func TestDreamCastGetEpisodes(t *testing.T) {
	t.Parallel()

	base, _ := serveRelease(t, "dreamcast_release.html")
	p := newDreamCast(base, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), base+"/home/release/541-yomi-no-tsugai")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The real #2 blob decodes through the full crypto chain to the
	// live 23-episode playlist (Yomi no Tsugai, release 541).
	if len(episodes) != 23 {
		t.Fatalf("episodes = %d, want 23 (live capture)", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[0].Title != "Серия 1" {
		t.Errorf("first = %q/%q, want 1/Серия 1", episodes[0].Num, episodes[0].Title)
	}
	if episodes[22].Num != "23" || episodes[22].Title != "Серия 23" {
		t.Errorf("last = %q/%q, want 23/Серия 23", episodes[22].Num, episodes[22].Title)
	}
	raw := episodes[0].RawEmbeds["DreamCast"]
	if len(raw) != 1 {
		t.Fatalf("RawEmbeds = %v, want the single DreamCast embed", episodes[0].RawEmbeds)
	}
	// The live file field carries BOTH manifests joined by " or ":
	// dash .mpd first, hls master.m3u8 last.
	if !strings.HasPrefix(raw[0], "https://play.dreamerscast.com/dash/") ||
		!strings.Contains(raw[0], " or https://play.dreamerscast.com/hls/") ||
		!strings.HasSuffix(raw[0], "master.m3u8") {
		t.Errorf("embed = %q, want the live dual-manifest string", raw[0])
	}
}

// Film releases (12 on the live catalog) carry a single-string file
// instead of an episode array — one episode titled Фильм (the kodik
// movie fallback convention), not a silent [].
func TestDreamCastGetEpisodesFilm(t *testing.T) {
	t.Parallel()

	base, _ := serveRelease(t, "dreamcast_release_film.html")
	p := newDreamCast(base, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), base+"/home/release/527-kobayashi-san")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1 (film release)", len(episodes))
	}
	if episodes[0].Num != "1" || episodes[0].Title != "Фильм" {
		t.Errorf("episode = %q/%q, want 1/Фильм", episodes[0].Num, episodes[0].Title)
	}
	raw := episodes[0].RawEmbeds["DreamCast"]
	if len(raw) != 1 || !strings.Contains(raw[0], "manifest.mpd or ") {
		t.Errorf("RawEmbeds = %v, want the film's dual-manifest string", raw)
	}
}

// The blocked variant (license-blocked releases: label "<id>-block",
// file …/dash/block,…) is a typed geo-block, not a silent [].
func TestDreamCastGetEpisodesBlockedTyped(t *testing.T) {
	t.Parallel()

	base, _ := serveRelease(t, "dreamcast_release_blocked.html")
	p := newDreamCast(base, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), base+"/home/release/111-blich-tysiacheletniaia-krovavaia-voina-bleach-sennen-kessen-hen")
	if err == nil {
		t.Fatalf("GetEpisodes: %v, %d episodes — want a typed geo-block", err, len(episodes))
	}
	if !errors.Is(err, contracts.ErrGeoBlocked) {
		t.Errorf("err = %v, want ErrGeoBlocked", err)
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Op != contracts.OpGetEpisodes {
		t.Errorf("err = %v, want a *ProviderError tagged op=get_episodes", err)
	}
}

// A page without the Playerjs markers is extraction drift (or a player-
// less page): typed extract failure instead of the frozen silent [].
func TestDreamCastGetEpisodesNoPlayerTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>plain page without playerjs</body></html>"))
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/")
	if err == nil {
		t.Fatalf("GetEpisodes: %v, %d episodes — want a typed error", err, len(episodes))
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
}

// A playerjs whose crypto chain cannot decode the blob is drift: typed
// extract failure (the frozen port swallowed this as [], which masked
// the whole 2026 site drift until the provider looked dead).
func TestDreamCastGetEpisodesDecodeFailureTyped(t *testing.T) {
	t.Parallel()

	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/js/playerjs"):
			_, _ = w.Write(fixture(t, "dreamcast_playerjs.js"))
		default:
			_, _ = w.Write([]byte(`<script>new Playerjs("ZZnotdecodable");</script><script src="/js/playerjs/x.js"></script>`))
		}
	})
	p := newDreamCast(srv.URL, testClient(t, "dreamcast"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/")
	if err == nil {
		t.Fatalf("GetEpisodes: %v, %d episodes — want a typed error", err, len(episodes))
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
}

func TestDreamCastResolveStream(t *testing.T) {
	t.Parallel()

	p := newDreamCast("https://dreamerscast.com", testClient(t, "dreamcast"))
	// The live file shape: dash mpd and hls m3u8 joined by " or " — the
	// m3u8 is the LAST media match and wins the single 1080 slot.
	episode := contracts.Episode{
		Num:   "1",
		RawID: "1",
		RawEmbeds: map[string][]string{
			"DreamCast": {"https://play.dreamerscast.com/dash/3fe636a3/b1749594_,1080,720,low,opus,.mp4.urlset/manifest.mpd or https://play.dreamerscast.com/hls/3fe636a3/b1749594_,1080,720,low,aac,.mp4.urlset/master.m3u8"},
		},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "DreamCast")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry", stream.Links)
	}
	if src.URL != "https://play.dreamerscast.com/hls/3fe636a3/b1749594_,1080,720,low,aac,.mp4.urlset/master.m3u8" {
		t.Errorf("URL = %q, want the hls master (last match wins)", src.URL)
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

// Comma/space separated lists keep the frozen semantics: only http(s)
// URLs ending .m3u8/.mpd count, the LAST match wins.
func TestDreamCastResolveStreamCommaList(t *testing.T) {
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
	src, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("Links = %v, want a 1080 entry", stream.Links)
	}
	if src.URL != "https://b.example/2/stream.mpd" {
		t.Errorf("URL = %q, want the mpd (last match wins)", src.URL)
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
// 2026-09-12): the same inputs must decode identically in Go. The live
// library (Playerjs 20.7.1) ships the same salt/pepper constants — the
// o.y operand is byte-identical to the frozen port's.
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
	// the frozen Python _unpack_playerjs (oracle, 2026-09-12). The live
	// 20.7.1 library uses the same packer (probe-verified 2026-09-18:
	// the real 823KB file unpacks and yields the u key assignment).
	packed := "eval(function(p,a,c,k,e,d){e=function(c){return(c<a?'':e(parseInt(c/a)))" +
		"+((c=c%a)>35?String.fromCharCode(c+29):c.toString(36))};while(c--)" +
		"{if(k[c]){d[e(c)]=k[c]}}return p}('var greet=function(){return 0 1 2}" +
		"',62,3,'base|hello|world'.split('|'),0,{}))"
	want := "var greet=function(){return base hello world}"

	if got := unpackPlayerJS(packed); got != want {
		t.Errorf("unpackPlayerJS = %q, want %q", got, want)
	}

	// No packer marker: returned unchanged (dreamcast.py:143) — the
	// playerjs fixture rides this path (unpacked fragment).
	plain := "var a = 1;"
	if got := unpackPlayerJS(plain); got != plain {
		t.Errorf("unpackPlayerJS(plain) = %q, want unchanged", got)
	}
}

// The live smoke query: the catalog is the team's own dubs with strict
// prefix search, so the shared probes can never surface — the provider
// declares its own broad, stable, UNBLOCKED hit (Мао, release 542;
// Bleach TYBW — the catalog's top «блич» hit — is license-blocked).
func TestDreamCastSmokeQuery(t *testing.T) {
	t.Parallel()

	p := newDreamCast(DreamCastBase, testClient(t, "dreamcast"))
	if sq, ok := contracts.Provider(p).(contracts.SmokeQueryProvider); !ok {
		t.Fatalf("DreamCast does not declare SmokeQueryProvider")
	} else if sq.SmokeQuery() == "" {
		t.Fatalf("SmokeQuery = \"\", want a provider-specific probe")
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
