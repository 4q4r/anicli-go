//go:build load

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Load profile: 200 concurrent virtual users each run the full read
// journey (login → me → home/feed → episodes → streams/resolve →
// history ×6 over the 1000-row seeded table → library → search →
// providers → calendar → shikimori page → health → chained refresh)
// 10 times against a REAL chi server on a random loopback port. The
// provider and the Shikimori client are in-process fakes, so the
// measured latencies are honest for the server layer (routing, auth,
// cache, sqlite, serialization) without site I/O.
//
// SLO split (PR81 review): this is the STRESS profile — at 200 VUs the
// single-connection sqlite pool makes queue wait, not endpoint work,
// dominate p99 (healthy measured p99 ≈ 1-9 s depending on endpoint).
// The assertions here are completion + zero errors + the stress p99
// envelope; the 250 ms latency net lives in TestLoadAPIRamp (light
// journey) and the /history endpoint cost is pinned standalone by the
// storage HistoryListBatched benchmark (12 ms @1000 rows).

const (
	loadVUs       = 200
	loadIteration = 10

	// loadStressP99 bounds every endpoint's p99 under the 200-VU heavy
	// journey. At 200 VUs on the single-connection sqlite pool the p99
	// is QUEUE WAIT, not endpoint work: each iteration fires a ×6
	// /history burst over the 1000-row table, and the endpoints riding
	// behind that burst (library, home/feed) inherit its backlog
	// (measured healthy p99: library 11.06 s, feed 7.20 s across
	// rounds; standalone endpoint costs are milliseconds — pinned by
	// the storage HistoryListBatched benchmark). The 30 s envelope is
	// ~3× the worst observed healthy backlog and still a structural
	// tripwire: the pre-fix /history N+1 could not complete this
	// profile at all (5-min timeout), and a new per-row query pattern
	// amplifies through the same queue into the minutes.
	loadStressP99 = 30 * time.Second
	// loadSLOErrorRate bounds the fraction of failed requests.
	loadSLOErrorRate = 0.001 // 0.1%

	// loadLogin/loadPassword are the credentials of the single load
	// user; the hash below is generated with 1000 pbkdf2 iterations
	// (see loadUserHash) so the KDF does not dominate the measurement —
	// the auth CODE path is identical, only the work factor differs
	// from production-configured hashes.
	loadLogin    = "loadvu"
	loadPassword = "load vu password"
)

// loadUserHash derives the low-iteration pbkdf2 vector via the same
// production helpers VerifyPasswordHash parses.
func loadUserHash(t *testing.T) string {
	t.Helper()
	salt := []byte("anicli-load-salt")
	digest := pbkdf2SHA256([]byte(loadPassword), salt, 1000, 32)
	return fmt.Sprintf("pbkdf2_sha256$1000$%s$%s", base64URLEncode(salt), base64URLEncode(digest))
}

// loadProvider is the in-process fake backend: instant, deterministic
// answers on every operation.
type loadProvider struct{}

func (loadProvider) ID() string                       { return "load" }
func (loadProvider) Name() string                     { return "load" }
func (loadProvider) BaseURL() string                  { return "https://load.example" }
func (loadProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (loadProvider) Search(_ context.Context, query string) ([]contracts.SearchResult, error) {
	out := make([]contracts.SearchResult, 0, 5)
	for i := range 5 {
		out = append(out, contracts.SearchResult{
			Title:    fmt.Sprintf("load hit %d for %s", i, query),
			URL:      fmt.Sprintf("https://load.example/anime/%d", i),
			SourceID: "load",
		})
	}
	return out, nil
}

func (loadProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	out := make([]contracts.Episode, 0, 12)
	for i := range 12 {
		num := fmt.Sprint(i + 1)
		out = append(out, contracts.Episode{
			Num:   num,
			Title: "Episode " + num,
			RawID: "ep-" + num,
			RawEmbeds: map[string][]string{
				"1080": {"https://load.example/embed/" + num + "/1080"},
				"720":  {"https://load.example/embed/" + num + "/720"},
			},
		})
	}
	return out, nil
}

func (loadProvider) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return contracts.MediaStream{
		DubName: "load dub " + dubID,
		Links: map[string]contracts.VideoSource{
			"1080": {URL: "https://load.example/media/1080.m3u8", Quality: "1080", Type: "m3u8"},
			"720":  {URL: "https://load.example/media/720.m3u8", Quality: "720", Type: "m3u8"},
		},
	}, nil
}

// newLoadApp builds the app under load: memory store seeded with 1000
// history rows (the real ListHistory scan the /history endpoint pays
// for), one fake provider, an instant fake Shikimori client (search
// enrichment, feed, library, calendar, anime page), low-iteration
// credentials, discard logger.
func newLoadApp(t *testing.T) *App {
	t.Helper()

	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed the 1000-row history: /history is the unbounded ListHistory
	// scan under audit (P1#3) — the load must ride a realistic roster.
	// One anime_source row per record, so the batched source query
	// (ListAll) scans real data instead of an empty table.
	seedCtx := context.Background()
	for i := range loadHistoryRows {
		rec := loadSeedRow(i)
		if err := store.Progress.Upsert(seedCtx, rec); err != nil {
			t.Fatalf("seed history row %d: %v", i, err)
		}
		row, err := store.Progress.GetBySource(seedCtx, rec.SourceID, rec.SourceURL)
		if err != nil {
			t.Fatalf("seed source lookup %d: %v", i, err)
		}
		if err := store.Sources.ReplaceForAnime(seedCtx, row.ID, []storage.AnimeSource{
			{SourceID: "load", SourceURL: rec.SourceURL},
		}); err != nil {
			t.Fatalf("seed anime_source %d: %v", i, err)
		}
	}

	reg := providers.NewEmptyRegistry()
	if err := reg.Register(loadProvider{}); err != nil {
		t.Fatalf("register load provider: %v", err)
	}

	cfg := config.Default()
	cfg.API.Enabled = true
	cfg.API.TokenTTL = 15 * time.Minute
	cfg.API.RefreshTokenTTL = 24 * time.Hour
	cfg.API.AuthSecret = "load-secret"
	cfg.Web.Users = map[string]config.WebUser{
		loadLogin: {PasswordHash: loadUserHash(t)},
	}

	app, err := NewApp(Config{Settings: cfg, Store: store, Registry: reg, Logger: logDiscard(), Shiki: loadShikiFake(), ShikiNet: loadShikiNet()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return app
}

// loadHistoryRows is the seeded history roster size (review #3: the
// /history p95 must be measured over a realistic 1000-row table).
const loadHistoryRows = 1000

// loadSeedRow builds one deterministic history row.
func loadSeedRow(i int) *storage.AnimeProgress {
	poster := fmt.Sprintf("https://load.example/poster/%d.jpg", i)
	return &storage.AnimeProgress{
		Title:          fmt.Sprintf("Load History Anime %d", i),
		Poster:         &poster,
		SourceID:       "load",
		SourceURL:      fmt.Sprintf("https://load.example/anime/%d", i),
		CurrentEpisode: fmt.Sprint(i%24 + 1),
		TotalEpisodes:  24,
		Score:          i % 11,
		UpdatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).UTC(),
	}
}

// loadShikiFake builds the in-process Shikimori client fake wired into
// the load app: 25 bound rates over 25 anime (feed, library, calendar
// and search-enrichment all do real work).
func loadShikiFake() *fakeShiki {
	rates := make([]shikimori.UserRate, 0, 25)
	animes := make([]shikimori.Anime, 0, 25)
	for i := range 25 {
		rates = append(rates, shikimori.UserRate{
			ID: int64(9000 + i), UserID: 50, TargetID: int64(500 + i),
			TargetType: "Anime", Status: [4]string{"watching", "completed", "planned", "rewatching"}[i%4],
			Episodes: i % 12, Score: i % 10,
		})
		animes = append(animes, shikimori.Anime{
			ID: int64(500 + i), Name: fmt.Sprintf("Load Shiki Anime %d", i),
			Russian: fmt.Sprintf("Лоад аниме %d", i), Episodes: 24, Status: "ongoing", Kind: "TV",
		})
	}
	ongoing := make([]shikimori.OngoingCandidate, 0, 5)
	for i := range 5 {
		ongoing = append(ongoing, shikimori.OngoingCandidate{ShikimoriID: int64(500 + i), TitleRu: loadPtr("Лоад онгоинг")})
	}
	return &fakeShiki{rates: rates, animes: animes, ongoing: ongoing, authed: true}
}

// loadPtr returns a pointer to the string (the ongoing candidates'
// nullable titles).
func loadPtr(s string) *string { return &s }

// loadShikiNet builds the request-scoped shikimori netclient the app
// requires alongside any shikimori client (never used by the fake —
// the fake answers in-process).
func loadShikiNet() *netclient.Client {
	net, err := netclient.New(config.Network{ProxyURL: ""})
	if err != nil {
		panic("load shiki netclient: " + err.Error())
	}
	return net
}

// loadStats accumulates per-endpoint latencies and failures.
type loadStats struct {
	mu       sync.Mutex
	latency  map[string][]time.Duration
	failures map[string]int
}

func newLoadStats() *loadStats {
	return &loadStats{
		latency:  map[string][]time.Duration{},
		failures: map[string]int{},
	}
}

func (s *loadStats) record(endpoint string, d time.Duration, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency[endpoint] = append(s.latency[endpoint], d)
	if failed {
		s.failures[endpoint]++
	}
}

func (s *loadStats) percentile(endpoint string, p float64) time.Duration {
	values := s.latency[endpoint]
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	idx := int(float64(len(values)-1) * p)
	return values[idx]
}

func (s *loadStats) total() (calls int, failures int) {
	for _, values := range s.latency {
		calls += len(values)
	}
	for _, n := range s.failures {
		failures += n
	}
	return calls, failures
}

// loadJourney runs one virtual-user iteration over the SAFE route set:
// login → me → home/feed → episodes → streams/resolve → history list
// ×6 (the unbounded ListHistory scan, P1#3 evidence) → history one →
// history episodes → history progress → library → search → providers →
// calendar → shikimori page → health → refresh (chained: each response
// rotates the per-VU refresh token).
//
// MUTATING ROUTES DELIBERATELY EXCLUDED (documented exclusions, PR81
// fix-round review): POST /auth/logout (revokes the per-VU session the
// chained refresh depends on), PATCH /history/{id},
// PATCH /history/{id}/progress (mutate seeded rows mid-run), POST
// /library/bind (creates rows). The journey is read-only by design.
func loadJourney(client *http.Client, baseURL string, vu, iter int, stats *loadStats, refreshToken *string) {
	call := func(endpoint, method, path, body string, auth string) {
		start := time.Now()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+path, rd)
		if err != nil {
			stats.record(endpoint, 0, true)
			return
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := client.Do(req)
		failed := err != nil
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			failed = resp.StatusCode != http.StatusOK
		}
		stats.record(endpoint, time.Since(start), failed)
	}

	// login (fresh session each iteration: exercises the full auth path)
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/auth/login",
		strings.NewReader(fmt.Sprintf(`{"login": %q, "password": %q}`, loadLogin, loadPassword)))
	if err != nil {
		stats.record("auth/login", 0, true)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		stats.record("auth/login", time.Since(start), true)
		return
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	closeErr := resp.Body.Close()
	stats.record("auth/login", time.Since(start), decodeErr != nil || resp.StatusCode != http.StatusOK)
	if decodeErr != nil || resp.StatusCode != http.StatusOK || closeErr != nil {
		return
	}
	token := payload.AccessToken
	*refreshToken = payload.RefreshToken

	call("auth/me", http.MethodGet, "/api/v1/auth/me", "", token)
	call("home/feed", http.MethodGet, "/api/v1/home/feed", "", token)
	call("episodes", http.MethodGet,
		fmt.Sprintf("/api/v1/episodes?source_id=load&source_url=https%%3A%%2F%%2Fload.example%%2Fanime%%2F%d-%d", vu, iter),
		"", token)
	call("streams/resolve", http.MethodPost, "/api/v1/streams/resolve",
		`{"source_id": "load", "episode_num": "1", "episode_raw_id": "ep-1", "video_key": "1080", "urls_video": ["https://load.example/embed/1/1080"]}`,
		token)

	// /history ×6: the unbounded ListHistory scan (P1#3) — measured
	// over the 1000-row seeded table every iteration. The per-record
	// GETs rotate over the seeded ids (AUTOINCREMENT 1..1000).
	historyID := 1 + (vu+iter)%loadHistoryRows
	for range 6 {
		call("history", http.MethodGet, "/api/v1/history", "", token)
	}
	call("history/one", http.MethodGet, fmt.Sprintf("/api/v1/history/%d", historyID), "", token)
	call("history/episodes", http.MethodGet, fmt.Sprintf("/api/v1/history/%d/episodes", historyID), "", token)
	call("history/progress", http.MethodGet, fmt.Sprintf("/api/v1/history/%d/progress", historyID), "", token)
	call("library", http.MethodGet, "/api/v1/library", "", token)
	call("search", http.MethodGet, fmt.Sprintf("/api/v1/search?q=load%%20anime%%20%d", vu), "", token)
	call("providers", http.MethodGet, "/api/v1/providers", "", token)
	call("calendar", http.MethodGet, "/api/v1/releases/calendar?days=7", "", token)
	call("shikimori/page", http.MethodGet, fmt.Sprintf("/api/v1/shikimori/anime/%d/page", 500+vu%25), "", token)
	call("health", http.MethodGet, "/api/v1/health", "", "")

	// refresh: chain the rotated token (each refresh invalidates the
	// previous one — using a stale token would fail the zero-error SLO).
	start = time.Now()
	req, err = http.NewRequest(http.MethodPost, baseURL+"/api/v1/auth/refresh",
		strings.NewReader(fmt.Sprintf(`{"refresh_token": %q}`, *refreshToken)))
	if err != nil {
		stats.record("auth/refresh", 0, true)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		stats.record("auth/refresh", time.Since(start), true)
		return
	}
	var refreshed struct {
		RefreshToken string `json:"refresh_token"`
	}
	decodeErr = json.NewDecoder(resp.Body).Decode(&refreshed)
	closeErr = resp.Body.Close()
	failed := decodeErr != nil || resp.StatusCode != http.StatusOK || closeErr != nil
	if !failed && refreshed.RefreshToken != "" {
		*refreshToken = refreshed.RefreshToken
	}
	stats.record("auth/refresh", time.Since(start), failed)
}

// TestLoadAPIServer hammers the real server with 200 VUs and asserts
// the latency/error SLOs, printing the results table either way.
func TestLoadAPIServer(t *testing.T) {
	app := newLoadApp(t)
	t.Cleanup(app.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	baseURL := "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, ln) }()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        loadVUs * 2,
			MaxIdleConnsPerHost: loadVUs * 2,
			MaxConnsPerHost:     0,
		},
	}

	stats := newLoadStats()
	start := time.Now()

	var wg sync.WaitGroup
	for vu := range loadVUs {
		wg.Add(1)
		go func(vu int) {
			defer wg.Done()
			refresh := "" // per-VU rotated refresh chain
			for iter := range loadIteration {
				loadJourney(client, baseURL, vu, iter, stats, &refresh)
			}
		}(vu)
	}
	wg.Wait()
	wall := time.Since(start)

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("server exit: %v", err)
	}

	calls, failures := stats.total()
	errorRate := float64(failures) / float64(calls)

	// Results table (always printed — make load output).
	fmt.Fprintf(os.Stdout, "\n=== API load results (%d VUs × %d iterations) ===\n", loadVUs, loadIteration)
	fmt.Fprintln(os.Stdout, "endpoint\tp50\tp95\tp99\terrors")
	endpoints := make([]string, 0, len(stats.latency))
	for endpoint := range stats.latency {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)

	var sloBroken bool
	for _, endpoint := range endpoints {
		p50 := stats.percentile(endpoint, 0.50)
		p95 := stats.percentile(endpoint, 0.95)
		p99 := stats.percentile(endpoint, 0.99)
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%d\n",
			endpoint, p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond),
			stats.failures[endpoint])
		if p99 >= loadStressP99 {
			sloBroken = true
			t.Errorf("stress p99 envelope violated: %s p99 %s >= %s", endpoint, p99, loadStressP99)
		}
	}
	rps := float64(calls) / wall.Seconds()
	fmt.Fprintf(os.Stdout, "total\t%d calls\t%.0f rps\terror rate %.4f%%\twall %s\n\n",
		calls, rps, errorRate*100, wall.Round(time.Millisecond))

	if errorRate >= loadSLOErrorRate {
		sloBroken = true
		t.Errorf("SLO violated: error rate %.4f%% >= %.4f%%", errorRate*100, loadSLOErrorRate*100)
	}
	if sloBroken {
		t.Fatal("load SLOs violated (see table above)")
	}
}
