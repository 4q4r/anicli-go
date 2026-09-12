package providers

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// aaEnv is the four-server fake world of the AllAnime protocol: the
// mkissa.to referer page (epoch + partB + entry bundle), the CDN
// (entry bundle + mask chunk), the GraphQL API (aaReq verification +
// tobeparsed responses) and the stream host (clock.json + m3u8).
type aaEnv struct {
	t               *testing.T
	referer         *httptest.Server
	cdn             *httptest.Server
	api             *httptest.Server
	stream          *httptest.Server
	refererHits     atomic.Int32
	maskHex         string
	partB           []byte
	maskB           []byte
	apiKey          func() []byte // key the API encrypts tobeparsed with
	apiRequests     atomic.Int32
	episodeAttempts atomic.Int32 // every episode-sources request (extensions present)
	lastExt         atomic.Value // string: extensions param of last episode call
	lastVars        atomic.Value // string: variables param of last episode call
	lastQuery       atomic.Value // string: query param of last episode call
	lastAPIHdr      atomic.Value // http.Header: last API request header
	apiBehaviors    []func(r *http.Request, w http.ResponseWriter) bool
	clockHits       atomic.Int32
	// lastMasterReferer records the Referer of the last /master.m3u8
	// fetch; masterForbidden counts requests the fake CDN rejected for
	// a wrong Referer (F30: the playlist must be fetched with the clock
	// URL as Referer, Python allanime.py:233-236).
	lastMasterReferer atomic.Value
	masterForbidden   atomic.Int32
	mu                sync.Mutex // guards referer page mutation below
	refererBody       atomic.Value
}

// aaHexEnc is the inverse of the Python "--" decoder (chr ^ 56).
func aaHexEnc(s string) string {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = append(out, []byte(fmt.Sprintf("%02x", int(r)^56))...)
	}
	return string(out)
}

// newAAEnv builds the fake world. keyMask/keyPartB derive key K1; the
// referer page initially serves epoch 4130 with that material.
func newAAEnv(t *testing.T) *aaEnv {
	t.Helper()

	env := &aaEnv{t: t}
	env.maskB = make([]byte, 32)
	env.partB = make([]byte, 32)
	for i := range env.maskB {
		env.maskB[i] = byte(40 + i)
		env.partB[i] = byte(170 - i)
	}
	env.maskHex = hex.EncodeToString(env.maskB)
	key1 := make([]byte, 32)
	for i := range key1 {
		key1[i] = env.maskB[i] ^ env.partB[i]
	}
	cur := key1
	env.apiKey = func() []byte {
		env.mu.Lock()
		defer env.mu.Unlock()
		return cur
	}

	// --- CDN: entry bundle importing one mask chunk ---
	env.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/all/mk/_app/immutable/entry/app.x1.js":
			_, _ = fmt.Fprint(w, `import{a}from"../chunks/one.js";import{b}from"../chunks/two.js";`)
		case "/all/mk/_app/immutable/chunks/one.js":
			_, _ = fmt.Fprintf(w, `export const m=%q;`, env.maskHex)
		case "/all/mk/_app/immutable/chunks/two.js":
			_, _ = w.Write([]byte(`export const unused=1;`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.cdn.Close)

	// --- referer page: epoch + partB + entry bundle URL ---
	env.refererBody.Store(fmt.Sprintf(
		`<html><script>var __ssr={"epoch":4130,"partB":%q};</script>`+
			`<script defer src=%q></script></html>`,
		base64.StdEncoding.EncodeToString(env.partB),
		env.cdn.URL+"/all/mk/_app/immutable/entry/app.x1.js"))
	env.referer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.refererHits.Add(1)
		_, _ = w.Write([]byte(env.refererBody.Load().(string)))
	}))
	t.Cleanup(env.referer.Close)

	// --- API ---
	env.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.lastAPIHdr.Store(r.Header.Clone())
		if r.URL.Query().Get("extensions") != "" {
			env.episodeAttempts.Add(1)
			env.lastExt.Store(r.URL.Query().Get("extensions"))
		}
		if r.URL.Query().Get("variables") != "" {
			env.lastVars.Store(r.URL.Query().Get("variables"))
		}
		if r.URL.Query().Get("query") != "" {
			env.lastQuery.Store(r.URL.Query().Get("query"))
		}
		for _, pre := range env.apiBehaviors {
			if pre(r, w) {
				return
			}
		}
		query := r.URL.Query().Get("query")
		switch {
		case strings.Contains(query, "shows("):
			_, _ = w.Write([]byte(`{"data":{"shows":{"edges":[` +
				`{"_id":"id1","name":"Naruto","thumbnail":"https://img/n.png","availableEpisodes":{"sub":1,"dub":1}},` +
				`{"_id":"id2","name":"Boruto","thumbnail":"https://img/b.png","availableEpisodes":{"sub":2}}]}}}`))
		case strings.Contains(query, "availableEpisodesDetail"):
			_, _ = w.Write([]byte(`{"data":{"show":{"_id":"id1","availableEpisodesDetail":` +
				`{"sub":["2","1"],"dub":["1","10.5"]}}}}`))
		default:
			// Episode-sources query (persisted or full).
			env.apiRequests.Add(1)
			ext := r.URL.Query().Get("extensions")
			if ext == "" || !strings.Contains(ext, "aaReq") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING"}]}`))
				return
			}
			plain := `{"data":{"episode":{"episodeString":"1","sourceUrls":[` +
				fmt.Sprintf(`{"sourceUrl":"--%s","sourceName":"S-mp4"}`, aaHexEnc("/all/manga/clock?w=1")) +
				`]}}}`
			_, _ = fmt.Fprintf(w, `{"data":{"_m":"b7","tobeparsed":%q}`,
				aaTestSeal(t, env.apiKey(), plain))
		}
	}))
	t.Cleanup(env.api.Close)

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
			// fetches without the clock-URL Referer (Python parity
			// allanime.py:233-236).
			if ref := r.Header.Get("Referer"); !strings.HasPrefix(ref, env.stream.URL+"/all/manga/clock.json") {
				env.masterForbidden.Add(1)
				http.Error(w, "wrong referer", http.StatusForbidden)
				return
			}
			env.lastMasterReferer.Store(r.Header.Get("Referer"))
			_, _ = w.Write([]byte("#EXTM3U\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n" +
				"1080.m3u8\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\n" +
				"720.m3u8\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(env.stream.Close)

	return env
}

// provider wires the AllAnime provider against the fake world.
func (e *aaEnv) provider() *AllAnime {
	return newAllAnime(e.api.URL+"/api", e.referer.URL, e.stream.URL, testClient(e.t, "allanime"))
}

// encryptWith seals plain with the given key (test-side oracle).
func aaEncryptWith(t *testing.T, key []byte, plain string) string {
	t.Helper()
	return aaTestSeal(t, key, plain)
}

func TestAllAnimeSearch(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
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

	vars := env.lastVars.Load().(string)
	if !strings.Contains(vars, `"limit":26`) || !strings.Contains(vars, `"page":1`) {
		t.Errorf("search variables = %q, want limit 26 page 1", vars)
	}
	if !strings.Contains(vars, `"countryOrigin":"ALL"`) {
		t.Errorf("search variables = %q, want countryOrigin ALL", vars)
	}
	if q := env.lastQuery.Load().(string); !strings.Contains(q, "shows(") {
		t.Errorf("search query = %q, want the shows query", q)
	}
}

func TestAllAnimeSearchSendsRefererAndOrigin(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	p := env.provider()

	if _, err := p.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// New-protocol transport: every API request carries the mkissa.to
	// Referer AND Origin (old allmanga.to values get stripped answers;
	// in the wired provider both come from the same referer constant).
	hdr, ok := env.lastAPIHdr.Load().(http.Header)
	if !ok || hdr == nil {
		t.Fatal("no API request recorded")
	}
	if got := hdr.Get("Referer"); got != env.referer.URL {
		t.Errorf("Referer = %q, want %q", got, env.referer.URL)
	}
	if got := hdr.Get("Origin"); got != env.referer.URL {
		t.Errorf("Origin = %q, want %q", got, env.referer.URL)
	}
}

func TestAllAnimeSearchTransportFailureSilent(t *testing.T) {
	t.Parallel()

	// Dead endpoint: Python's except returned [] (allanime.py:116-117).
	p := newAllAnime("http://"+deadAddr(t)+"/api", "http://"+deadAddr(t), "https://allanime.day", testClient(t, "allanime"))
	results, err := p.Search(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("Search transport error must be silent, got %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
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
		if episodes[i].Title != "Episode "+want {
			t.Errorf("episodes[%d].Title = %q", i, episodes[i].Title)
		}
		if episodes[i].RawID != "id1" {
			t.Errorf("episodes[%d].RawID = %q, want the show id", i, episodes[i].RawID)
		}
	}
	// 1 exists in both sub and dub; 2 only sub; 10.5 only dub.
	if _, ok := episodes[0].RawEmbeds["sub"]; !ok {
		t.Error("episode 1 missing sub embed")
	}
	if _, ok := episodes[0].RawEmbeds["dub"]; !ok {
		t.Error("episode 1 missing dub embed")
	}
	if _, ok := episodes[1].RawEmbeds["dub"]; ok {
		t.Error("episode 2 has phantom dub embed")
	}
	if _, ok := episodes[2].RawEmbeds["sub"]; ok {
		t.Error("episode 10.5 has phantom sub embed")
	}
}

// TestAllAnimeResolveStreamRoundTrip is the full protocol round trip:
// key derivation (referer page -> entry bundle -> mask chunk), aaReq
// token in the extensions, tobeparsed decryption, "--" URL decode,
// clock.json fetch and m3u8 variant resolution.
func TestAllAnimeResolveStreamRoundTrip(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "sub" {
		t.Errorf("DubName = %q", stream.DubName)
	}

	// aaReq + persisted query present on the episode call.
	ext := env.lastExt.Load().(string)
	var extParsed struct {
		PersistedQuery *struct {
			Version    int    `json:"version"`
			SHA256Hash string `json:"sha256Hash"`
		} `json:"persistedQuery"`
		AAReq string `json:"aaReq"`
	}
	if err := json.Unmarshal([]byte(ext), &extParsed); err != nil {
		t.Fatalf("extensions not JSON: %v (raw %q)", err, ext)
	}
	if extParsed.AAReq == "" {
		t.Fatal("extensions carry no aaReq token")
	}
	if extParsed.PersistedQuery == nil || extParsed.PersistedQuery.SHA256Hash != aaEpisodeQueryHash {
		t.Fatalf("persistedQuery = %+v, want hash %s", extParsed.PersistedQuery, aaEpisodeQueryHash)
	}

	// clock.json was fetched through the decoded internal URL.
	if env.clockHits.Load() < 2 { // clock.json + master.m3u8
		t.Fatalf("stream host hits = %d, want clock.json + playlist fetches", env.clockHits.Load())
	}

	// Variant playlist produced 1080 and 720 links with absolute URIs.
	l1080, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("links = %#v, want a 1080 entry", stream.Links)
	}
	if want := env.stream.URL + "/1080.m3u8"; l1080.URL != want {
		t.Errorf("1080 URL = %q, want %q", l1080.URL, want)
	}
	if l1080.Type != "m3u8" || l1080.Quality != "1080" {
		t.Errorf("1080 source = %+v", l1080)
	}
	wantReferer := env.stream.URL + "/all/manga/clock.json?w=1"
	if l1080.Headers["Referer"] != wantReferer {
		t.Errorf("1080 Referer = %q, want %q", l1080.Headers["Referer"], wantReferer)
	}
	// F30: the master playlist itself must have been FETCHED with the
	// clock URL as Referer (allanime.py:233-236), not the provider
	// headers — the fake CDN above 403s anything else.
	if got := env.lastMasterReferer.Load(); got == nil || got.(string) != wantReferer {
		t.Errorf("master.m3u8 fetch Referer = %v, want %q (F30)", got, wantReferer)
	}
	if env.masterForbidden.Load() != 0 {
		t.Errorf("master.m3u8 rejected %d fetches for a wrong Referer", env.masterForbidden.Load())
	}
	if _, ok := stream.Links["720"]; !ok {
		t.Error("no 720 link from the second variant")
	}
}

// TestAllAnimeResolveStreamDirectMedia pins the Python direct branch:
// a decoded URL containing .mp4/.m3u8 becomes a 1080 link without any
// clock fetch (allanime.py:211-213).
func TestAllAnimeResolveStreamDirectMedia(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Query().Get("query") != "" && strings.Contains(r.URL.Query().Get("query"), "episode(") {
			return false
		}
		if r.URL.Query().Get("extensions") == "" {
			return false
		}
		plain := fmt.Sprintf(`{"data":{"episode":{"episodeString":"1","sourceUrls":[`+
			`{"sourceUrl":"--%s","sourceName":"Yt-mp4"}]}}}`, aaHexEnc("/getwide/cool.mp4"))
		_, _ = fmt.Fprintf(w, `{"data":{"tobeparsed":%q}`,
			aaEncryptWith(t, env.apiKey(), plain))
		return true
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	l, ok := stream.Links["1080"]
	if !ok {
		t.Fatalf("links = %#v, want direct 1080", stream.Links)
	}
	if l.URL != env.stream.URL+"/getwide/cool.mp4" || l.Type != "" {
		t.Errorf("direct link = %+v", l)
	}
	if env.clockHits.Load() != 0 {
		t.Error("direct media must skip the clock flow")
	}
}

// TestAllAnimeResolveStreamPlainSourceUrls pins the tolerance for
// unencrypted responses: data.episode.sourceUrls directly.
func TestAllAnimeResolveStreamPlainSourceUrls(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Query().Get("extensions") == "" {
			return false
		}
		_, _ = w.Write([]byte(`{"data":{"episode":{"episodeString":"1","sourceUrls":[` +
			fmt.Sprintf(`{"sourceUrl":"--%s","sourceName":"Luf-Mp4"}]}}}`, aaHexEnc("/all/manga/clock?w=9"))))
		return true
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if _, ok := stream.Links["1080"]; !ok || env.clockHits.Load() == 0 {
		t.Fatalf("plain sourceUrls not expanded: links=%d clockHits=%d", len(stream.Links), env.clockHits.Load())
	}
}

// TestAllAnimeResolveStreamClockMP4 pins the mp4 branch of the clock
// links (allanime.py:260-261): non-hls .mp4 link becomes a 1080 mp4.
func TestAllAnimeResolveStreamClockMP4(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	plain := fmt.Sprintf(`{"data":{"episode":{"episodeString":"1","sourceUrls":[`+
		`{"sourceUrl":"--%s","sourceName":"S-mp4"}]}}}`, aaHexEnc("/all/manga/clock?w=2"))
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		if r.URL.Query().Get("extensions") == "" {
			return false
		}
		_, _ = fmt.Fprintf(w, `{"data":{"tobeparsed":%q}`, aaEncryptWith(t, env.apiKey(), plain))
		return true
	})
	env.stream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.clockHits.Add(1)
		if r.URL.Path == "/all/manga/clock.json" {
			_, _ = w.Write([]byte(`{"links":[{"link":"https://mirror.example/v/720.mp4"}]}`))
			return
		}
		http.NotFound(w, r)
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	l, ok := stream.Links["1080"]
	if !ok || l.URL != "https://mirror.example/v/720.mp4" || l.Type != "mp4" {
		t.Fatalf("mp4 link = %+v ok=%v", l, ok)
	}
}

// TestAllAnimeResolveStreamPersistedQueryFallback pins the APQ
// fallback: PersistedQueryNotFound -> retry once with the full query
// while keeping the aaReq extension.
func TestAllAnimeResolveStreamPersistedQueryFallback(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	var sawFullQuery atomic.Bool
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		ext := r.URL.Query().Get("extensions")
		if ext == "" || !strings.Contains(ext, "aaReq") {
			return false
		}
		if strings.Contains(ext, "persistedQuery") && !sawFullQuery.Load() {
			_, _ = w.Write([]byte(`{"errors":[{"message":"PersistedQueryNotFound"}]}`))
			return true
		}
		sawFullQuery.Store(true)
		return false
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if !sawFullQuery.Load() {
		t.Fatal("no fallback request with the full query")
	}
	if got := env.episodeAttempts.Load(); got != 2 {
		t.Fatalf("episode attempts = %d, want 2", got)
	}
	if q := env.lastQuery.Load(); q == nil || !strings.Contains(q.(string), "episode(") {
		t.Fatal("fallback request missing the full episode query")
	}
	// The fallback must still carry aaReq.
	ext := env.lastExt.Load().(string)
	if !strings.Contains(ext, "aaReq") {
		t.Fatalf("fallback extensions = %q, want aaReq kept", ext)
	}
	if _, ok := stream.Links["1080"]; !ok {
		t.Fatalf("fallback produced no links: %#v", stream.Links)
	}
}

// TestAllAnimeResolveStreamKeyRefreshOnCorruption pins the dispatch
// scenario: the API encrypts with a rotated key; the first decrypt
// fails, the provider force-refreshes the key material (the referer
// page now serves the new epoch) and succeeds.
func TestAllAnimeResolveStreamKeyRefreshOnCorruption(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	rotated := make([]byte, 32)
	for i := range rotated {
		rotated[i] = byte(99 + i)
	}
	var newPartB []byte
	for i := range rotated {
		newPartB = append(newPartB, env.maskB[i]^rotated[i])
	}
	// API encrypts with the rotated key from the start.
	env.mu.Lock()
	env.apiKey = func() []byte { return rotated }
	env.mu.Unlock()

	// Referer serves the old partB on the first derivation, the rotated
	// partB afterwards (epoch bumped — the rotation signal).
	var refererServes atomic.Int32
	env.referer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.refererHits.Add(1)
		n := refererServes.Add(1)
		part := env.partB
		epoch := 4130
		if n > 1 {
			part = newPartB
			epoch = 4131
		}
		_, _ = fmt.Fprintf(w,
			`<html><script>var __ssr={"epoch":%d,"partB":%q};</script>`+
				`<script defer src=%q></script></html>`,
			epoch, base64.StdEncoding.EncodeToString(part),
			env.cdn.URL+"/all/mk/_app/immutable/entry/app.x1.js")
	})
	p := env.provider()

	stream, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err != nil {
		t.Fatalf("ResolveStream after refresh: %v", err)
	}
	if _, ok := stream.Links["1080"]; !ok {
		t.Fatalf("links after refresh = %#v", stream.Links)
	}
	if got := env.refererHits.Load(); got < 2 {
		t.Fatalf("referer fetches = %d, want >= 2 (initial + forced refresh)", got)
	}
}

// TestAllAnimeResolveStreamDecryptFailureAfterRefresh pins the loud
// typed error when the blob still fails GCM after one refresh.
func TestAllAnimeResolveStreamDecryptFailureAfterRefresh(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	garbage := make([]byte, 32)
	for i := range garbage {
		garbage[i] = byte(i + 7)
	}
	env.mu.Lock()
	env.apiKey = func() []byte { return garbage }
	env.mu.Unlock()
	p := env.provider()

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if err == nil {
		t.Fatal("decrypt failure swallowed")
	}
	if !errors.Is(err, errAADecryptFailed) {
		t.Fatalf("err = %v, want errAADecryptFailed chain", err)
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed chain", err)
	}
	if got := env.refererHits.Load(); got < 2 {
		t.Fatalf("referer fetches = %d, want the forced refresh", got)
	}
}

// TestAllAnimeResolveStreamMaskNotFound pins the typed derivation
// failure when no chunk carries the mask.
func TestAllAnimeResolveStreamMaskNotFound(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.cdn.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chunks/one.js") {
			_, _ = w.Write([]byte(`export const nothing="here";`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chunks/two.js") {
			_, _ = w.Write([]byte(`export const also="nothing";`))
			return
		}
		_, _ = fmt.Fprint(w, `import{a}from"../chunks/one.js";import{b}from"../chunks/two.js";`)
	})
	p := env.provider()

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if !errors.Is(err, errAAMaskNotFound) {
		t.Fatalf("err = %v, want errAAMaskNotFound", err)
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want contracts.ErrExtractFailed chain", err)
	}
}

// TestAllAnimeResolveStreamPartBMissing pins the typed derivation
// failure when the referer page lacks partB.
func TestAllAnimeResolveStreamPartBMissing(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.referer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.refererHits.Add(1)
		_, _ = w.Write([]byte(`<html><script>var x=1;</script></html>`))
	})
	p := env.provider()

	_, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub")
	if !errors.Is(err, errAAPartBMissing) {
		t.Fatalf("err = %v, want errAAPartBMissing", err)
	}
}

// TestAllAnimeResolveStreamAACryptoRetry pins the server-side stale-key
// signal: an AA_CRYPTO_MISSING error triggers one key refresh and one
// request retry with a freshly signed token.
func TestAllAnimeResolveStreamAACryptoRetry(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	rotated := make([]byte, 32)
	for i := range rotated {
		rotated[i] = byte(120 + i)
	}
	newPartB := make([]byte, 32)
	for i := range rotated {
		newPartB[i] = env.maskB[i] ^ rotated[i]
	}
	key1 := make([]byte, 32)
	for i := range key1 {
		key1[i] = env.maskB[i] ^ env.partB[i]
	}
	// The server rotates its key the moment it rejects the stale token.
	var serverRotated atomic.Bool
	env.mu.Lock()
	env.apiKey = func() []byte {
		if serverRotated.Load() {
			return rotated
		}
		return key1
	}
	env.mu.Unlock()

	var refererServes atomic.Int32
	env.referer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		env.refererHits.Add(1)
		n := refererServes.Add(1)
		part := env.partB
		if n > 1 {
			part = newPartB
		}
		_, _ = fmt.Fprintf(w,
			`<html><script>var __ssr={"epoch":4130,"partB":%q};</script>`+
				`<script defer src=%q></script></html>`,
			base64.StdEncoding.EncodeToString(part),
			env.cdn.URL+"/all/mk/_app/immutable/entry/app.x1.js")
	})

	var cryptoRejections atomic.Int32
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		ext := r.URL.Query().Get("extensions")
		if ext == "" || !strings.Contains(ext, "aaReq") {
			return false
		}
		// Reject the first token as stale and rotate server-side.
		if cryptoRejections.Add(1) == 1 {
			serverRotated.Store(true)
			_, _ = w.Write([]byte(`{"errors":[{"message":"AA_CRYPTO_MISSING","extensions":{"code":"AA_CRYPTO_MISSING"}}]}`))
			return true
		}
		return false
	})
	p := env.provider()

	if _, err := p.ResolveStream(context.Background(), contracts.Episode{Num: "1", RawID: "id1"}, "sub"); err != nil {
		t.Fatalf("ResolveStream after AA_CRYPTO retry: %v", err)
	}
	if got := env.episodeAttempts.Load(); got != 2 {
		t.Fatalf("episode attempts = %d, want 2", got)
	}
	if got := env.refererHits.Load(); got < 2 {
		t.Fatalf("referer fetches = %d, want refresh", got)
	}
}

// TestAllAnimeResolveStreamEmptyEpisode pins the null-episode outcome:
// no sources, empty MediaStream, no error (Python sourceUrls [] path).
func TestAllAnimeResolveStreamEmptyEpisode(t *testing.T) {
	t.Parallel()

	env := newAAEnv(t)
	env.apiBehaviors = append(env.apiBehaviors, func(r *http.Request, w http.ResponseWriter) bool {
		ext := r.URL.Query().Get("extensions")
		if ext == "" || !strings.Contains(ext, "aaReq") {
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

// deadAddr returns an address that refuses connections instantly.
func deadAddr(t *testing.T) string {
	t.Helper()
	l := newDeadListener(t)
	return l.Addr().String()
}
