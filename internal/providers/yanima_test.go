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
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// yanimaProvider builds the provider against baseURL with test DDoS
// cookies. The cookies are REQUIRED on every API request (dossier
// 2026-09-14: without them the Mitelis wall answers 403), so every
// network-path test carries them.
func yanimaProvider(baseURL string, http *netclient.Client) *Yanima {
	return newYanima(baseURL, "p1-cookie", "p2-cookie", "", http)
}

// yanimaLog records every request the fake API saw (the resolve flow
// issues two-plus calls, so the single-slot fixtureServer recorder is
// not enough here).
type yanimaLog struct {
	mu    sync.Mutex
	paths []string
	query []string
	heads []http.Header
}

func (l *yanimaLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// EscapedPath, not Path: Path is the DECODED view and would hide
	// the wire-level %20 the dub-name encoding contract is pinned on.
	l.paths = append(l.paths, r.URL.EscapedPath())
	l.query = append(l.query, r.URL.RawQuery)
	l.heads = append(l.heads, r.Header.Clone())
}

func (l *yanimaLog) all() (paths, queries []string, heads []http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.paths...), append([]string{}, l.query...),
		append([]http.Header{}, l.heads...)
}

// yanimaFakeAPI serves the resolve protocol: SOURCE returns the
// fixture variants, MANIFEST echoes an m3u8 URL built from the
// requested episode/resolution plus a fixed JWT, and /anime/… stands
// in for the s3.yanima.space HLS server the native-bypass probes hit.
// Handlers can be overridden per-test.
type yanimaFakeAPI struct {
	srv        *httptest.Server
	log        yanimaLog
	sourceBody func() []byte
	manifestFn func(animeID, episode, quality, resolution string) (int, string)
	s3Fn       func(r *http.Request) (int, string)
}

func newYanimaFakeAPI(t *testing.T) *yanimaFakeAPI {
	t.Helper()

	fake := &yanimaFakeAPI{
		sourceBody: func() []byte { return fixture(t, "yanima_source.json") },
		manifestFn: func(_, _, _, resolution string) (int, string) {
			return http.StatusOK, `{"url":"https://s3.yanima.space/anime/350/5/video/8842/` +
				resolution + `/avc/index.m3u8","token":"jwt-` + resolution + `"}`
		},
		// Default S3 answer 404: an unconfigured probe target must fail
		// the bypass candidate, never leak a fake success.
		s3Fn: func(*http.Request) (int, string) { return http.StatusNotFound, "" },
	}
	fake.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.log.add(r)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/anime/v1/player/source/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fake.sourceBody())
		case strings.HasPrefix(r.URL.Path, "/api/anime/v2/player/manifest/"):
			parts := strings.Split(r.URL.Path, "/")
			n := len(parts)
			code, body := fake.manifestFn(parts[n-4], parts[n-3], parts[n-2], parts[n-1])
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		case strings.HasPrefix(r.URL.Path, "/anime/"):
			code, body := fake.s3Fn(r)
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.srv.Close)
	return fake
}

// yanimaEpisode is the canonical raw_embeds carrier: the /play page URL
// with the episode number in the query (dossier: /play/{shikimori_id}).
// The embed URL is the PRODUCTION page URL — only the provider's API
// base points at the fake server.
func yanimaEpisode() contracts.Episode {
	return contracts.Episode{
		Num:   "5",
		RawID: "5",
		RawEmbeds: map[string][]string{
			"Shiroi Kitsune": {YanimaBase + "/play/350?episode=5"},
		},
	}
}

func TestYanimaResolveStreamHappyPath(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	stream, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	paths, queries, heads := fake.log.all()
	if len(paths) != 4 { // 1 source + 3 manifest (2160/1080/720)
		t.Fatalf("requests = %v, want 1 source + 3 manifest calls", paths)
	}

	// SOURCE endpoint: path carries the URL-encoded dub name (dossier
	// example "Shiroi%20Kitsune"), fetchKodik=true is mandatory.
	if paths[0] != "/api/anime/v1/player/source/350/5/Shiroi%20Kitsune" {
		t.Errorf("source path = %q, want the encoded-dub SOURCE route", paths[0])
	}
	if queries[0] != "fetchKodik=true" {
		t.Errorf("source query = %q, want fetchKodik=true", queries[0])
	}
	cookie := heads[0].Get("Cookie")
	if !strings.Contains(cookie, "mit_ck_p1=p1-cookie") || !strings.Contains(cookie, "mit_ck_p2=p2-cookie") {
		t.Errorf("source Cookie = %q, want both DDoS cookies", cookie)
	}
	if heads[0].Get("Accept") != "*/*" {
		t.Errorf("source Accept = %q, want */* (dossier curl capture)", heads[0].Get("Accept"))
	}

	// MANIFEST endpoint: quality/resolution from the SOURCE variants,
	// same DDoS cookies.
	if paths[1] != "/api/anime/v2/player/manifest/350/5/1780/2160" {
		t.Errorf("manifest[0] path = %q, want quality=1780 resolution=2160", paths[1])
	}
	if !strings.Contains(heads[1].Get("Cookie"), "mit_ck_p1=") {
		t.Errorf("manifest Cookie = %q, want the DDoS cookies", heads[1].Get("Cookie"))
	}

	// Links: one entry per resolution, m3u8 type, the ym_ JWT Cookie
	// and the site Referer (both load-bearing on the S3 stream).
	if len(stream.Links) != 3 {
		t.Fatalf("Links = %v, want 2160/1080/720 entries", stream.Links)
	}
	top := stream.Links["2160"]
	if top.URL != "https://s3.yanima.space/anime/350/5/video/8842/2160/avc/index.m3u8" {
		t.Errorf("2160 URL = %q, want the manifest URL", top.URL)
	}
	if top.Type != "m3u8" {
		t.Errorf("2160 Type = %q, want m3u8", top.Type)
	}
	if top.Quality != "2160" {
		t.Errorf("2160 Quality = %q", top.Quality)
	}
	if top.Headers["Referer"] != YanimaStreamReferer {
		t.Errorf("2160 Referer = %q, want %q", top.Headers["Referer"], YanimaStreamReferer)
	}
	if top.Headers["Cookie"] != "ym_=jwt-2160" {
		t.Errorf("2160 Cookie = %q, want the ym_ JWT from the manifest", top.Headers["Cookie"])
	}
	if stream.DubName != "Shiroi Kitsune" {
		t.Errorf("DubName = %q", stream.DubName)
	}
}

func TestYanimaResolveStreamSessionCookieFlows(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := newYanima(fake.srv.URL, "p1", "p2", "sess-42", testClient(t, "yanima"))

	if _, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune"); err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	paths, _, heads := fake.log.all()
	if len(paths) == 0 {
		t.Fatal("no requests recorded")
	}
	// YAA_SESS_ID is the authenticated-content session (dossier); it
	// rides along on the API call when configured…
	if c := heads[0].Get("Cookie"); !strings.Contains(c, "YAA_SESS_ID=sess-42") {
		t.Errorf("source Cookie = %q, want the YAA_SESS_ID session", c)
	}
	// …and on the S3 stream request.
	stream, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if c := stream.Links["2160"].Headers["Cookie"]; !strings.Contains(c, "YAA_SESS_ID=sess-42") {
		t.Errorf("stream Cookie = %q, want the session alongside ym_", c)
	}
}

func TestYanimaResolveStreamWithoutCookiesFailsTyped(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := newYanima(fake.srv.URL, "", "p2-only", "", testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if err == nil {
		t.Fatal(" ResolveStream without mit_ck_p1 must fail")
	}
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "DDoS cookies not configured (providers.yanima.ddoS_p1/ddoS_p2)") {
		t.Errorf("error = %v, want the config-hinting message", err)
	}
	// Fail-loud BEFORE any network: a 403 round-trip would mean the
	// guard missed.
	if paths, _, _ := fake.log.all(); len(paths) != 0 {
		t.Errorf("requests = %v, want zero calls on the config guard", paths)
	}
}

func TestYanimaResolveStreamUnknownDubFailsTyped(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(),
		contracts.Episode{RawEmbeds: map[string][]string{}}, "NoSuchDub")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if paths, _, _ := fake.log.all(); len(paths) != 0 {
		t.Errorf("requests = %v, want zero calls for an unknown dub", paths)
	}
}

func TestYanimaResolveStreamForeignEmbedRejected(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(), contracts.Episode{
		RawEmbeds: map[string][]string{
			"Shiroi Kitsune": {"https://evil.example.com/play/350?episode=5"},
		},
	}, "Shiroi Kitsune")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want ErrExtractFailed for a non-yanima host", err)
	}
	if paths, _, _ := fake.log.all(); len(paths) != 0 {
		t.Errorf("requests = %v, want zero calls for a foreign embed", paths)
	}
}

func TestYanimaResolveStreamSource403MapsProvider403(t *testing.T) {
	t.Parallel()

	// The real-world missing-cookie shape: the Mitelis wall answers
	// 403 on every API route (dossier). The provider must surface the
	// standard sentinel.
	fake := newYanimaFakeAPI(t)
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.log.add(r)
		w.WriteHeader(http.StatusForbidden)
	})
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("error = %v, want ErrProvider403", err)
	}
}

func TestYanimaResolveStreamNoVariantsFailsLoud(t *testing.T) {
	t.Parallel()

	// A shape drift (field renamed upstream) must fail loud, never
	// masquerade as an empty-but-successful stream.
	fake := newYanimaFakeAPI(t)
	fake.sourceBody = func() []byte { return []byte(`{"unexpected":"shape"}`) }
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("error = %v, want ErrExtractFailed", err)
	}
}

func TestYanimaResolveStreamAllManifestsFail(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	fake.manifestFn = func(_, _, _, _ string) (int, string) { return http.StatusInternalServerError, `{}` }
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	_, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if !errors.Is(err, contracts.ErrAllCandidatesFailed) {
		t.Fatalf("error = %v, want ErrAllCandidatesFailed", err)
	}
}

func TestYanimaResolveStreamPartialManifestFailureKeepsGoodLinks(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	fake.manifestFn = func(_, _, _, resolution string) (int, string) {
		if resolution == "1080" {
			return http.StatusInternalServerError, `{}`
		}
		return http.StatusOK, `{"url":"https://s3.yanima.space/x/` + resolution + `/index.m3u8","token":"t"}`
	}
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	stream, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if _, ok := stream.Links["1080"]; ok {
		t.Errorf("the failed 1080 manifest must be skipped, got %v", stream.Links)
	}
	if _, ok := stream.Links["2160"]; !ok {
		t.Errorf("2160 must survive a sibling failure, got %v", stream.Links)
	}
}

func TestYanimaResolveStreamPinnedQualityOnly(t *testing.T) {
	t.Parallel()

	// A yanima embed URL may pin quality/resolution as query params —
	// only that variant is resolved then.
	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	episode := yanimaEpisode()
	episode.RawEmbeds["Shiroi Kitsune"] = []string{
		YanimaBase + "/play/350?episode=5&quality=900&resolution=1080",
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	paths, _, _ := fake.log.all()
	if len(paths) != 1 { // pinned pair: manifest only, no SOURCE discovery
		t.Fatalf("requests = %v, want only the pinned manifest call", paths)
	}
	if paths[0] != "/api/anime/v2/player/manifest/350/5/900/1080" {
		t.Errorf("manifest path = %q, want the pinned 900/1080", paths[0])
	}
	if len(stream.Links) != 1 || stream.Links["1080"].Quality != "1080" {
		t.Errorf("Links = %v, want only the pinned 1080", stream.Links)
	}
}

// yanimaEp6 is the second-episode carrier: same embed grammar, next
// episode — the one the live-verified subscription wall covers.
func yanimaEp6() contracts.Episode {
	episode := yanimaEpisode()
	episode.Num, episode.RawID = "6", "6"
	episode.RawEmbeds["Shiroi Kitsune"] = []string{YanimaBase + "/play/350?episode=6"}
	return episode
}

// hostOf extracts the hostname of a test server URL for the provider's
// injectable s3Host (nativeSrc compares port-stripped hostnames, so
// production srcs like s3.yanima.space:9000 match the bare hostname).
func hostOf(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return u.Hostname()
}

func TestYanimaResolveStreamEp2KodikFallbackWhenManifestWall(t *testing.T) {
	t.Parallel()

	// LIVE-VERIFIED protocol (controller 2026-09-13): the MANIFEST
	// endpoint enforces a server-side subscription wall from episode 2
	// on ("This content is only for subscribed users") while SOURCE
	// keeps serving Kodik m3u8 links for every episode. Ep2+ must fall
	// back to those links instead of failing ErrAllCandidatesFailed.
	fake := newYanimaFakeAPI(t)
	fake.manifestFn = func(_, _, _, _ string) (int, string) {
		return http.StatusForbidden, `{"message":"This content is only for subscribed users"}`
	}
	fake.sourceBody = func() []byte { return fixture(t, "yanima_source_kodik.json") }
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	stream, err := p.ResolveStream(context.Background(), yanimaEp6(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	if len(stream.Links) != 2 {
		t.Fatalf("Links = %v, want the 720/480 Kodik fallback links", stream.Links)
	}
	link := stream.Links["720"]
	if link.URL != "https://kodik.example/hls/817aa7618b177f2b2b874d0e1a3a1a1a/720.m3u8" {
		t.Errorf("720 URL = %q, want the SOURCE-disclosed Kodik src", link.URL)
	}
	if link.Type != "m3u8" || link.Quality != "720" {
		t.Errorf("720 link = %+v, want the m3u8 720 fallback", link)
	}
	if c := link.Headers["Cookie"]; c != "" {
		t.Errorf("Kodik link Cookie = %q, want none (Kodik plays without the ym_ JWT)", c)
	}
	if _, ok := stream.Links["480"]; !ok {
		t.Errorf("480 fallback missing: %v", stream.Links)
	}
	// Manifest-first contract: the wall was still attempted before the
	// fallback engaged.
	paths, _, _ := fake.log.all()
	manifestAttempts := 0
	for _, path := range paths {
		if strings.Contains(path, "/api/anime/v2/player/manifest/") {
			manifestAttempts++
		}
	}
	if manifestAttempts == 0 {
		t.Errorf("no manifest attempts recorded: %v, want manifest-first", paths)
	}
}

func TestYanimaResolveStreamNativeBypassReusesManifestToken(t *testing.T) {
	t.Parallel()

	// The 4K bypass: ep1's manifest mints a valid ym_ JWT for free and
	// the S3 server gates on that JWT alone. The provider caches the
	// token on the ep1 resolve; when ep2's manifest hits the wall, the
	// SOURCE-disclosed native S3 URLs are probed with the cached
	// credential instead of being dropped.
	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))
	p.s3Host = hostOf(fake.srv.URL) // the fake stands in for s3.yanima.space

	// Ep1: the default fixture resolves through manifests; jwt-2160
	// (first variant, first-wins) becomes the cached credential.
	if _, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune"); err != nil {
		t.Fatalf("ep1 resolve: %v", err)
	}

	// Ep2: the manifest wall answers 403; SOURCE discloses the native
	// S3 URL; S3 accepts the ep1 token.
	fake.manifestFn = func(_, ep, _, _ string) (int, string) {
		if ep == "6" {
			return http.StatusForbidden, `{"message":"subscribers only"}`
		}
		return http.StatusOK, `{"url":"` + fake.srv.URL + `/anime/350/6/video/9001/2160/avc/index.m3u8","token":"jwt-ep6"}`
	}
	fake.sourceBody = func() []byte {
		return []byte(`{"sources":[{"source":"yanima","priority":1,` +
			`"resolution":{"name":"2160p","numeric":2160},` +
			`"src":"` + fake.srv.URL + `/anime/350/6/video/9001/2160/avc/index.m3u8"}]}`)
	}
	fake.s3Fn = func(r *http.Request) (int, string) {
		if r.Header.Get("Cookie") == "ym_=jwt-2160" {
			return http.StatusOK, "#EXTM3U\n"
		}
		return http.StatusForbidden, ""
	}

	stream, err := p.ResolveStream(context.Background(), yanimaEp6(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ep2 resolve: %v", err)
	}

	link, ok := stream.Links["2160"]
	if !ok {
		t.Fatalf("Links = %v, want the bypassed native 2160", stream.Links)
	}
	if want := fake.srv.URL + "/anime/350/6/video/9001/2160/avc/index.m3u8"; link.URL != want {
		t.Errorf("2160 URL = %q, want the native S3 src %q", link.URL, want)
	}
	if link.Headers["Cookie"] != "ym_=jwt-2160" {
		t.Errorf("2160 Cookie = %q, want the cached ep1 ym_ token", link.Headers["Cookie"])
	}
	if link.Headers["Referer"] != YanimaStreamReferer {
		t.Errorf("2160 Referer = %q, want %q", link.Headers["Referer"], YanimaStreamReferer)
	}
	if link.Type != "m3u8" || link.Quality != "2160" {
		t.Errorf("2160 link = %+v, want m3u8 2160", link)
	}
}

func TestYanimaResolveStreamNativeBypassRejectedFallsBackToKodik(t *testing.T) {
	t.Parallel()

	// If the S3 server enforces the JWT claims (ep1 token rejected on
	// ep2 files), the bypass candidates die in the probe and the Kodik
	// rows from SOURCE remain the playable links.
	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	if _, err := p.ResolveStream(context.Background(), yanimaEpisode(), "Shiroi Kitsune"); err != nil {
		t.Fatalf("ep1 resolve: %v", err)
	}

	fake.manifestFn = func(_, ep, _, _ string) (int, string) {
		if ep == "6" {
			return http.StatusForbidden, `{"message":"subscribers only"}`
		}
		return http.StatusOK, `{"url":"https://s3.yanima.space/a/index.m3u8","token":"jwt-ep6"}`
	}
	fake.sourceBody = func() []byte {
		return []byte(`{"sources":[` +
			`{"source":"yanima","priority":1,"resolution":{"name":"2160p","numeric":2160},` +
			`"src":"` + fake.srv.URL + `/anime/350/6/video/9001/2160/avc/index.m3u8"},` +
			`{"source":"Kodik","priority":2,"resolution":{"name":"720p","numeric":720},` +
			`"src":"https://kodik.example/hls/x/720.m3u8"}]}`)
	}
	fake.s3Fn = func(*http.Request) (int, string) { return http.StatusForbidden, "" }
	p.s3Host = hostOf(fake.srv.URL)

	stream, err := p.ResolveStream(context.Background(), yanimaEp6(), "Shiroi Kitsune")
	if err != nil {
		t.Fatalf("ep2 resolve: %v", err)
	}

	if _, ok := stream.Links["2160"]; ok {
		t.Errorf("the rejected native 2160 must be dropped, got %v", stream.Links)
	}
	if link, ok := stream.Links["720"]; !ok || link.URL != "https://kodik.example/hls/x/720.m3u8" {
		t.Errorf("720 link = %+v, want the Kodik fallback", link)
	}
}

func TestYanimaResolveStreamTokenlessBypassSkippedFailsLoud(t *testing.T) {
	t.Parallel()

	// Without an earlier manifest success there is no ym_ credential:
	// the bypass must be skipped (zero blind S3 hits) and a
	// native-only SOURCE answer must fail loud instead of emitting
	// links that would 403 in the player.
	fake := newYanimaFakeAPI(t)
	fake.manifestFn = func(_, ep, _, _ string) (int, string) {
		if ep == "6" {
			return http.StatusForbidden, `{"message":"subscribers only"}`
		}
		return http.StatusOK, `{"url":"https://s3.yanima.space/a/index.m3u8","token":"jwt-ep6"}`
	}
	fake.sourceBody = func() []byte {
		return []byte(`{"sources":[{"source":"yanima","priority":1,` +
			`"resolution":{"name":"2160p","numeric":2160},` +
			`"src":"` + fake.srv.URL + `/anime/350/6/video/9001/2160/avc/index.m3u8"}]}`)
	}
	fake.s3Fn = func(*http.Request) (int, string) { return http.StatusOK, "#EXTM3U\n" }
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))
	p.s3Host = hostOf(fake.srv.URL)

	_, err := p.ResolveStream(context.Background(), yanimaEp6(), "Shiroi Kitsune")
	if !errors.Is(err, contracts.ErrAllCandidatesFailed) {
		t.Fatalf("error = %v, want ErrAllCandidatesFailed (no token, no dead links)", err)
	}
	paths, _, _ := fake.log.all()
	for _, path := range paths {
		if strings.HasPrefix(path, "/anime/") {
			t.Errorf("tokenless bypass hit the S3 probe target %q, want zero blind S3 calls", path)
		}
	}
}

func TestYanimaResolveStreamEmbedURLShapes(t *testing.T) {
	t.Parallel()

	fake := newYanimaFakeAPI(t)
	p := yanimaProvider(fake.srv.URL, testClient(t, "yanima"))

	for _, embed := range []string{
		"https://yanima.space/play/350?episode=5",                      // query episode (canonical)
		"https://yanima.space/play/350/5",                              // path episode
		"https://yanima.space/anime/350?episode=5",                     // /anime route (dossier)
		"https://yanima.space/play/350/5?quality=1780&resolution=2160", // pinned
	} {
		episode := contracts.Episode{RawEmbeds: map[string][]string{
			"Shiroi Kitsune": {embed},
		}}
		if _, err := p.ResolveStream(context.Background(), episode, "Shiroi Kitsune"); err != nil {
			t.Errorf("embed %q: %v", embed, err)
		}
	}
}

func TestYanimaSearchStubTypedError(t *testing.T) {
	t.Parallel()

	p := yanimaProvider(YanimaBase, testClient(t, "yanima"))

	_, err := p.Search(context.Background(), "naruto")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "catalog") {
		t.Errorf("error = %v, want the via-catalog explanation", err)
	}
}

func TestYanimaGetEpisodesStubTypedError(t *testing.T) {
	t.Parallel()

	p := yanimaProvider(YanimaBase, testClient(t, "yanima"))

	_, err := p.GetEpisodes(context.Background(), YanimaBase+"/play/350")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "not characterized") {
		t.Errorf("error = %v, want the not-yet-characterized marker", err)
	}
}

func TestYanimaProviderMeta(t *testing.T) {
	t.Parallel()

	p := yanimaProvider(YanimaBase, testClient(t, "yanima"))
	if p.ID() != "yanima" || p.Name() != "Yanima" || p.BaseURL() != YanimaBase {
		t.Errorf("ID/Name/BaseURL = %q/%q/%q", p.ID(), p.Name(), p.BaseURL())
	}
	// Russian dub aggregator: wanted-language audio + video (task
	// brief pins SourceType BOTH).
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("SourceType = %q, want both", p.SourceType())
	}
	if p.ContentLanguage() != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", p.ContentLanguage())
	}
}

// --- parse helpers: fixture→shape pins ---

func TestParseYanimaVariantsFixture(t *testing.T) {
	t.Parallel()

	variants, err := parseYanimaVariants(fixture(t, "yanima_source.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(variants) != 3 {
		t.Fatalf("variants = %+v, want 3", variants)
	}
	if variants[0].quality != "1780" || variants[0].resolution != "2160" {
		t.Errorf("variants[0] = %+v, want quality 1780 resolution 2160", variants[0])
	}
}

func TestParseYanimaVariantsShapes(t *testing.T) {
	t.Parallel()

	t.Run("top-level array", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`[{"resolution":1080}]`))
		if err != nil || len(variants) != 1 || variants[0].resolution != "1080" {
			t.Errorf("variants = %+v err = %v", variants, err)
		}
	})
	t.Run("wrapper keys", func(t *testing.T) {
		for _, key := range []string{"sources", "variants", "qualities", "data", "results"} {
			body := fmt.Sprintf(`{%q:[{"quality":900,"resolution":1080}]}`, key)
			variants, err := parseYanimaVariants([]byte(body))
			if err != nil || len(variants) != 1 || variants[0].quality != "900" {
				t.Errorf("key %q: variants = %+v err = %v", key, variants, err)
			}
		}
	})
	t.Run("string numbers tolerated", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`{"sources":[{"quality":"1780","resolution":"2160"}]}`))
		if err != nil || variants[0].quality != "1780" || variants[0].resolution != "2160" {
			t.Errorf("variants = %+v err = %v", variants, err)
		}
	})
	t.Run("resolution without quality defaults quality", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`{"sources":[{"resolution":720}]}`))
		if err != nil || variants[0].quality != "720" {
			t.Errorf("variants = %+v err = %v", variants, err)
		}
	})
	t.Run("resolution object pins numeric (live-verified row shape)", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(
			`{"sources":[{"source":"Kodik","priority":2,"resolution":{"name":"720p","numeric":720},` +
				`"src":"https://kodik.example/hls/x/720.m3u8"}]}`))
		if err != nil || len(variants) != 1 {
			t.Fatalf("variants = %+v err = %v", variants, err)
		}
		if variants[0].resolution != "720" || variants[0].quality != "720" {
			t.Errorf("variants[0] = %+v, want the numeric label 720", variants[0])
		}
		if variants[0].src != "https://kodik.example/hls/x/720.m3u8" {
			t.Errorf("src = %q, want the SOURCE-disclosed m3u8", variants[0].src)
		}
	})
	t.Run("resolution object falls back to name", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`{"sources":[{"resolution":{"name":"720p"}}]}`))
		if err != nil || variants[0].resolution != "720p" {
			t.Errorf("variants = %+v err = %v, want the name fallback", variants, err)
		}
	})
	t.Run("rows without src stay manifest-only", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`{"sources":[{"quality":450,"resolution":720}]}`))
		if err != nil || variants[0].src != "" {
			t.Errorf("variants = %+v err = %v, want an empty src", variants, err)
		}
	})
	t.Run("items without resolution are skipped", func(t *testing.T) {
		variants, err := parseYanimaVariants([]byte(`{"sources":[{"codec":"avc"},{"resolution":480}]}`))
		if err != nil || len(variants) != 1 || variants[0].resolution != "480" {
			t.Errorf("variants = %+v err = %v", variants, err)
		}
	})
	t.Run("garbage fails loud", func(t *testing.T) {
		if _, err := parseYanimaVariants([]byte(`not json`)); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("err = %v, want ErrExtractFailed", err)
		}
		if _, err := parseYanimaVariants([]byte(`{"no":[1,2,3]}`)); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("err = %v, want ErrExtractFailed for a variantless shape", err)
		}
	})
}

func TestParseYanimaManifestFixture(t *testing.T) {
	t.Parallel()

	man, err := parseYanimaManifest(fixture(t, "yanima_manifest.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if man.url != "https://s3.yanima.space/anime/350/5/video/8842/2160/avc/index.m3u8" {
		t.Errorf("url = %q", man.url)
	}
	if man.token == "" || strings.Count(man.token, ".") != 2 {
		t.Errorf("token = %q, want the three-segment JWT", man.token)
	}
}

func TestParseYanimaManifestShapes(t *testing.T) {
	t.Parallel()

	t.Run("alias keys", func(t *testing.T) {
		for _, urlKey := range []string{"url", "file", "manifest", "m3u8", "src"} {
			for _, tokenKey := range []string{"token", "ym_", "jwt"} {
				body := fmt.Sprintf(`{%q:"https://s3/a/index.m3u8",%q:"j.w.t"}`, urlKey, tokenKey)
				man, err := parseYanimaManifest([]byte(body))
				if err != nil || man.url != "https://s3/a/index.m3u8" || man.token != "j.w.t" {
					t.Errorf("%s/%s: man = %+v err = %v", urlKey, tokenKey, man, err)
				}
			}
		}
	})
	t.Run("missing url fails loud", func(t *testing.T) {
		if _, err := parseYanimaManifest([]byte(`{"token":"j.w.t"}`)); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("err = %v, want ErrExtractFailed", err)
		}
	})
	t.Run("missing token fails loud", func(t *testing.T) {
		if _, err := parseYanimaManifest([]byte(`{"url":"https://s3/a/index.m3u8"}`)); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("err = %v, want ErrExtractFailed", err)
		}
	})
	t.Run("garbage fails loud", func(t *testing.T) {
		if _, err := parseYanimaManifest([]byte(`{`)); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("err = %v, want ErrExtractFailed", err)
		}
	})
}

// parseYanimaEmbed unit pins: the accepted page-URL grammar.
func TestParseYanimaEmbedShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw                  string
		wantID, wantEp       string
		wantQuality, wantRes string
	}{
		{"https://yanima.space/play/350?episode=5", "350", "5", "", ""},
		{"https://yanima.space/anime/350?episode=5", "350", "5", "", ""},
		{"https://yanima.space/play/350/5", "350", "5", "", ""},
		{"https://dev.yanima.space/play/350?episode=5", "350", "5", "", ""},
		{"https://yanima.space/play/350?episode=12&quality=1780&resolution=2160",
			"350", "12", "1780", "2160"},
	}
	for _, tc := range cases {
		ref, err := parseYanimaEmbed([]string{tc.raw})
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if ref.animeID != tc.wantID || ref.episode != tc.wantEp ||
			ref.quality != tc.wantQuality || ref.resolution != tc.wantRes {
			t.Errorf("%s: ref = %+v, want id=%s ep=%s q=%s res=%s",
				tc.raw, ref, tc.wantID, tc.wantEp, tc.wantQuality, tc.wantRes)
		}
	}

	for _, bad := range []string{
		"https://evil.example.com/play/350?episode=5",
		"https://yanima.space/watch/350?episode=5", // unknown route
		"https://yanima.space/play/?episode=5",     // no id
		"https://yanima.space/play/350",            // no episode anywhere
		"://broken",
	} {
		if _, err := parseYanimaEmbed([]string{bad}); !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("%s: err = %v, want ErrExtractFailed", bad, err)
		}
	}
}

// parseYanimaEmbed tries every candidate embed until one parses.
func TestParseYanimaEmbedTriesCandidates(t *testing.T) {
	t.Parallel()

	ref, err := parseYanimaEmbed([]string{
		"https://evil.example.com/play/350?episode=5",
		"https://yanima.space/play/350?episode=5",
	})
	if err != nil || ref.animeID != "350" {
		t.Errorf("ref = %+v err = %v, want the second candidate to parse", ref, err)
	}
}

// guard: the fixtures stay machine-checkable JSON shapes.
func TestYanimaFixturesDecode(t *testing.T) {
	t.Parallel()

	var v any
	if err := json.Unmarshal(fixture(t, "yanima_source.json"), &v); err != nil {
		t.Errorf("yanima_source.json: %v", err)
	}
	if err := json.Unmarshal(fixture(t, "yanima_source_kodik.json"), &v); err != nil {
		t.Errorf("yanima_source_kodik.json: %v", err)
	}
	if err := json.Unmarshal(fixture(t, "yanima_manifest.json"), &v); err != nil {
		t.Errorf("yanima_manifest.json: %v", err)
	}
}
