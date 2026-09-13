package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// animeProgressCols is the shared column list; INSERT, SELECT and scan all
// follow this order.
const animeProgressCols = `title, poster, source_id, source_url, current_episode,
	video_dub, audio_dub, shikimori_title, bound_title, bound_similarity,
	shikimori_id, shikimori_rate_id, shikimori_status, score, total_episodes,
	rewatches, needs_correction, progress_seconds, total_seconds, dirty, updated_at`

const animeProgressSelect = `SELECT id, ` + animeProgressCols + ` FROM anime_progress`

// ProgressRepo is the anime_progress aggregate repository.
//
// Timestamp policy: Upsert stores the caller-supplied UpdatedAt verbatim
// (the import/export and sync flows need explicit timestamps); the
// targeted update methods stamp time.Now (python ORM onupdate behavior).
type ProgressRepo struct {
	db *sql.DB
}

// Upsert inserts the row or, when (source_id, source_url) already exists,
// replaces every column of the existing row (python save_progress
// source-first upsert; shikimori-id merge stays a service-layer concern).
// The row ID is written back into p.
func (r *ProgressRepo) Upsert(ctx context.Context, p *AnimeProgress) error {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO anime_progress (`+animeProgressCols+`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (source_id, source_url) DO UPDATE SET
	title             = excluded.title,
	poster            = excluded.poster,
	current_episode   = excluded.current_episode,
	video_dub         = excluded.video_dub,
	audio_dub         = excluded.audio_dub,
	shikimori_title   = excluded.shikimori_title,
	bound_title       = excluded.bound_title,
	bound_similarity   = excluded.bound_similarity,
	shikimori_id       = excluded.shikimori_id,
	shikimori_rate_id  = excluded.shikimori_rate_id,
	shikimori_status   = excluded.shikimori_status,
	score             = excluded.score,
	total_episodes    = excluded.total_episodes,
	rewatches         = excluded.rewatches,
	needs_correction  = excluded.needs_correction,
	progress_seconds  = excluded.progress_seconds,
	total_seconds     = excluded.total_seconds,
	dirty             = excluded.dirty,
	updated_at        = excluded.updated_at`,
		p.Title, nullString(p.Poster), p.SourceID, p.SourceURL, p.CurrentEpisode,
		nullString(p.VideoDub), nullString(p.AudioDub),
		nullString(p.ShikimoriTitle), nullString(p.BoundTitle), nullFloat64(p.BoundSimilarity),
		nullInt64(p.ShikimoriID), nullInt64(p.ShikimoriRateID), p.ShikimoriStatus,
		p.Score, p.TotalEpisodes, p.Rewatches, btoi(p.NeedsCorrection),
		p.ProgressSeconds, p.TotalSeconds, btoi(p.Dirty), fmtTime(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert anime progress: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("upsert anime progress: row id: %w", err)
	}
	p.ID = id
	return nil
}

// scanAnimeProgress maps one row (in animeProgressCols order) onto the
// struct.
func scanAnimeProgress(row rowScanner) (*AnimeProgress, error) {
	var (
		p                      AnimeProgress
		poster, vd, ad         sql.NullString
		shTitle, boundTitle    sql.NullString
		sim                    sql.NullFloat64
		shID, rateID           sql.NullInt64
		needsCorrection, dirty int64
		updatedAt              string
	)
	err := row.Scan(&p.ID, &p.Title, &poster, &p.SourceID, &p.SourceURL, &p.CurrentEpisode,
		&vd, &ad, &shTitle, &boundTitle, &sim, &shID, &rateID, &p.ShikimoriStatus,
		&p.Score, &p.TotalEpisodes, &p.Rewatches, &needsCorrection,
		&p.ProgressSeconds, &p.TotalSeconds, &dirty, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.Poster = stringPtr(poster)
	p.VideoDub = stringPtr(vd)
	p.AudioDub = stringPtr(ad)
	p.ShikimoriTitle = stringPtr(shTitle)
	p.BoundTitle = stringPtr(boundTitle)
	p.BoundSimilarity = float64Ptr(sim)
	p.ShikimoriID = int64Ptr(shID)
	p.ShikimoriRateID = int64Ptr(rateID)
	p.NeedsCorrection = needsCorrection != 0
	p.Dirty = dirty != 0
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetByID loads one anime_progress row by primary key.
func (r *ProgressRepo) GetByID(ctx context.Context, id int64) (*AnimeProgress, error) {
	p, err := scanAnimeProgress(
		r.db.QueryRowContext(ctx, animeProgressSelect+` WHERE id = ?`, id))
	if err != nil {
		return nil, notFound(fmt.Errorf("get anime progress %d: %w", id, err))
	}
	return p, nil
}

// GetByShikimoriID loads the most recently updated row bound to a Shikimori
// anime (python's shikimori-id fallback match).
func (r *ProgressRepo) GetByShikimoriID(ctx context.Context, shikimoriID int64) (*AnimeProgress, error) {
	p, err := scanAnimeProgress(r.db.QueryRowContext(ctx,
		animeProgressSelect+` WHERE shikimori_id = ? ORDER BY updated_at DESC, id DESC LIMIT 1`,
		shikimoriID))
	if err != nil {
		return nil, notFound(fmt.Errorf("get anime progress by shikimori id %d: %w", shikimoriID, err))
	}
	return p, nil
}

// ListHistory returns history rows newest-first. An empty status means all
// statuses; limit <= 0 means unlimited; offset only applies together with a
// positive limit.
func (r *ProgressRepo) ListHistory(ctx context.Context, status string, limit, offset int) ([]AnimeProgress, error) {
	q := animeProgressSelect
	var args []any
	if status != "" {
		q += ` WHERE shikimori_status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	if limit > 0 {
		if offset < 0 {
			offset = 0
		}
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list history (status=%q limit=%d offset=%d): %w", status, limit, offset, err)
	}
	defer func() { _ = rows.Close() }()

	var out []AnimeProgress
	for rows.Next() {
		p, err := scanAnimeProgress(rows)
		if err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpdatePlayback records the last playback position: current episode, dub
// preferences ("" clears to NULL) and the seconds snapshot, bumping
// updated_at.
func (r *ProgressRepo) UpdatePlayback(ctx context.Context, id int64,
	episode, videoDub, audioDub string, progressSeconds, totalSeconds int64,
) error {
	res, err := r.db.ExecContext(ctx, `
UPDATE anime_progress SET
	current_episode = ?, video_dub = ?, audio_dub = ?,
	progress_seconds = ?, total_seconds = ?, updated_at = ?
WHERE id = ?`,
		episode, nullNonEmpty(videoDub), nullNonEmpty(audioDub),
		progressSeconds, totalSeconds, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("update playback %d: %w", id, err)
	}
	return requireAffected(res, fmt.Errorf("update playback %d: %w", id, contracts.ErrNotFound))
}

// SetRateID stores the Shikimori rate (list entry) ID of an anime.
func (r *ProgressRepo) SetRateID(ctx context.Context, id int64, rateID int64) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE anime_progress SET shikimori_rate_id = ?, updated_at = ? WHERE id = ?`,
		rateID, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set rate id %d: %w", id, err)
	}
	return requireAffected(res, fmt.Errorf("set rate id %d: %w", id, contracts.ErrNotFound))
}

// MarkDirty flags an anime as needing sync toward Shikimori.
func (r *ProgressRepo) MarkDirty(ctx context.Context, id int64) error {
	return r.setDirty(ctx, id, true)
}

// ClearDirty drops the sync-pending flag after a successful sync.
func (r *ProgressRepo) ClearDirty(ctx context.Context, id int64) error {
	return r.setDirty(ctx, id, false)
}

func (r *ProgressRepo) setDirty(ctx context.Context, id int64, dirty bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE anime_progress SET dirty = ?, updated_at = ? WHERE id = ?`,
		btoi(dirty), fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set dirty %d = %v: %w", id, dirty, err)
	}
	return requireAffected(res, fmt.Errorf("set dirty %d = %v: %w", id, dirty, contracts.ErrNotFound))
}

// UpdateLocalStatus patches the caller-supplied subset of the local
// status fields (python db_service.update_local_status as consumed by
// PATCH /api/v1/history/{anime_id}): nil pointers leave their column
// untouched. A missing row fails with contracts.ErrNotFound.
func (r *ProgressRepo) UpdateLocalStatus(ctx context.Context, id int64,
	status *string, score *int, rewatches *int, episodes *string,
) error {
	sets := ""
	var args []any
	if status != nil {
		sets += ", shikimori_status = ?"
		args = append(args, *status)
	}
	if score != nil {
		sets += ", score = ?"
		args = append(args, *score)
	}
	if rewatches != nil {
		sets += ", rewatches = ?"
		args = append(args, *rewatches)
	}
	if episodes != nil {
		sets += ", current_episode = ?"
		args = append(args, *episodes)
	}

	args = append(args, fmtTime(time.Now()), id)

	query := `UPDATE anime_progress SET updated_at = ? WHERE id = ?`
	if sets != "" {
		query = `UPDATE anime_progress SET ` + sets[1:] + `, updated_at = ? WHERE id = ?`
	}
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update local status %d: %w", id, err)
	}
	return requireAffected(res, fmt.Errorf("update local status %d: %w", id, contracts.ErrNotFound))
}

// Delete removes an anime and, via ON DELETE CASCADE, its episode progress,
// sources and auto-download rule.
func (r *ProgressRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM anime_progress WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete anime progress %d: %w", id, err)
	}
	return requireAffected(res, fmt.Errorf("delete anime progress %d: %w", id, contracts.ErrNotFound))
}

// requireAffected turns a zero-rows UPDATE/DELETE into the wrapped
// not-found error.
func requireAffected(res sql.Result, notFoundErr error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return notFoundErr
	}
	return nil
}
