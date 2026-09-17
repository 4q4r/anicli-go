package providers

// AllAnime v3 protocol tests: the aaEnv fake world now models the
// client-crypto bootstrap (x-aa-boot validation against the
// golden-pinned port), the POST-only GraphQL transport (with aaReq +
// x-build-id on episode queries) and tobeparsed responses. Live
// fixtures under testdata/allanime carry [LIVE-VERIFIED 2026-09-13]
// provenance.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// aaEnv is the fake world of the AllAnime v3 protocol: the GraphQL API
// (bootstrap endpoint + query endpoint with aaReq verification +
// tobeparsed responses) and the stream host (clock.json + m3u8).
type aaEnv struct {
	t *testing.T

	api     *httptest.Server
	stream  *httptest.Server
	apiHost string // referer host folded into x-aa-boot (gT)

	// bootstrap material (fixture defaults = live epoch-2958 capture)
	mu          sync.Mutex
	partB       string
	rotated     bool // rotate on the NEXT bootstrap hit (after set)
	epoch       int64
	switchAt    int64
	bootHits    int
	bootAnswers int // 0 = healthy; 1 = 400 (unknown build)
	// activeBuildID is the buildId the fake API derives its key with
	// (tests flip it when the bridge discovers a new build).
	activeBuildID string

	// episode-source crypto gating: rejectNoAAReq makes the query
	// endpoint answer AA_CRYPTO_MISSING_BUILD unless x-build-id +
	// aaReq arrive.
	rejectNoAAReq bool

	apiRequests  atomic.Int32
	lastBody     atomic.Value // string: last query-endpoint body
	lastAPIHdr   atomic.Value // http.Header
	apiBehaviors []func(r *http.Request, w http.ResponseWriter) bool

	clockHits atomic.Int32
	// lastMasterReferer records the Referer of the last /master.m3u8
	// fetch (F30: the playlist must be fetched with the clock URL as
	// Referer).
	lastMasterReferer atomic.Value

	nowMillis int64
}

// newAAEnv builds the fake world with the live epoch-2958 bootstrap
// material (key = cy("168") XOR live partB — the golden-pinned key).
func newAAEnv(t *testing.T) *aaEnv {
	t.Helper()

	env := &aaEnv{
		t:         t,
		partB:     "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=",
		epoch:     2958,
		switchAt:  4102444800000,
		nowMillis: 1789000000000,
	}

	env.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.lastAPIHdr.Store(r.Header.Clone())
		switch {
		case strings.HasPrefix(r.URL.Path, "/client-crypto/v1/bootstrap"):
			env.mu.Lock()
			env.bootHits++
			answers := env.bootAnswers
			if env.rotated {
				env.partB = base64.StdEncoding.EncodeToString(mkBytes(31))
				env.rotated = false
			}
			partB, epoch, switchAt := env.partB, env.epoch, env.switchAt
			env.mu.Unlock()
			if answers == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING_BUILD"}]}`))
				return
			}
			// Validate x-aa-boot against the golden-pinned port for one
			// of the epoch candidates of the fixed clock.
			boot := r.Header.Get("x-aa-boot")
			mask := mustAAMask(t, r.URL.Query().Get("buildId"))
			ok := false
			for _, cand := range aaEpochCandidates(env.nowMillis) {
				want, err := aaBootHeader(mask, aaBootParams{
					Lane: "k7", BuildID: r.URL.Query().Get("buildId"),
					Group: aaKeyGroup(env.apiHost), Host: env.apiHost, Epoch: cand,
				})
				if err != nil {
					t.Errorf("aaBootHeader: %v", err)
				}
				if want == boot {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("bootstrap x-aa-boot %q matches no candidate", boot)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch": epoch, "partB": partB, "switchAt": switchAt, "k": "k7",
			})
			return

		case r.URL.Path == "/api":
			var req aaGraphqlRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			env.lastBody.Store(req.Query)
			for _, pre := range env.apiBehaviors {
				if pre(r, w) {
					return
				}
			}
			query := req.Query
			switch {
			case strings.Contains(query, "shows("):
				_, _ = w.Write([]byte(`{"data":{"shows":{"edges":[` +
					`{"_id":"id1","name":"Naruto","thumbnail":"https://img/n.png"},` +
					`{"_id":"id2","name":"Boruto","thumbnail":"https://img/b.png"}]}}}`))
			case strings.Contains(query, "availableEpisodesDetail"):
				_, _ = w.Write([]byte(`{"data":{"show":{"_id":"id1","availableEpisodesDetail":` +
					`{"sub":["2","1"],"dub":["1","10.5"]}}}}`))
			case strings.Contains(query, "episode("):
				env.apiRequests.Add(1)
				// Live server contract: aaReq + k + persistedQuery in
				// extensions, x-build-id header, else BUILD error.
				if env.rejectNoAAReq {
					if req.Extensions == nil || req.Extensions["aaReq"] == nil || r.Header.Get("x-build-id") == "" {
						_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING_BUILD","extensions":{"code":"AA_CRYPTO_MISSING_BUILD"}}]}`))
						return
					}
				}
				plain := `{"episode":{"episodeString":"1","sourceUrls":[` +
					fmt.Sprintf(`{"sourceUrl":"--%s","sourceName":"S-mp4"}]}}}`, aaHexEnc("/all/manga/clock?w=1"))
				key := env.apiKey()
				_, _ = fmt.Fprintf(w, `{"data":{"_m":"b7","tobeparsed":%q}`,
					aaSealBlob(t, key, plain))
			default:
				http.Error(w, "unknown query", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.api.Close)
	env.apiHost = strings.TrimPrefix(env.api.URL, "http://")

	// --- stream host: clock.json + m3u8 ---
	env.stream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.clockHits.Add(1)
		switch r.URL.Path {
		case "/all/manga/clock.json":
			_, _ = fmt.Fprintf(w, `{"links":[`+
				`{"link":%q,"hls":true,"resolution":"1080","subtitles":[]}]}`,
				env.stream.URL+"/master.m3u8")
		case "/master.m3u8":
			// F30 server-side assertion: a real CDN rejects playlist
			// fetches without the clock-URL Referer.
			if ref := r.Header.Get("Referer"); !strings.HasPrefix(ref, env.stream.URL+"/all/manga/clock.json") {
				http.Error(w, "wrong referer", http.StatusForbidden)
				return
			}
			env.lastMasterReferer.Store(r.Header.Get("Referer"))
			_, _ = w.Write([]byte("#EXTM3U\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n" +
				"1080.m3u8\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=2000,RESOLUTION=1280x720\n" +
				"720.m3u8\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.stream.Close)

	return env
}

// apiKey derives the key the fake API encrypts with (mask XOR partB
// for the active buildId).
func (e *aaEnv) apiKey() []byte {
	e.mu.Lock()
	partB, buildID := e.partB, e.activeBuildID
	e.mu.Unlock()
	if buildID == "" {
		buildID = "168"
	}
	key, err := aaDeriveKeyMaterial(mustAAMask(e.t, buildID), partB)
	if err != nil {
		e.t.Fatalf("apiKey: %v", err)
	}
	return key
}

// bootHitCount reports the bootstrap request count under the mutex.
func (e *aaEnv) bootHitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bootHits
}

// provider wires the AllAnime provider against the fake world (no
// bridge, memory-only buildId cache, fixed clock).
func (e *aaEnv) provider() *AllAnime {
	return e.providerWithBridge(nil)
}

// providerWithBridge wires the provider with a bridge seam.
func (e *aaEnv) providerWithBridge(bridge aaBridgeSource) *AllAnime {
	p := newAllAnime(e.api.URL+"/api", "http://"+e.apiHost, e.stream.URL, testClient(e.t, "allanime"), bridge, "")
	p.material = newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: e.api.URL + "/client-crypto/v1/bootstrap",
		Referer:       "http://" + e.apiHost,
		RefererHost:   e.apiHost,
		Lane:          aaContentLane,
		BuildID:       func() (string, error) { return "168", nil },
		HTTP:          p.http,
		Now:           func() time.Time { return time.UnixMilli(e.nowMillis) },
	})
	return p
}

// aaHexEnc is the inverse of the Python "--" decoder (chr ^ 56).
func aaHexEnc(s string) string {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = append(out, []byte(fmt.Sprintf("%02x", int(r)^56))...)
	}
	return string(out)
}

func TestAllAnimeSearch(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	p := env.provider()

	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// Similarity sort: "Naruto" (1.0) before "Boruto".
	if results[0].Title != "Naruto" || results[0].URL != "id1" {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[0].SourceID != "allanime" || results[0].Poster != "https://img/n.png" {
		t.Errorf("results[0] meta wrong: %+v", results[0])
	}

	// POST body shape: live search variables incl. allowAdult/
	// allowUnknown/limit 40/translationType sub.
	body := env.lastBody.Load().(string)
	if !strings.Contains(body, "shows(") || !strings.Contains(body, "englishName") {
		t.Errorf("search query doc = %q, want the live document (incl englishName)", body)
	}
	hdr, _ := env.lastAPIHdr.Load().(http.Header)
	if hdr == nil || hdr.Get("Referer") != "http://"+env.apiHost || hdr.Get("Origin") != "http://"+env.apiHost {
		t.Errorf("search headers = %v", hdr)
	}
}

// TestAllAnimeSearchLiveFixture pins the decode against the live-captured
// search response (ROAD OF NARUTO). [LIVE-VERIFIED 2026-09-13].
func TestAllAnimeSearchLiveFixture(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if strings.Contains(r.URL.Path, "/api") && !strings.Contains(r.URL.Path, "bootstrap") {
			_, _ = w.Write([]byte(aaFixture(t, "search_live_road_of_naruto.json")))
			return true
		}
		return false
	})
	p := env.provider()

	results, err := p.Search(context.Background(), "road of naruto")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	want := contracts.SearchResult{
		Title:    "ROAD OF NARUTO",
		URL:      "2oXgpDPd3xKWdgnoz",
		SourceID: "allanime",
		Poster:   "https://s4.anilist.co/file/anilistcdn/media/anime/cover/large/bx155348-4ipji8SvTjyh.jpg",
	}
	got := results[0]
	if got.Title != want.Title || got.URL != want.URL || got.SourceID != want.SourceID || got.Poster != want.Poster {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func TestAllAnimeSearchTransportFailureSilent(t *testing.T) {
	t.Parallel()

	// Dead endpoint: Python's except returned [] (allanime.py:116-117).
	p := newAllAnime("http://"+deadAddr(t)+"/api", "http://"+deadAddr(t), "https://allanime.day", testClient(t, "allanime"), nil, "")
	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search transport error must be silent, got %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

// TestAllAnimeEpisodesLiveFixture pins the episodes decode against the
// live capture. [LIVE-VERIFIED 2026-09-13].
func TestAllAnimeEpisodesLiveFixture(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(aaFixture(t, "episodes_live_road_of_naruto.json")))
		return true
	})
	p := env.provider()

	episodes, err := p.GetEpisodes(context.Background(), "2oXgpDPd3xKWdgnoz")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].Num != "1" {
		t.Fatalf("episodes = %+v, want episode 1 only", episodes)
	}
	if _, ok := episodes[0].RawEmbeds["sub"]; !ok {
		t.Error("episode 1 missing sub embed")
	}
	if _, ok := episodes[0].RawEmbeds["dub"]; ok {
		t.Error("episode 1 has phantom dub embed (live dub list is empty)")
	}
}

func TestAllAnimeGetEpisodes(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	p := env.provider()

	episodes, err := p.GetEpisodes(context.Background(), "id1")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	// sub {2,1} union dub {1,10.5} = {1,2,10.5}, float-sorted.
	if len(episodes) != 3 {
		t.Fatalf("episodes = %d, want 3", len(episodes))
	}
	wantNums := []string{"1", "2", "10.5"}
	for i, want := range wantNums {
		if episodes[i].Num != want {
			t.Errorf("episodes[%d].Num = %q, want %q", i, episodes[i].Num, want)
		}
	}
	if _, ok := episodes[0].RawEmbeds["sub"]; !ok {
		t.Error("episode 1 missing sub embed")
	}
	if _, ok := episodes[0].RawEmbeds["dub"]; !ok {
		t.Error("episode 1 missing dub embed")
	}
	if _, ok := episodes[1].RawEmbeds["dub"]; ok {
		t.Error("episode 2 has phantom dub embed")
	}
}

// TestAllAnimeResolveStreamRoundTrip is the full v3 protocol round
// trip: bootstrap (x-aa-boot validation), aaReq token + x-build-id on
// the episode POST, tobeparsed decryption, "--" URL decode, clock.json
// fetch and m3u8 variant resolution.
func TestAllAnimeResolveStreamRoundTrip(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "sub" {
		t.Errorf("DubName = %q", stream.DubName)
	}
	if got := env.apiRequests.Load(); got != 1 {
		t.Errorf("episode requests = %d, want 1", got)
	}

	// clock.json was fetched through the decoded internal URL.
	if env.clockHits.Load() < 2 { // clock.json + master.m3u8
		t.Fatalf("stream host hits = %d, want clock.json + playlist fetches", env.clockHits.Load())
	}
	l1080, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("links = %#v, want a 1080 entry", stream.Links)
	}
	if want := env.stream.URL + "/1080.m3u8"; l1080.URL != want {
		t.Errorf("1080 URL = %q, want %q", l1080.URL, want)
	}
	wantReferer := env.stream.URL + "/all/manga/clock.json?w=1"
	if l1080.Headers["Referer"] != wantReferer {
		t.Errorf("1080 Referer = %q, want %q", l1080.Headers["Referer"], wantReferer)
	}
	if got := env.lastMasterReferer.Load(); got == nil || got.(string) != wantReferer {
		t.Errorf("master.m3u8 fetch Referer = %v, want %q (F30)", got, wantReferer)
	}
	if _, ok := stream.Links["720"]; !ok {
		t.Error("no 720 link from the second variant")
	}
}

// TestAllAnimeResolveStreamDirectPlayer pins the live v3 direct-media
// shape: type "player" sources (Yt-mp4) become links without a clock
// fetch. Vector from the live plaintext fixture.
// [LIVE-VERIFIED 2026-09-13].
func TestAllAnimeResolveStreamDirectPlayer(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		plain := `{"episode":{"episodeString":"1","sourceUrls":[` +
			`{"sourceUrl":"https://tools.fast4speed.rsvp/media9/videos/x/sub/1?Authorization=1","sourceName":"Yt-mp4","type":"player","fallBack":"mp4"},` +
			`{"sourceUrl":"https://ok.ru/videoembed/1","sourceName":"Ok","type":"iframe"}]}}`
		_, _ = fmt.Fprintf(w, `{"data":{"tobeparsed":%q}`, aaSealBlob(t, env.apiKey(), plain))
		return true
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	l, ok := stream.Links["1080"]
	if !ok || l.URL != "https://tools.fast4speed.rsvp/media9/videos/x/sub/1?Authorization=1" {
		t.Fatalf("direct player link = %+v ok=%v", l, ok)
	}
	if env.clockHits.Load() != 0 {
		t.Error("direct player source must skip the clock flow")
	}
	if len(stream.Links) != 1 {
		t.Errorf("iframe embed leaked into links: %#v", stream.Links)
	}
}

// TestAllAnimeResolveStreamKeyRefreshOnRotation pins the AA_CRYPTO
// retry: the server rejects the first token as stale and rotates
// server-side; the provider refreshes the material (second bootstrap)
// and retries with a fresh token.
func TestAllAnimeResolveStreamKeyRefreshOnRotation(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	var rejections, posts atomic.Int32
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		posts.Add(1)
		if rejections.Add(1) == 1 {
			// Reject as stale AND arm the server-side rotation for the
			// forced material refresh.
			env.mu.Lock()
			env.rotated = true
			env.mu.Unlock()
			_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING","extensions":{"code":"AA_CRYPTO_MISSING"}}]}`))
			return true
		}
		return false
	})
	p := env.provider()

	if _, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub"); err != nil {
		t.Fatalf("ResolveStream after rotation: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("episode requests = %d, want 2 (initial + retried)", got)
	}
	if got := env.bootHitCount(); got != 2 {
		t.Errorf("bootstrap hits = %d, want 2 (initial + forced refresh)", got)
	}
}

// TestAllAnimeResolveStreamRateLimitRetry pins the live rate-limit
// contract of the episode query: the site answers "Too many requests,
// please try again in 5 seconds." (and NEED_CAPTCHA) when the client
// resolves too fast — observed live 2026-09-17 on concurrent resolves.
// The provider must back off once (aaRateLimitBackoff) and retry the
// SAME query; a persisting limit still fails loud, with exactly one
// retry (no hammering).
func TestAllAnimeResolveStreamRateLimitRetry(t *testing.T) {
	t.Parallel()

	// Per-instance override: a package-level knob would race with the
	// package's parallel tests under -race (PR45 gate lesson).
	const fastBackoff = 5 * time.Millisecond

	t.Run("recovers after backoff", func(t *testing.T) {
		t.Parallel()
		env := newAAEnv(t)
		var posts, limits atomic.Int32
		env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
			if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
				return false
			}
			posts.Add(1)
			if limits.Add(1) == 1 {
				// Verbatim live body (2026-09-17).
				_, _ = w.Write([]byte(`{"errors":[{"message":"Too many requests, please try again in 5 seconds.","extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`))
				return true
			}
			return false
		})
		p := env.provider()
		p.rateLimitBackoff = fastBackoff

		stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
		if err != nil {
			t.Fatalf("ResolveStream after one rate-limit: %v", err)
		}
		if len(stream.Links) == 0 {
			t.Fatal("no links after rate-limit retry")
		}
		if got := posts.Load(); got != 2 {
			t.Errorf("episode requests = %d, want 2 (initial + one retry)", got)
		}
	})

	t.Run("NEED_CAPTCHA as transient limiter", func(t *testing.T) {
		t.Parallel()
		env := newAAEnv(t)
		var posts, limits atomic.Int32
		env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
			if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
				return false
			}
			posts.Add(1)
			if limits.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"errors":[{"message":"NEED_CAPTCHA","extensions":{"code":"NEED_CAPTCHA"}}]}`))
				return true
			}
			return false
		})
		p := env.provider()
		p.rateLimitBackoff = fastBackoff

		stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
		if err != nil {
			t.Fatalf("ResolveStream after transient NEED_CAPTCHA: %v", err)
		}
		if len(stream.Links) == 0 {
			t.Fatal("no links after NEED_CAPTCHA retry")
		}
		if got := posts.Load(); got != 2 {
			t.Errorf("episode requests = %d, want 2", got)
		}
	})

	t.Run("persisting limit fails loud with one retry", func(t *testing.T) {
		t.Parallel()
		env := newAAEnv(t)
		var posts atomic.Int32
		env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
			if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
				return false
			}
			posts.Add(1)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Too many requests, please try again in 5 seconds.","extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}`))
			return true
		})
		p := env.provider()
		p.rateLimitBackoff = fastBackoff

		_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
		if err == nil {
			t.Fatal("persisting rate limit must fail loud")
		}
		if !errors.Is(err, contracts.ErrExtractFailed) {
			t.Errorf("error = %v, want ErrExtractFailed chain", err)
		}
		if got := posts.Load(); got != 2 {
			t.Errorf("episode requests = %d, want exactly 2 (initial + one retry)", got)
		}
	})
}

// TestAllAnimeResolveStreamLoudOnCryptoFailure pins the no-silent-empty
// policy: when the material never works (no bridge), resolve fails
// with the typed rotation error chained under ErrExtractFailed.
func TestAllAnimeResolveStreamLoudOnCryptoFailure(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_EXPIRED","extensions":{"code":"AA_CRYPTO_EXPIRED"}}]}`))
		return true
	})
	p := env.provider()

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err == nil {
		t.Fatal("crypto failure swallowed into an empty stream")
	}
	if !errors.Is(err, errAACryptoRotated) {
		t.Fatalf("err = %v, want errAACryptoRotated chain", err)
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed chain", err)
	}
}

// TestAllAnimeResolveStreamBridgeOnBuildMismatch pins the bridge
// ladder: a rejected buildId triggers one bridge handoff; the
// bridge-derived buildId is adopted, persisted in the cache and the
// resolve retried once; a failing bridge escalates to the typed error.
func TestAllAnimeResolveStreamBridgeOnBuildMismatch(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	// The fake server "already runs" build 999: it rejects the pinned
	// 168 bootstrap and encrypts with build-999 material from the start.
	env.mu.Lock()
	env.bootAnswers = 1
	env.activeBuildID = "999"
	env.mu.Unlock()
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if strings.Contains(r.URL.Path, "/client-crypto/v1/bootstrap") {
			if r.URL.Query().Get("buildId") == "999" {
				env.mu.Lock()
				env.bootAnswers = 0
				env.mu.Unlock()
			}
			return false // fall through to the default handler
		}
		if r.Header.Get("x-build-id") != "999" {
			_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING_BUILD","extensions":{"code":"AA_CRYPTO_MISSING_BUILD"}}]}`))
			return true
		}
		return false
	})

	dir := t.TempDir()
	buildIDs := newAABuildIDCache(dir)
	bridge := &aaFakeBridge{material: aaBridgeMaterial{
		BuildID: "999", Epoch: 2958, PartB: "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=",
	}}
	p := newAllAnime(env.api.URL+"/api", "http://"+env.apiHost, env.stream.URL, testClient(t, "allanime"), bridge, dir)
	p.buildIDs = buildIDs
	p.material = newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: env.api.URL + "/client-crypto/v1/bootstrap",
		Referer:       "http://" + env.apiHost,
		RefererHost:   env.apiHost,
		Lane:          aaContentLane,
		BuildID:       func() (string, error) { return buildIDs.Load(), nil },
		HTTP:          p.http,
		Now:           func() time.Time { return time.UnixMilli(env.nowMillis) },
	})

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream via bridge: %v", err)
	}
	if _, ok := stream.Links["1080"]; !ok {
		t.Fatalf("no links via bridge: %#v", stream.Links)
	}
	if bridge.calls != 1 {
		t.Errorf("bridge calls = %d, want 1", bridge.calls)
	}
	if got := buildIDs.Load(); got != "999" {
		t.Errorf("buildId cache = %q, want persisted 999", got)
	}
}

// TestAllAnimeResolveStreamBridgeFailureLoud pins the failure mode: a
// bridge that also fails leaves a typed error, never an empty stream.
func TestAllAnimeResolveStreamBridgeFailureLoud(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.rejectNoAAReq = true
	env.mu.Lock()
	env.bootAnswers = 1
	env.mu.Unlock()
	bridge := &aaFakeBridge{err: errors.New("browser exploded")}
	p := env.providerWithBridge(bridge)

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err == nil {
		t.Fatal("bridge failure swallowed")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed chain", err)
	}
	if !strings.Contains(err.Error(), "browser exploded") {
		t.Errorf("err = %v, want the bridge cause chained", err)
	}
}

// TestAllAnimeResolveStreamEmptyEpisode pins the null-episode outcome:
// no sources, empty MediaStream, no error.
func TestAllAnimeResolveStreamEmptyEpisode(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		_, _ = w.Write([]byte(`{"data":{"episode":null}}`))
		return true
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(stream.Links) != 0 {
		t.Fatalf("links = %#v, want empty", stream.Links)
	}
}

// TestAllAnimeResolveStreamNeedCaptchaTyped pins [I3]: a GraphQL
// errors[] body carrying NEED_CAPTCHA (and no episode data) must
// surface the typed errAACaptcha — never a silent empty stream — and
// must NOT trigger the browser bridge (a captcha verdict is not a
// crypto rotation).
func TestAllAnimeResolveStreamNeedCaptchaTyped(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		_, _ = w.Write([]byte(`{"errors":[{"message":"NEED_CAPTCHA","extensions":{"code":"NEED_CAPTCHA"}}]}`))
		return true
	})
	bridge := &aaFakeBridge{}
	p := env.providerWithBridge(bridge)

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err == nil {
		t.Fatal("NEED_CAPTCHA collapsed to a silent empty stream")
	}
	if !errors.Is(err, errAACaptcha) {
		t.Fatalf("err = %v, want errAACaptcha chain", err)
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed chain", err)
	}
	if bridge.calls != 0 {
		t.Errorf("bridge calls = %d, want 0 (captcha must not burn a bridge session)", bridge.calls)
	}
}

// TestAllAnimeResolveStreamGraphQLErrorsLoud pins [I3]: any other
// GraphQL errors[] body without episode data fails loudly (wrapped
// into the contracts family by ResolveStream) instead of returning an
// empty stream.
func TestAllAnimeResolveStreamGraphQLErrorsLoud(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if !strings.Contains(r.URL.Path, "/api") || strings.Contains(r.URL.Path, "bootstrap") {
			return false
		}
		_, _ = w.Write([]byte(`{"errors":[{"message":"internal server error"},{"message":"second failure"}]}`))
		return true
	})
	p := env.provider()

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err == nil {
		t.Fatal("graphql errors[] collapsed to a silent empty stream")
	}
	if errors.Is(err, errAACaptcha) {
		t.Fatalf("err = %v, generic errors must not classify as captcha", err)
	}
	if errors.Is(err, errAACryptoRotated) {
		t.Fatalf("err = %v, generic errors must not classify as crypto rotation", err)
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed chain", err)
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("err = %v, want the server message chained", err)
	}
}

// TestAAPrioritizeSources pins the ani-cli provider priority
// (Default > S-mp4 > Luf-Mp4 > Yt-mp4, then response order).
func TestAAPrioritizeSources(t *testing.T) {
	t.Parallel()

	got := aaPrioritizeSources([]aaSource{
		{Name: "Filemoon", URL: "1"},
		{Name: "S-mp4", URL: "2"},
		{Name: "Default", URL: "3"},
		{Name: "Yt-mp4", URL: "4"},
		{Name: "Luf-Mp4", URL: "5"},
		{Name: "Kuro", URL: "6"},
	})
	want := []string{"3", "2", "5", "4", "1", "6"}
	for i, url := range want {
		if got[i].URL != url {
			t.Fatalf("priority[%d].URL = %s, want %s (%+v)", i, got[i].URL, url, got)
		}
	}
}

// TestAABridgeScriptShape sanity-checks the bridge extraction script
// (compiles as the IIFE wrapper, returns a JSON string).
func TestAABridgeScriptShape(t *testing.T) {
	t.Parallel()

	// The crypto-chunk marker must be the stable ST error literal, not
	// the bootPrefix (which is char-coded in some builds — verified
	// live 2026-09-13 when the source representation rotated).
	if !strings.Contains(aaBridgeExtractionJS, "invalid_part_b") {
		t.Error("extraction script lost the crypto-chunk marker")
	}
	if !strings.Contains(aaBridgeExtractionJS, "return JSON.stringify(__out)") {
		t.Error("extraction script lost the JSON return")
	}
	for _, closure := range []string{"cy", "iT", "gT", "mT", "gy", "sd"} {
		if !strings.Contains(aaBridgeExtractionJS, closure) {
			t.Errorf("extraction script does not extract %s", closure)
		}
	}
	// C1 regression guard: the const is a Go RAW string — backslashes
	// pass through verbatim, so a doubled `\\` in the Go source reaches
	// the JS engine as an escaped backslash. Inside a regex literal
	// (`/https?:\/\//` written as `\\/`) the bare second slash
	// TERMINATES the regex and the whole script dies with a SyntaxError
	// at parse. The emitted script must contain single backslashes
	// only (`\s`, `\/`, `\.`, `\n` …), never two in a row.
	if strings.Contains(aaBridgeExtractionJS, `\\`) {
		t.Error("extraction script contains a doubled backslash (raw-string escape leak) — regexes and join('\\n') would be broken in JS")
	}
}

// TestAABridgeScriptParsesUnderNode proves the emitted script is valid
// JavaScript: it materializes the exact IIFE Go evaluates in the page
// into a temp file and parses it with `node --check`. Skipped when node
// is not installed (the shape test above still guards the escape
// class). [C1]
func TestAABridgeScriptParsesUnderNode(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; script parse check skipped")
	}
	script := fmt.Sprintf("(async () => { const LANE = %q; %s })()", "k7", aaBridgeExtractionJS)
	path := filepath.Join(t.TempDir(), "bridge_extraction.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput() //nolint:gosec // test-only: node resolved from PATH, script path inside t.TempDir()
	if err != nil {
		t.Fatalf("node --check rejected the emitted bridge script (SyntaxError class): %v\n%s", err, out)
	}
}

// TestAAParseBridgeReply pins the bridge reply decode.
func TestAAParseBridgeReply(t *testing.T) {
	t.Parallel()

	mask := mustAAMask(t, "168")
	mat, err := parseAABridgeReply(`{"ok":true,"buildId":"168","epoch":2958,` +
		`"partB":"lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=",` +
		`"mask":"` + base64.StdEncoding.EncodeToString(mask) + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if mat.BuildID != "168" || mat.Epoch != 2958 || len(mat.Mask) != 32 {
		t.Errorf("material = %+v", mat)
	}
	if _, err := parseAABridgeReply(`{"ok":false,"error":"boom"}`); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("error reply = %v, want boom", err)
	}
	if _, err := parseAABridgeReply(`not json`); err == nil {
		t.Error("garbage reply accepted")
	}
}

// aaFakeBridge is the test bridge seam.
type aaFakeBridge struct {
	material aaBridgeMaterial
	err      error
	calls    int
}

func (b *aaFakeBridge) ExtractCrypto(context.Context) (aaBridgeMaterial, error) {
	b.calls++
	if b.err != nil {
		return aaBridgeMaterial{}, b.err
	}
	return b.material, nil
}

// deadAddr returns an address that refuses connections instantly.
func deadAddr(t *testing.T) string {
	t.Helper()
	l := newDeadListener(t)
	return l.Addr().String()
}
