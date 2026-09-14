package regression

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/api"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"
)

// update regenerates the golden files (make goldens-update).
var update = flag.Bool("update", false, "rewrite golden capture files")

// goldenLogin/goldenPassword reuse the frozen python pbkdf2 vector so
// the golden suite verifies the real hash format (240k iterations cost
// ~a few login calls per run — acceptable).
const (
	goldenLogin    = "alice"
	goldenPassword = "correct horse battery staple"
	goldenHash     = "pbkdf2_sha256$240000$uE0qBHS22YmdwZyQyeio4w$L17pdSzHKbUXDkb-itZUhRVltIcfhCRrSQJHLmCKLbM"
)

// volatileKeys are response fields whose values are derived from
// wall-clock time or per-run randomness (tokens, session ids,
// now()-stamped rows and their artwork cache seeds). They are
// normalized to a placeholder before the byte-compare; every other
// byte of every response is pinned.
var volatileKeys = map[string]bool{
	"access_token":    true,
	"refresh_token":   true,
	"session_id":      true,
	"updated_at":      true,
	"etag_seed":       true,
	"artwork_version": true,
	// ttl_seconds = fixed stream expiry minus wall clock: presence is
	// pinned, the countdown value is not.
	"ttl_seconds": true,
}

// normalize walks a decoded JSON value replacing volatile fields with a
// placeholder (maps only; arrays recursed).
func normalize(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if volatileKeys[k] {
				t[k] = "<volatile>"
				continue
			}
			normalize(val)
		}
	case []any:
		for _, val := range t {
			normalize(val)
		}
	}
}

// newGoldenApp wires the App under capture: in-memory store seeded with
// fixed rows, the fixed provider registry, the fixed shikimori client.
func newGoldenApp(t *testing.T) (*api.App, http.Handler) {
	t.Helper()

	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	seed := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	rows := []*storage.AnimeProgress{
		{
			Title: "Ковбой Бибоп", Poster: strPtr("https://fixed.example/poster/1.jpg"),
			SourceID: fixedProviderID, SourceURL: "https://fixed.example/anime/1",
			CurrentEpisode: "12", VideoDub: strPtr("1080"), AudioDub: strPtr("dub-1"),
			ShikimoriTitle: strPtr("Cowboy Bebop"), ShikimoriID: int64Ptr(21),
			ShikimoriStatus: "watching", Score: 9, TotalEpisodes: 26,
			ProgressSeconds: 1200, TotalSeconds: 1440, UpdatedAt: seed,
		},
		{
			Title: "Второе аниме", SourceID: fixedProviderID, SourceURL: "https://fixed.example/anime/2",
			CurrentEpisode: "3", ShikimoriStatus: "planned", Score: 0, TotalEpisodes: 12,
			UpdatedAt: seed.Add(-24 * time.Hour),
		},
	}
	for _, row := range rows {
		if err := store.Progress.Upsert(context.Background(), row); err != nil {
			t.Fatalf("seed progress: %v", err)
		}
	}
	if err := store.Sources.ReplaceForAnime(context.Background(), rows[0].ID, []storage.AnimeSource{
		{SourceID: fixedProviderID, SourceURL: "https://fixed.example/anime/1"},
		{SourceID: "shikimori", SourceURL: "https://shikimori.io/animes/21"},
	}); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	if err := store.Episodes.Upsert(context.Background(), &storage.EpisodeProgress{
		AnimeID: rows[0].ID, Episode: "1", PositionSec: 600, DurationSec: 1440,
		VideoKey: strPtr("1080"), UpdatedAt: seed,
	}); err != nil {
		t.Fatalf("seed episode progress: %v", err)
	}

	reg := providers.NewEmptyRegistry()
	if err := reg.Register(fixedProvider{}); err != nil {
		t.Fatalf("register fixed provider: %v", err)
	}

	cfg := config.Default()
	cfg.API.Enabled = true
	cfg.API.TokenTTL = 15 * time.Minute
	cfg.API.RefreshTokenTTL = 24 * time.Hour
	cfg.API.AuthSecret = "golden-secret"
	cfg.Web.Users = map[string]config.WebUser{goldenLogin: {PasswordHash: goldenHash}}

	shikiNet, err := netclient.New(cfg.Network, netclient.WithProvider("shikimori"))
	if err != nil {
		t.Fatalf("netclient for shikimori seam: %v", err)
	}

	app, err := api.NewApp(api.Config{
		Settings: cfg,
		Store:    store,
		Registry: reg,
		Shiki:    fixedShiki{},
		ShikiNet: shikiNet,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(app.Close)
	return app, app.Router()
}

// goldenRequest is one captured call: a name for the golden file, the
// HTTP request to issue and whether it needs the bearer token minted by
// the login step (executed first in the fixed sequence below).
type goldenRequest struct {
	name    string
	request *http.Request
}

func jsonReq(method, target, body string, bearer string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, target, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

// TestAPIContractGoldens drives the full 20-endpoint surface against
// fixed fakes and byte-compares every normalized response against
// testdata/golden. Any contract change must consciously regenerate the
// goldens via `make goldens-update` (go test ./internal/regression -update).
func TestAPIContractGoldens(t *testing.T) {
	_, router := newGoldenApp(t)

	// 1. login first: the minted token feeds the authed requests.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, jsonReq(http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"login": %q, "password": %q}`, goldenLogin, goldenPassword), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("login prerequisite failed: %d %s", rec.Code, rec.Body.String())
	}
	var loginBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &loginBody); err != nil {
		t.Fatalf("login prerequisite body: %v", err)
	}
	token, _ := loginBody["access_token"].(string)
	refresh, _ := loginBody["refresh_token"].(string)
	if token == "" || refresh == "" {
		t.Fatal("login prerequisite missing tokens")
	}

	requests := []goldenRequest{
		{"01-health", jsonReq(http.MethodGet, "/api/v1/health", "", "")},
		{"02-auth-login", jsonReq(http.MethodPost, "/api/v1/auth/login",
			fmt.Sprintf(`{"login": %q, "password": %q}`, goldenLogin, goldenPassword), "")},
		{"03-auth-me", jsonReq(http.MethodGet, "/api/v1/auth/me", "", token)},
		{"04-providers", jsonReq(http.MethodGet, "/api/v1/providers", "", token)},
		{"05-history-list", jsonReq(http.MethodGet, "/api/v1/history", "", token)},
		{"06-history-one", jsonReq(http.MethodGet, "/api/v1/history/1", "", token)},
		{"07-history-patch", jsonReq(http.MethodPatch, "/api/v1/history/1",
			`{"status": "completed", "score": 10}`, token)},
		{"08-history-episodes", jsonReq(http.MethodGet, "/api/v1/history/1/episodes", "", token)},
		{"09-history-progress-patch", jsonReq(http.MethodPatch, "/api/v1/history/1/progress",
			`{"episode": "2", "position_sec": 120, "duration_sec": 1440, "video_key": "1080", "quality": 1080}`, token)},
		{"10-history-progress-get", jsonReq(http.MethodGet, "/api/v1/history/1/progress?episode=2", "", token)},
		{"11-library-list", jsonReq(http.MethodGet, "/api/v1/library", "", token)},
		{"12-library-bind", jsonReq(http.MethodPost, "/api/v1/library/bind",
			`{"title": "Третье аниме", "artwork": {"poster": "https://fixed.example/poster/3.jpg"}, "sources": [{"source_id": "fixed", "source_url": "https://fixed.example/anime/3"}], "shikimori_id": 25, "shikimori_title": "Third Fixed Anime", "shikimori_status": "watching"}`, token)},
		{"13-search", jsonReq(http.MethodGet, "/api/v1/search?q=cowboy", "", token)},
		{"14-releases-calendar", jsonReq(http.MethodGet, "/api/v1/releases/calendar", "", token)},
		{"15-home-feed", jsonReq(http.MethodGet, "/api/v1/home/feed", "", token)},
		{"16-episodes", jsonReq(http.MethodGet,
			"/api/v1/episodes?source_id=fixed&source_url=https%3A%2F%2Ffixed.example%2Fanime%2F1", "", token)},
		{"17-streams-resolve", jsonReq(http.MethodPost, "/api/v1/streams/resolve",
			`{"source_id": "fixed", "episode_num": "1", "episode_raw_id": "ep-1", "video_key": "1080", "urls_video": ["https://fixed.example/embed/1/1080"]}`, token)},
		{"18-shikimori-anime-page", jsonReq(http.MethodGet, "/api/v1/shikimori/anime/21/page", "", token)},
		{"19-auth-refresh", jsonReq(http.MethodPost, "/api/v1/auth/refresh",
			fmt.Sprintf(`{"refresh_token": %q}`, refresh), "")},
		// 20. logout LAST: it revokes the session every earlier request
		// relied on.
		{"20-auth-logout", jsonReq(http.MethodPost, "/api/v1/auth/logout", "", token)},
	}

	if len(requests) != 20 {
		t.Fatalf("golden suite must enumerate exactly 20 endpoints, has %d", len(requests))
	}

	for _, gr := range requests {
		t.Run(gr.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, gr.request)

			var body any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON (%d): %q", rec.Code, rec.Body.String())
			}
			normalize(body)

			wrapper, err := json.Marshal(map[string]any{"status": rec.Code, "body": body})
			if err != nil {
				t.Fatalf("marshal normalized capture: %v", err)
			}
			wrapper = append(wrapper, '\n')

			goldenPath := filepath.Join("testdata", "golden", gr.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
					t.Fatalf("create golden dir: %v", err)
				}
				if writeErr := os.WriteFile(goldenPath, wrapper, 0o600); writeErr != nil { //nolint:gosec // golden fixture
					t.Fatalf("write golden: %v", writeErr)
				}
				return
			}

			want, err := os.ReadFile(goldenPath) //nolint:gosec // golden fixture path
			if err != nil {
				t.Fatalf("golden missing (%v) — run `make goldens-update` and review the diff", err)
			}
			if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(wrapper)) {
				t.Fatalf("API contract drift for %s:\n--- golden ---\n%s\n--- now ---\n%s",
					gr.name, want, wrapper)
			}
		})
	}
}
