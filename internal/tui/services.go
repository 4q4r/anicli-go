package tui

import (
	"context"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/storage"
)

// ProviderMeta identifies one search provider in status tables.
type ProviderMeta struct {
	ID   string
	Name string
}

// SearchService fans searches out to providers. The screen issues one
// command per provider so bubbletea owns the concurrency; each call
// resolves independently into a live status row (python
// search_provider_task port).
type SearchService interface {
	// Providers lists the registered (searchable) providers in
	// registry order — the fan-out roster.
	Providers() []ProviderMeta
	// DisabledProviders lists providers excluded at startup for
	// missing configuration (PR24): they never join the fan-out and
	// the health surface renders them as ОТКЛЮЧЁН.
	DisabledProviders() []providers.DisabledProvider
	// Search queries one provider. Errors mark the provider's row as
	// failed but never abort the fan-out.
	Search(ctx context.Context, providerID, query string) ([]contracts.SearchResult, error)
}

// MetadataService resolves alternative titles for the hybrid search
// (PR24): the Shikimori-matched title expands into aliases from the
// metadata providers (AniList/Kitsu/anisearch/anidb).
type MetadataService interface {
	// SearchAlternativeTitles returns normalized unique aliases for a
	// query. Errors mean "no enrichment"; callers fall back.
	SearchAlternativeTitles(ctx context.Context, query string) ([]string, error)
}

// EpisodeService lists episodes and resolves streams for one source.
type EpisodeService interface {
	// GetEpisodes lists the episodes of the anime at animeURL on the
	// given provider.
	GetEpisodes(ctx context.Context, providerID, animeURL string) ([]contracts.Episode, error)
	// ResolveStream resolves playable quality links for one episode
	// and dub.
	ResolveStream(ctx context.Context, providerID string, episode contracts.Episode, dubID string) (contracts.MediaStream, error)
	// ContentLanguage reports the provider's primary content language
	// ("ru", "ja", …) used to tag its dubs in the pickers; "" when
	// unknown (PR23).
	ContentLanguage(providerID string) string
}

// PlaybackService plays a resolved stream through mpv with skip
// chapters (skip.Resolve) attached.
type PlaybackService interface {
	// ResolveSkips resolves the skip chapter file for the episode, or
	// "" when none. The returned cleanup removes the temp file.
	ResolveSkips(ctx context.Context, shikimoriID int64, episode float64) (path string, cleanup func(), err error)
	// Play launches mpv and blocks until it exits.
	Play(ctx context.Context, req PlayRequest) error
}

// PlayRequest is one playback invocation (player.Request flattened).
type PlayRequest struct {
	URL          string
	AudioURL     string
	Title        string
	Headers      map[string]string
	ExtraMPVOpts []string
	ChaptersFile string
}

// HistoryService reads and writes viewing progress.
type HistoryService interface {
	// List returns the full history (all statuses), newest first.
	List(ctx context.Context) ([]storage.AnimeProgress, error)
	// SavePlayback records the watched episode and dub preferences.
	SavePlayback(ctx context.Context, rec storage.AnimeProgress, episode, videoDub, audioDub string) error
	// BindSource rebinds a record onto a new (source_id, url) pair
	// (python search_and_bind record patch).
	BindSource(ctx context.Context, id int64, sourceID, sourceURL string) error
	// GetByShikimoriID loads the record bound to a shikimori anime,
	// nil when none is bound.
	GetByShikimoriID(ctx context.Context, shikimoriID int64) (*storage.AnimeProgress, error)
	// SetRateID persists the shikimori rate (list entry) id of a
	// record (SyncEpisodeProgress pattern: PATCH vs Create dispatch).
	SetRateID(ctx context.Context, animeID, rateID int64) error
}

// OfflineTitle is one downloaded title directory with its snapshot.
type OfflineTitle struct {
	// Dir is the title directory path.
	Dir string
	// Name is the directory base name.
	Name string
	// Snapshot is the validated offline index view.
	Snapshot download.Snapshot
}

// OfflineService exposes the downloaded library.
type OfflineService interface {
	// Titles enumerates title directories with at least one indexed
	// episode.
	Titles() ([]OfflineTitle, error)
}

// DatabaseService backs «🗄️ Управление БД».
type DatabaseService interface {
	// ClearSkips deletes predicted skip times, returning the count.
	ClearSkips(ctx context.Context) (int64, error)
	// ClearHistory deletes viewing history, returning the count.
	ClearHistory(ctx context.Context) (int64, error)
	// ClearAll wipes both, returning (history, skips) counts.
	ClearAll(ctx context.Context) (history, skips int64, err error)
}

// HealthService backs «🛠 Проверка».
type HealthService interface {
	// Check runs one lightweight provider search ("test") with a
	// connect-timeout budget (python check_provider_safe port).
	Check(ctx context.Context, providerID string) error
}

// ShikimoriService is the manual status-update surface of «Изменить
// инфо» plus the title→id binding lookup.
type ShikimoriService interface {
	// Enabled reports whether the tracker integration is active.
	Enabled() bool
	// Mode reports the client's auth mode diagnostic ("disabled",
	// "none", "cookie" or "bearer"); "disabled" means the enrichment
	// and authed flows must skip Shikimori entirely (PR25 A).
	Mode() string
	// UpdateStatus patches the rate of one anime; nil score/rewatches
	// leave them untouched. rateID > 0 PATCHes the existing rate,
	// otherwise a new rate is created and its id returned.
	UpdateStatus(ctx context.Context, shikimoriID, rateID int64, status string, score, rewatches *int) (int64, error)
	// SearchIDs maps candidate titles to shikimori anime ids (python
	// search_ids autocomplete port).
	SearchIDs(ctx context.Context, query string) (map[string]int64, error)
}

// DownloadTask is one episode download submitted by «Скачать серии».
type DownloadTask struct {
	// AnimeTitle is the display title (feeds output paths).
	AnimeTitle string
	// EpisodeNum is the episode label.
	EpisodeNum string
	// ShikimoriID binds the download to the skip lookup; 0 skips it.
	ShikimoriID int64
	// ProviderID is the source provider.
	ProviderID string
	// Episode identifies the episode for stream resolution.
	Episode contracts.Episode
	// DubID is the selected dub.
	DubID string
	// Quality is the preferred resolution label.
	Quality string
}

// DownloadResult reports one finished (or failed) download.
type DownloadResult struct {
	// Task identifies the download.
	Task DownloadTask
	// Err is nil on success.
	Err error
}

// DownloadService backs «Скачать серии» with foreground and background
// modes.
type DownloadService interface {
	// Download runs one download to completion (foreground mode).
	Download(ctx context.Context, task DownloadTask) error
	// Submit queues one background download.
	Submit(task DownloadTask)
	// ActiveBanner renders the background-task summary line ("" when
	// idle).
	ActiveBanner() string
}
