package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// shikiAPI is the shikimori surface the API face consumes; *shikimori.Client
// satisfies it, tests inject fakes.
type shikiAPI interface {
	Autocomplete(ctx context.Context, query string, limit int) ([]shikimori.AutocompleteItem, error)
	GetUserRates(ctx context.Context) ([]shikimori.UserRate, error)
	GetAnimesInfo(ctx context.Context, ids []int64) ([]shikimori.Anime, error)
	FetchOngoingCandidates(ctx context.Context) ([]shikimori.OngoingCandidate, error)
	GetAnimeDetails(ctx context.Context, shikimoriID int64) (*shikimori.AnimeDetails, error)
	Authenticated() bool
}

// Config wires the API face to the shared core.
type Config struct {
	Settings config.Settings
	Store    *storage.Store
	Registry *providers.Registry
	// Shiki is the app-default Shikimori client; nil disables tracker
	// features (they degrade to empty payloads, python parity).
	Shiki shikiAPI
	// ShikiNet is the netclient used to build request-scoped clients
	// from per-user credentials; nil means Shiki's transport is shared
	// when present, otherwise scoped clients cannot be built.
	ShikiNet *netclient.Client
	Logger   *slog.Logger
}

// App is the API application: settings, the shared core handles and the
// in-process TTL cache. Build with NewApp; serve with Router or Run.
type App struct {
	cfg      config.Settings
	store    *storage.Store
	registry *providers.Registry
	shiki    shikiAPI
	shikiNet *netclient.Client
	log      *slog.Logger
	cache    *TTLCache
	secret   []byte
	now      func() time.Time

	cacheCtx    context.Context
	cacheCancel context.CancelFunc
}

// cacheSweepInterval is the TTL-cache janitor cadence.
const cacheSweepInterval = 30 * time.Second

// NewApp validates the wiring (fail loud: an API without a signing
// secret must never start) and starts the cache janitor.
func NewApp(cfg Config) (*App, error) {
	if cfg.Settings.API.AuthSecret == "" {
		return nil, errors.New("api: auth secret is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("api: storage store is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("api: provider registry is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ShikiNet == nil && cfg.Shiki != nil {
		return nil, errors.New("api: shiki net client is required when a shikimori client is set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg:         cfg.Settings,
		store:       cfg.Store,
		registry:    cfg.Registry,
		shiki:       cfg.Shiki,
		shikiNet:    cfg.ShikiNet,
		log:         logger,
		cache:       NewTTLCache(ctx, cacheSweepInterval),
		secret:      []byte(cfg.Settings.API.AuthSecret),
		now:         time.Now,
		cacheCtx:    ctx,
		cacheCancel: cancel,
	}, nil
}

// Close releases background resources (the cache janitor).
func (a *App) Close() {
	a.cacheCancel()
}

// Router builds the chi router with the middleware chain and every
// ported route of the api_server.py contract.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(a.withTraceID)
	r.Use(a.withLogging)
	r.Use(a.withRecover)
	r.Use(a.withBearerGuard)

	r.Get("/api/v1/health", a.handleHealth)

	r.Post("/api/v1/auth/login", a.handleAuthLogin)
	r.Post("/api/v1/auth/refresh", a.handleAuthRefresh)
	r.Post("/api/v1/auth/logout", a.handleAuthLogout)
	r.Get("/api/v1/auth/me", a.handleAuthMe)

	r.Get("/api/v1/providers", a.handleProviders)

	r.Get("/api/v1/history", a.handleHistoryList)
	r.Get("/api/v1/history/{anime_id}", a.handleHistoryOne)
	r.Patch("/api/v1/history/{anime_id}", a.handleHistoryPatch)
	r.Get("/api/v1/history/{anime_id}/episodes", a.handleHistoryEpisodes)
	r.Patch("/api/v1/history/{anime_id}/progress", a.handleHistoryProgressPatch)
	r.Get("/api/v1/history/{anime_id}/progress", a.handleHistoryProgressGet)

	r.Get("/api/v1/library", a.handleLibraryList)
	r.Post("/api/v1/library/bind", a.handleLibraryBind)

	r.Get("/api/v1/search", a.handleSearch)
	r.Get("/api/v1/releases/calendar", a.handleReleasesCalendar)
	r.Get("/api/v1/home/feed", a.handleHomeFeed)

	r.Get("/api/v1/episodes", a.handleEpisodes)
	r.Post("/api/v1/streams/resolve", a.handleStreamsResolve)

	r.Get("/api/v1/shikimori/anime/{anime_id}/page", a.handleShikimoriAnimePage)

	r.NotFound(a.handleNotFound)
	r.MethodNotAllowed(a.handleMethodNotAllowed)

	return r
}

// handleNotFound renders 404s in the unified contract (python
// _http_exception_handler).
func (a *App) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, errNotFound("Not Found"))
}

// handleMethodNotAllowed renders 405s; python's FastAPI returns 405
// with a plain detail — normalized here into the unified shape.
func (a *App) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	e := &apiError{Code: "internal_error", Message: "Method Not Allowed"}
	writeJSON(w, http.StatusMethodNotAllowed, errorPayload{
		Error: errorBody{Code: e.Code, Message: e.Message, TraceID: traceIDOf(r)},
	})
}

// handleHealth is the public liveness probe (python /api/v1/health).
func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":               true,
		"database_backend": "sqlite",
		"provider_count":   len(a.registry.List()),
	})
}
