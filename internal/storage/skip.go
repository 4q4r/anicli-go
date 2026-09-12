package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SkipPredictionRepo is the predicted_skip_time repository.
type SkipPredictionRepo struct {
	db *sql.DB
}

// Get returns every predicted skip interval of one episode of one anime.
func (r *SkipPredictionRepo) Get(ctx context.Context, shikimoriID int64, episodeNum float64) ([]PredictedSkipTime, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, shikimori_id, episode_num, skip_type, start_time, end_time, episode_length, created_at
		 FROM predicted_skip_time WHERE shikimori_id = ? AND episode_num = ?`,
		shikimoriID, episodeNum)
	if err != nil {
		return nil, fmt.Errorf("get predicted skips (%d, %v): %w", shikimoriID, episodeNum, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PredictedSkipTime
	for rows.Next() {
		var s PredictedSkipTime
		var createdAt string
		if err := rows.Scan(&s.ID, &s.ShikimoriID, &s.EpisodeNum, &s.SkipType,
			&s.StartTime, &s.EndTime, &s.EpisodeLength, &createdAt); err != nil {
			return nil, fmt.Errorf("scan predicted skip row: %w", err)
		}
		if s.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Save upserts the skip intervals of one episode: existing intervals of the
// same skip type are updated in place, new ones inserted (python's
// re-analysis fix), all within one transaction.
func (r *SkipPredictionRepo) Save(ctx context.Context, shikimoriID int64, episodeNum float64,
	skipData map[string][2]float64, episodeLength float64,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save predicted skips: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	now := fmtTime(time.Now())
	for skipType, times := range skipData {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO predicted_skip_time
	(shikimori_id, episode_num, skip_type, start_time, end_time, episode_length, created_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT (shikimori_id, episode_num, skip_type) DO UPDATE SET
	start_time     = excluded.start_time,
	end_time       = excluded.end_time,
	episode_length = excluded.episode_length`,
			shikimoriID, episodeNum, skipType, times[0], times[1], episodeLength, now,
		); err != nil {
			return fmt.Errorf("save predicted skip %q (%d, %v): %w", skipType, shikimoriID, episodeNum, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save predicted skips: commit: %w", err)
	}
	return nil
}

// ClearAll deletes every predicted skip interval and returns the number of
// removed rows.
func (r *SkipPredictionRepo) ClearAll(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM predicted_skip_time`)
	if err != nil {
		return 0, fmt.Errorf("clear predicted skips: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear predicted skips: rows affected: %w", err)
	}
	return n, nil
}
