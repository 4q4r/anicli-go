package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
)

// EpisodeProgressRepo is the anime_episode_progress aggregate repository.
type EpisodeProgressRepo struct {
	db *sql.DB
}

// Upsert inserts a per-episode playback row or replaces position,
// duration, stream keys and quality when (anime_id, episode) already
// exists. The row ID is written back into e.
func (r *EpisodeProgressRepo) Upsert(ctx context.Context, e *EpisodeProgress) error {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO anime_episode_progress
	(anime_id, episode, position_sec, duration_sec, video_key, audio_key, quality, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT (anime_id, episode) DO UPDATE SET
	position_sec = excluded.position_sec,
	duration_sec = excluded.duration_sec,
	video_key    = excluded.video_key,
	audio_key    = excluded.audio_key,
	quality      = excluded.quality,
	updated_at   = excluded.updated_at`,
		e.AnimeID, e.Episode, e.PositionSec, e.DurationSec,
		nullString(e.VideoKey), nullString(e.AudioKey), nullInt64(e.Quality),
		fmtTime(e.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert episode progress (anime %d, episode %q): %w", e.AnimeID, e.Episode, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("upsert episode progress: row id: %w", err)
	}
	e.ID = id
	return nil
}

// GetByID loads one episode progress row by primary key.
func (r *EpisodeProgressRepo) GetByID(ctx context.Context, id int64) (*EpisodeProgress, error) {
	e, err := scanEpisodeProgress(r.db.QueryRowContext(ctx,
		`SELECT id, anime_id, episode, position_sec, duration_sec, video_key, audio_key, quality, updated_at
		 FROM anime_episode_progress WHERE id = ?`, id))
	if err != nil {
		return nil, notFound(fmt.Errorf("get episode progress %d: %w", id, err))
	}
	return e, nil
}

// ListByAnime returns all episode rows of one anime in natural order:
// episode numbers that both parse as numbers compare numerically ("2" <
// "10"), anything else compares lexically and lands after the numbers
// ("2" < "10" < "OVA").
func (r *EpisodeProgressRepo) ListByAnime(ctx context.Context, animeID int64) ([]EpisodeProgress, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, anime_id, episode, position_sec, duration_sec, video_key, audio_key, quality, updated_at
		 FROM anime_episode_progress WHERE anime_id = ? ORDER BY id`, animeID)
	if err != nil {
		return nil, fmt.Errorf("list episode progress (anime %d): %w", animeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []EpisodeProgress
	for rows.Next() {
		e, err := scanEpisodeProgress(rows)
		if err != nil {
			return nil, fmt.Errorf("scan episode progress row: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate episode progress: %w", err)
	}
	sortEpisodesNatural(out)
	return out, nil
}

// GetByEpisode loads one episode progress row by (anime, episode);
// missing rows map to contracts.ErrNotFound.
func (r *EpisodeProgressRepo) GetByEpisode(ctx context.Context, animeID int64, episode string) (*EpisodeProgress, error) {
	e, err := scanEpisodeProgress(r.db.QueryRowContext(ctx,
		`SELECT id, anime_id, episode, position_sec, duration_sec, video_key, audio_key, quality, updated_at
		 FROM anime_episode_progress WHERE anime_id = ? AND episode = ?`, animeID, episode))
	if err != nil {
		return nil, notFound(fmt.Errorf("get episode progress (anime %d, episode %q): %w", animeID, episode, err))
	}
	return e, nil
}

// LatestByAnime returns the most recently updated episode row of one
// anime (the GET /api/v1/history/{anime_id}/progress default when no
// episode is requested).
func (r *EpisodeProgressRepo) LatestByAnime(ctx context.Context, animeID int64) (*EpisodeProgress, error) {
	e, err := scanEpisodeProgress(r.db.QueryRowContext(ctx,
		`SELECT id, anime_id, episode, position_sec, duration_sec, video_key, audio_key, quality, updated_at
		 FROM anime_episode_progress WHERE anime_id = ?
		 ORDER BY updated_at DESC, id DESC LIMIT 1`, animeID))
	if err != nil {
		return nil, notFound(fmt.Errorf("latest episode progress (anime %d): %w", animeID, err))
	}
	return e, nil
}

func scanEpisodeProgress(row rowScanner) (*EpisodeProgress, error) {
	var (
		e                EpisodeProgress
		videoKey, audioK sql.NullString
		quality          sql.NullInt64
		updatedAt        string
	)
	err := row.Scan(&e.ID, &e.AnimeID, &e.Episode, &e.PositionSec, &e.DurationSec,
		&videoKey, &audioK, &quality, &updatedAt)
	if err != nil {
		return nil, err
	}
	e.VideoKey = stringPtr(videoKey)
	e.AudioKey = stringPtr(audioK)
	e.Quality = int64Ptr(quality)
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

// sortEpisodesNatural sorts ascending: numeric-vs-numeric compares by
// parsed value, every other pairing lexically.
func sortEpisodesNatural(eps []EpisodeProgress) {
	sort.SliceStable(eps, func(i, j int) bool {
		x, errX := strconv.ParseFloat(eps[i].Episode, 64)
		y, errY := strconv.ParseFloat(eps[j].Episode, 64)
		if errX == nil && errY == nil {
			return x < y
		}
		return eps[i].Episode < eps[j].Episode
	})
}
