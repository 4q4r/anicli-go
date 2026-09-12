// Package storage is the SQLite persistence layer of anicli-go, ported from
// the anicli-py schema (anicli/db/models.py plus the four alembic tables)
// onto hand-rolled SQL with modernc.org/sqlite (pure Go).
//
// Conventions:
//   - every column is snake_case, matching the Python schema;
//   - timestamps are TEXT in UTC ISO 8601 with fixed nine-digit
//     fractional seconds, so lexicographic ORDER BY equals chronological
//     order;
//   - nullable columns round-trip through pointer fields;
//   - sql.ErrNoRows maps to contracts.ErrNotFound;
//   - all queries are context-aware and use "?" placeholders only.
//
// Existing Python (anicli.db) databases are intentionally NOT migrated:
// the Go build starts from a fresh database; importing old data is a
// later, separate feature.
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// rowScanner is implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// timeLayout is the storage format for TEXT timestamps: ISO 8601 UTC with
// fixed-width nanoseconds ("2006-01-02T15:04:05.000000000Z").
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// fmtTime renders t for storage (always UTC).
func fmtTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime parses a stored TEXT timestamp.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp %q: %w", s, err)
	}
	return t, nil
}

func nullString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

// nullNonEmpty maps "" to NULL for columns where an empty string is not a
// meaningful value (dub preferences).
func nullNonEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func int64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func nullFloat64(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func float64Ptr(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	return &f.Float64
}

func nullTimePtr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: fmtTime(*t), Valid: true}
}

func timePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func btoi(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// AnimeProgress is the anime_progress row: the viewing-progress aggregate
// root (python anicli/db/models.py AnimeProgress plus the source binding,
// the dirty sync flag and the last-known playback seconds snapshot).
type AnimeProgress struct {
	ID              int64
	Title           string
	Poster          *string
	SourceID        string
	SourceURL       string
	CurrentEpisode  string
	VideoDub        *string
	AudioDub        *string
	ShikimoriTitle  *string
	BoundTitle      *string
	BoundSimilarity *float64
	ShikimoriID     *int64
	ShikimoriRateID *int64
	ShikimoriStatus string
	Score           int
	TotalEpisodes   int
	Rewatches       int
	NeedsCorrection bool
	ProgressSeconds int64
	TotalSeconds    int64
	Dirty           bool
	UpdatedAt       time.Time
}

// EpisodeProgress is the anime_episode_progress row: per-episode playback
// position (python alembic 20260404_000001).
type EpisodeProgress struct {
	ID          int64
	AnimeID     int64
	Episode     string
	PositionSec int64
	DurationSec int64
	VideoKey    *string
	AudioKey    *string
	Quality     *int64
	UpdatedAt   time.Time
}

// PredictedSkipTime is the predicted_skip_time row: locally predicted skip
// intervals keyed by (shikimori_id, episode_num, skip_type)
// (python anicli/db/models.py PredictedSkipTime).
type PredictedSkipTime struct {
	ID            int64
	ShikimoriID   int64
	EpisodeNum    float64
	SkipType      string
	StartTime     float64
	EndTime       float64
	EpisodeLength float64
	CreatedAt     time.Time
}

// AnimeDetails is the shikimori_anime_details_cache row: a JSON payload per
// shikimori anime with separate immutable/mutable freshness stamps
// (python alembic 20260415_000002).
type AnimeDetails struct {
	ID                int64
	AnimeID           int64
	PayloadJSON       string
	ImmutableCachedAt time.Time
	MutableUpdatedAt  time.Time
	UpdatedAt         time.Time
}

// SetPayload marshals v into PayloadJSON.
func (d *AnimeDetails) SetPayload(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal details payload: %w", err)
	}
	d.PayloadJSON = string(b)
	return nil
}

// DecodePayload unmarshals PayloadJSON into v.
func (d *AnimeDetails) DecodePayload(v any) error {
	if err := json.Unmarshal([]byte(d.PayloadJSON), v); err != nil {
		return fmt.Errorf("unmarshal details payload: %w", err)
	}
	return nil
}

// AnimeSource is the anime_source row: one bound provider source of an
// anime_progress entry (python alembic 20260628_000003).
type AnimeSource struct {
	ID              int64
	AnimeProgressID int64
	SourceID        string
	SourceURL       string
	VideoDub        *string
	AudioDub        *string
	Quality         *int64
	LastResolvedAt  *time.Time
	CreatedAt       time.Time
}

// AutoDownloadRule is the auto_download_rule row: one rule per anime
// (python alembic 20260628_000003).
type AutoDownloadRule struct {
	ID                int64
	AnimeID           int64
	Enabled           bool
	PreferredSourceID *string
	PreferredVideoDub *string
	PreferredQuality  int64
	MaxNewEpisodes    int64
	Title             *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ProviderStat is the provider_search_stat row: compact response-frequency
// counters per provider, feeding the search priority score (python
// ProviderSearchStat, kept columns only).
type ProviderStat struct {
	ProviderID   string
	Successes    int64
	Failures     int64
	AvgLatencyMS float64
	LastUsedAt   time.Time
}

// PriorityScore ports python DBService._provider_priority_score
// (`success*total/(avg_latency+1) - (error+timeout)*100 - empty*10`) onto
// the compact columns: failures merges error/timeout/empty and attempts
// replaces total_results, preserving the semantics — success volume scaled
// by attempt volume, latency-normalized, with a dominant failure penalty:
//
//	successes * attempts / (avg_latency_ms + 1) - failures * 100
//
// A never-used provider scores 0.
func (p ProviderStat) PriorityScore() float64 {
	attempts := p.Successes + p.Failures
	if attempts == 0 {
		return 0
	}
	return float64(p.Successes)*float64(attempts)/(p.AvgLatencyMS+1) - float64(p.Failures)*100
}
