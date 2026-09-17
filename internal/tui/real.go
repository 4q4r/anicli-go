package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/metadata"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/player"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/skip"
	"github.com/an0nx/anicli-go/internal/storage"
)

// RealDeps bundles the production services with their teardown hooks.
type RealDeps struct {
	// Deps is the service set the screens consume.
	Deps *Deps
	// Store is the opened database (closed on Close).
	Store *storage.Store
	// Downloads is the background download manager (closed on Close).
	Downloads *download.Manager
	// ShikiNet is the netclient shared by the skip manager and the
	// shikimori client.
	ShikiNet *netclient.Client
	// registry is the provider set (closed on Close: releases the CF
	// bypass stack when [cf] is enabled, and tears down the shared
	// torrent engine when it was ever started).
	registry *providers.Registry
}

// RealOption customizes the production wiring of NewRealDeps.
type RealOption func(*realOptions)

// realOptions carries the NewRealDeps customizations.
type realOptions struct {
	// shikiPersister reports refreshed Shikimori OAuth sections to the
	// settings file (PR25 E); nil keeps the client in-memory only.
	shikiPersister func(config.Shikimori) error
	// logger is the diagnostics sink for the torrent engine (PR35);
	// nil degrades to a discard logger — never stderr inside the TUI.
	logger *slog.Logger
}

// WithLogger installs the diagnostics sink the torrent engine logs to
// (the CLI passes the TUI file logger — stderr corrupts alt-screen).
func WithLogger(log *slog.Logger) RealOption {
	return func(o *realOptions) { o.logger = log }
}

// WithShikiPersister installs the Shikimori token persistence hook:
// successful OAuth refreshes survive process restarts.
func WithShikiPersister(p func(config.Shikimori) error) RealOption {
	return func(o *realOptions) { o.shikiPersister = p }
}

// logf normalizes the optional diagnostics sink: nil degrades to a
// discard logger (never stderr — it corrupts alt-screen).
func logf(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return log
}

// NewRealDeps wires the production core: registry, storage, player,
// skip manager, downloader, offline index and the shikimori client.
func NewRealDeps(settings config.Settings, store *storage.Store, opts ...RealOption) (*RealDeps, error) {
	var o realOptions
	for _, opt := range opts {
		opt(&o)
	}

	registry, err := providers.NewRegistry(settings, store.ProviderStats,
		providers.WithTorrentLogger(logf(o.logger)))
	if err != nil {
		return nil, fmt.Errorf("build provider registry: %w", err)
	}

	shikiNet, err := netclient.New(settings.Network, netclient.WithProvider("shikimori"))
	if err != nil {
		return nil, fmt.Errorf("build shikimori transport: %w", err)
	}

	// The hybrid search's alias source (PR24): metadata providers over
	// the shikimori transport (AniList/Kitsu/anisearch/anidb).
	metaManager := metadata.NewManager(metadata.DefaultProviders(shikiNet), nil, nil)

	real := &realCore{
		registry: registry,
		store:    store,
		player:   player.New(player.Options{Bin: settings.Player.Path}),
		skips:    skip.NewManager(settings.Skip, shikiNet),
		dl:       download.New(download.Options{FFmpeg: "ffmpeg"}),
		shiki: shikimori.New(settings.Shikimori, shikiNet, nil,
			shikimori.WithTokenPersister(o.shikiPersister)),
		settings: settings,
	}

	dlService := &realDownload{manager: nil, core: real}
	real.downloadBridge = dlService
	manager := download.NewManager(settings.Download.MaxConcurrency, real.runDownload)
	dlService.manager = manager
	// PR35/PR36: the torrent engine is fully lazy (client + listeners
	// on the first link) and is the registry's ONE shared client — the
	// torrent search providers (nyaa, anilibria-torrent, animetosho,
	// tokyotosho) resolve their picks through it. The PR40 removal of
	// the «Торренты» menu changed nothing here: the engine stays
	// provider-side, including its teardown via Registry.Close.
	deps := &Deps{
		Search:   &realSearch{registry: registry},
		Episode:  &realEpisode{registry: registry},
		Playback: &realPlayback{player: real.player, skips: real.skips},
		History:  &realHistory{store: store},
		Offline:  &realOffline{settings: settings},
		Database: &realDatabase{store: store},
		Health:   &realHealth{registry: registry, timeout: settings.Network.ConnectTimeout},
		Shiki:    &realShiki{client: real.shiki, enabled: settings.Shikimori.Enabled},
		Download: &realDownload{manager: manager, core: real},
		Metadata: realMetadata{manager: metaManager},
		// The per-provider fan-out ceiling (network.search_timeout,
		// default 30s — PR24).
		SearchTimeout: settings.Network.SearchTimeout,
	}
	return &RealDeps{
		Deps: deps, Store: store, Downloads: manager, ShikiNet: shikiNet,
		registry: registry,
	}, nil
}

// realMetadata adapts the metadata manager onto the TUI service
// interface.
type realMetadata struct{ manager *metadata.Manager }

// SearchAlternativeTitles resolves the alias set of one query.
func (m realMetadata) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	return m.manager.SearchAlternativeTitles(ctx, query)
}

// Close releases the background resources. The netclient needs no
// teardown (it owns no goroutines), so only the download manager, the
// registry (CF bypass stack + shared torrent engine) and the store
// are settled.
func (r *RealDeps) Close() {
	if r.Downloads != nil {
		_ = r.Downloads.Close()
	}
	if r.registry != nil {
		_ = r.registry.Close()
	}
}

// realCore carries the shared production handles.
type realCore struct {
	registry       *providers.Registry
	store          *storage.Store
	player         *player.Player
	skips          *skip.Manager
	dl             *download.Downloader
	shiki          *shikimori.Client
	settings       config.Settings
	downloadBridge *realDownload
}

// --- SearchService ---

type realSearch struct{ registry *providers.Registry }

func (s *realSearch) Providers() []ProviderMeta {
	list := s.registry.List()
	out := make([]ProviderMeta, 0, len(list))
	for _, p := range list {
		out = append(out, ProviderMeta{ID: p.ID(), Name: p.Name()})
	}
	return out
}

// DisabledProviders surfaces the startup exclusion set (PR24).
func (s *realSearch) DisabledProviders() []providers.DisabledProvider {
	return s.registry.Disabled()
}

func (s *realSearch) Search(ctx context.Context, providerID, query string) ([]contracts.SearchResult, error) {
	p, ok := s.registry.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("tui: unknown provider %q", providerID)
	}
	return p.Search(ctx, query)
}

// NamePreference surfaces the provider's search-name preference (PR42).
func (s *realSearch) NamePreference(providerID string) contracts.NamePreference {
	return s.registry.NamePreference(providerID)
}

// --- EpisodeService ---

type realEpisode struct{ registry *providers.Registry }

func (s *realEpisode) GetEpisodes(ctx context.Context, providerID, animeURL string) ([]contracts.Episode, error) {
	p, ok := s.registry.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("tui: unknown provider %q", providerID)
	}
	return p.GetEpisodes(ctx, animeURL)
}

func (s *realEpisode) ResolveStream(ctx context.Context, providerID string, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	p, ok := s.registry.Get(providerID)
	if !ok {
		return contracts.MediaStream{}, fmt.Errorf("tui: unknown provider %q", providerID)
	}
	return p.ResolveStream(ctx, episode, dubID)
}

func (s *realEpisode) ContentLanguage(providerID string) string {
	return s.registry.ContentLanguage(providerID)
}

// --- PlaybackService ---

type realPlayback struct {
	player *player.Player
	skips  *skip.Manager
}

func (s *realPlayback) ResolveSkips(ctx context.Context, shikimoriID int64, episode float64) (string, func(), error) {
	if shikimoriID == 0 {
		return "", func() {}, nil
	}
	bundle, err := s.skips.Resolve(ctx, skip.ResolveRequest{
		ShikimoriID: shikimoriID,
		EpisodeNum:  episode,
	})
	if err != nil {
		return "", func() {}, err
	}
	if bundle.Empty() {
		return "", func() {}, nil
	}
	path, err := bundle.WriteChaptersFile(os.TempDir())
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(path) }
	return path, cleanup, nil
}

func (s *realPlayback) Play(ctx context.Context, req PlayRequest) error {
	return s.player.Play(ctx, player.Request{
		URL:          req.URL,
		AudioURL:     req.AudioURL,
		Title:        req.Title,
		Headers:      req.Headers,
		ExtraMPVOpts: req.ExtraMPVOpts,
		ChaptersFile: req.ChaptersFile,
	})
}

// --- HistoryService ---

type realHistory struct{ store *storage.Store }

func (s *realHistory) List(ctx context.Context) ([]storage.AnimeProgress, error) {
	return s.store.Progress.ListHistory(ctx, "", 0, 0)
}

func (s *realHistory) SavePlayback(ctx context.Context, rec storage.AnimeProgress, episode, videoDub, audioDub string) error {
	rec.CurrentEpisode = episode
	rec.VideoDub = &videoDub
	rec.AudioDub = &audioDub
	rec.UpdatedAt = time.Now().UTC()
	return s.store.Progress.Upsert(ctx, &rec)
}

// BindSource moves a record onto a new source pair (python
// search_and_bind record patch): the modified row is upserted under
// the new (source_id, source_url) key and the old row removed.
func (s *realHistory) BindSource(ctx context.Context, id int64, sourceID, sourceURL string) error {
	rec, err := s.store.Progress.GetByID(ctx, id)
	if err != nil {
		return err
	}
	old := *rec
	rec.SourceID = sourceID
	rec.SourceURL = sourceURL
	rec.NeedsCorrection = false
	rec.BoundTitle = &rec.Title
	rec.UpdatedAt = time.Now().UTC()
	if err := s.store.Progress.Upsert(ctx, rec); err != nil {
		return err
	}
	if old.SourceID != sourceID || old.SourceURL != sourceURL {
		return s.store.Progress.Delete(ctx, old.ID)
	}
	return nil
}

// GetByShikimoriID loads the row bound to a shikimori anime; a miss
// maps to (nil, nil) so TUI callers treat it as "no stored rate".
func (s *realHistory) GetByShikimoriID(ctx context.Context, shikimoriID int64) (*storage.AnimeProgress, error) {
	rec, err := s.store.Progress.GetByShikimoriID(ctx, shikimoriID)
	if err != nil {
		if errors.Is(err, contracts.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

// SetRateID persists the shikimori rate id of a history row.
func (s *realHistory) SetRateID(ctx context.Context, animeID, rateID int64) error {
	return s.store.Progress.SetRateID(ctx, animeID, rateID)
}

// --- OfflineService ---

type realOffline struct{ settings config.Settings }

func (s *realOffline) Titles() ([]OfflineTitle, error) {
	dir := s.settings.Download.Dir
	if dir == "" {
		base, err := config.DataDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "downloads")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tui: read download dir: %w", err)
	}
	var out []OfflineTitle
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		snapshot, err := download.LoadSnapshot(path)
		if err != nil {
			continue // malformed index reads as empty (python parity)
		}
		if snapshot.TotalDownloaded() == 0 {
			continue
		}
		out = append(out, OfflineTitle{Dir: path, Name: e.Name(), Snapshot: snapshot})
	}
	return out, nil
}

// --- DatabaseService ---

type realDatabase struct{ store *storage.Store }

func (s *realDatabase) ClearSkips(ctx context.Context) (int64, error) {
	return s.store.Skips.ClearAll(ctx)
}

func (s *realDatabase) ClearHistory(ctx context.Context) (int64, error) {
	items, err := s.store.Progress.ListHistory(ctx, "", 0, 0)
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, it := range items {
		if err := s.store.Progress.Delete(ctx, it.ID); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *realDatabase) ClearAll(ctx context.Context) (int64, int64, error) {
	h, err := s.ClearHistory(ctx)
	if err != nil {
		return 0, 0, err
	}
	sk, err := s.store.Skips.ClearAll(ctx)
	if err != nil {
		return h, 0, err
	}
	return h, sk, nil
}

// --- HealthService ---

type realHealth struct {
	registry *providers.Registry
	timeout  time.Duration
}

func (s *realHealth) Check(ctx context.Context, providerID string) error {
	p, ok := s.registry.Get(providerID)
	if !ok {
		return fmt.Errorf("tui: unknown provider %q", providerID)
	}
	budget := s.timeout
	if budget <= 0 {
		budget = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	_, err := p.Search(ctx, "test")
	return err
}

// --- ShikimoriService ---

type realShiki struct {
	client  *shikimori.Client
	enabled bool
}

func (s *realShiki) Enabled() bool { return s.enabled }

// Mode reports the client's auth-mode diagnostic (PR25 A: "disabled"
// gates the enrichment out of hybrid search).
func (s *realShiki) Mode() string { return s.client.Mode() }

func (s *realShiki) UpdateStatus(ctx context.Context, shikimoriID, rateID int64, status string, score, rewatches *int) (int64, error) {
	input := shikimori.RateInput{
		Status:    shikimori.CanonicalStatus(status),
		Score:     score,
		Rewatches: rewatches,
	}
	if rateID > 0 {
		return s.client.UpdateRate(ctx, rateID, input)
	}
	return s.client.CreateRate(ctx, shikimoriID, input)
}

// SearchIDs maps candidate titles to shikimori anime ids.
func (s *realShiki) SearchIDs(ctx context.Context, query string) (map[string]int64, error) {
	return s.client.SearchIDs(ctx, query)
}

// Autocomplete resolves the rich autocomplete records (PR42).
func (s *realShiki) Autocomplete(ctx context.Context, query string, limit int) ([]shikimori.AutocompleteItem, error) {
	return s.client.Autocomplete(ctx, query, limit)
}

// --- DownloadService ---

// realDownload bridges the TUI tasks onto the background manager.
// The manager's Runner only receives download.Task, so the resolve
// parts (provider, episode, dub, quality, shikimori binding) are
// remembered under the task ID and recovered inside the runner.
type realDownload struct {
	manager *download.Manager
	core    *realCore

	mu    sync.Mutex
	parts map[string]DownloadTask
}

func (s *realDownload) Download(ctx context.Context, task DownloadTask) error {
	return s.core.downloadOne(ctx, task)
}

func (s *realDownload) Submit(task DownloadTask) {
	id := downloadTaskID(task)
	s.mu.Lock()
	if s.parts == nil {
		s.parts = make(map[string]DownloadTask)
	}
	// Prune parts of tasks the manager already settled (done/failed):
	// they can never run again, and a resubmit re-inserts its parts
	// under this same lock, so the check-and-drop is race-free (M13).
	for partID := range s.parts {
		if t, ok := s.manager.Task(partID); ok &&
			(t.State == download.StateDone || t.State == download.StateFailed) {
			delete(s.parts, partID)
		}
	}
	s.parts[id] = task
	s.mu.Unlock()
	s.manager.Submit(download.Task{
		ID:         id,
		Title:      task.AnimeTitle,
		EpisodeNum: task.EpisodeNum,
	})
}

func (s *realDownload) ActiveBanner() string {
	active := s.manager.ActiveTasks()
	if len(active) == 0 {
		return ""
	}
	return fmt.Sprintf("⬇ Фоновых загрузок: %d", len(active))
}

// recall fetches the resolve parts of one manager task.
func (s *realDownload) recall(id string) (DownloadTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.parts[id]
	return task, ok
}

// downloadTaskID builds the dedupe key of one download.
func downloadTaskID(task DownloadTask) string {
	return fmt.Sprintf("%s|%s|%s|%s", task.AnimeTitle, task.EpisodeNum, task.DubID, task.Quality)
}

// runDownload adapts the manager Runner onto downloadOne.
func (c *realCore) runDownload(ctx context.Context, task download.Task, _ func(float64)) error {
	bridge := c.downloadBridge
	if bridge == nil {
		return fmt.Errorf("tui: download: task bridge unavailable")
	}
	parts, ok := bridge.recall(task.ID)
	if !ok {
		return fmt.Errorf("tui: download: task %q lost its parts", task.ID)
	}
	return c.downloadOne(ctx, parts)
}

// downloadOne performs the full pipeline: resolve stream, resolve
// skips, merge via ffmpeg, index the result (python
// handle_download_flow foreground path).
func (c *realCore) downloadOne(ctx context.Context, task DownloadTask) error {
	p, ok := c.registry.Get(task.ProviderID)
	if !ok {
		return fmt.Errorf("tui: download: unknown provider %q", task.ProviderID)
	}

	stream, err := p.ResolveStream(ctx, task.Episode, task.DubID)
	if err != nil {
		return fmt.Errorf("tui: download: resolve: %w", err)
	}
	quality := pickQuality(stream.Links, task.Quality)
	video, ok := stream.Links[quality]
	if !ok {
		return fmt.Errorf("tui: download: quality %q unavailable", quality)
	}

	var audio *contracts.VideoSource
	audioKey := audioKeyOfTask(task)
	if audioKey != "" && audioKey != task.DubID {
		if ap, ok := c.registry.Get(providerOfTrackKey(audioKey)); ok {
			audioStream, err := ap.ResolveStream(ctx, task.Episode, audioKey)
			if err == nil && len(audioStream.Links) > 0 {
				best := audioStream.Links[sortedQualityDesc(audioStream.Links)[0]]
				audio = &best
			}
		}
	}

	chaptersFile := ""
	if task.ShikimoriID > 0 && c.skips != nil {
		bundle, err := c.skips.Resolve(ctx, skip.ResolveRequest{
			ShikimoriID: task.ShikimoriID,
			EpisodeNum:  EpisodeSortKey(task.Episode.Num),
		})
		if err == nil && !bundle.Empty() {
			if path, err := bundle.WriteChaptersFile(os.TempDir()); err == nil {
				chaptersFile = path
				defer func() { _ = os.Remove(path) }()
			}
		}
	}

	outDir := filepath.Join(c.downloadRoot(), download.CleanTitleForFS(task.AnimeTitle))
	name := fmt.Sprintf("EP_%s_%s_%sp.mp4",
		sanitizeFragment(task.Episode.Num), sanitizeFilename(task.DubID), quality)
	outPath := filepath.Join(outDir, name)

	input := download.CommandInput{
		Video:        video,
		Audio:        audio,
		ChaptersFile: chaptersFile,
		OutputPath:   outPath,
		Title:        fmt.Sprintf("%s — серия %s", task.AnimeTitle, task.Episode.Num),
	}
	if err := c.dl.Download(ctx, input); err != nil {
		return err
	}

	var animeID *int64
	if task.ShikimoriID > 0 {
		if rec, err := c.store.Progress.GetByShikimoriID(ctx, task.ShikimoriID); err == nil && rec != nil {
			animeID = &rec.ID
		}
	}
	key := audioKey
	if key == "" {
		key = task.DubID
	}
	return download.UpsertEntry(outDir, download.EntryInput{
		EpisodeNum: task.Episode.Num,
		AnimeID:    animeID,
		VideoKey:   task.DubID,
		AudioKey:   key,
		Quality:    qualityIntOf(quality),
		FilePath:   outPath,
	})
}

// audioKeyOfTask recovers the separate audio dub of a task (the
// session records it in the DubID composition when present).
func audioKeyOfTask(task DownloadTask) string {
	if before, _, found := strings.Cut(task.DubID, "\x1f+"); found {
		return before
	}
	return ""
}

// downloadRoot resolves the configured download directory.
func (c *realCore) downloadRoot() string {
	if c.settings.Download.Dir != "" {
		return c.settings.Download.Dir
	}
	base, err := config.DataDir()
	if err != nil {
		return "."
	}
	return filepath.Join(base, "downloads")
}

// pickQuality picks the preferred quality or the best available.
func pickQuality(links map[string]contracts.VideoSource, preferred string) string {
	if preferred != "" {
		if _, ok := links[preferred]; ok {
			return preferred
		}
	}
	sorted := sortedQualityDesc(links)
	if len(sorted) == 0 {
		return "720"
	}
	return sorted[0]
}

func qualityIntOf(q string) int {
	n, _ := strconv.Atoi(q)
	return n
}

// unsafePathChars matches everything but letters, digits and a few
// separators for file-name fragments.
var unsafePathChars = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// sanitizeFilename strips brackets and slashes from dub keys.
func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "[", "")
	s = strings.ReplaceAll(s, "]", "")
	return sanitizeFragment(s)
}

// sanitizeFragment keeps only safe filename runes.
func sanitizeFragment(s string) string {
	return strings.Trim(unsafePathChars.ReplaceAllString(s, "_"), "_")
}
