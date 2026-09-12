package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DetailsCacheRepo is the shikimori_anime_details_cache repository.
type DetailsCacheRepo struct {
	db *sql.DB
}

// Get loads the cached details payload of one Shikimori anime.
func (r *DetailsCacheRepo) Get(ctx context.Context, animeID int64) (*AnimeDetails, error) {
	d, err := scanAnimeDetails(r.db.QueryRowContext(ctx,
		`SELECT id, anime_id, payload_json, immutable_cached_at, mutable_updated_at, updated_at
		 FROM shikimori_anime_details_cache WHERE anime_id = ?`, animeID))
	if err != nil {
		return nil, notFound(fmt.Errorf("get anime details %d: %w", animeID, err))
	}
	return d, nil
}

// Upsert stores the details payload of one Shikimori anime. On conflict the
// payload and the mutable freshness stamps are refreshed;
// immutable_cached_at keeps the value of the first cache fill (the
// immutable part of the payload never changes identity).
func (r *DetailsCacheRepo) Upsert(ctx context.Context, d *AnimeDetails) error {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO shikimori_anime_details_cache
	(anime_id, payload_json, immutable_cached_at, mutable_updated_at, updated_at)
VALUES (?,?,?,?,?)
ON CONFLICT (anime_id) DO UPDATE SET
	payload_json       = excluded.payload_json,
	mutable_updated_at = excluded.mutable_updated_at,
	updated_at         = excluded.updated_at`,
		d.AnimeID, d.PayloadJSON, fmtTime(d.ImmutableCachedAt),
		fmtTime(d.MutableUpdatedAt), fmtTime(d.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert anime details %d: %w", d.AnimeID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("upsert anime details %d: row id: %w", d.AnimeID, err)
	}
	d.ID = id
	return nil
}

// PurgeOlder removes cache entries whose immutable_cached_at is strictly
// older than cutoff and returns the number of removed rows.
func (r *DetailsCacheRepo) PurgeOlder(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM shikimori_anime_details_cache WHERE immutable_cached_at < ?`,
		fmtTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge old anime details: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge old anime details: rows affected: %w", err)
	}
	return n, nil
}

func scanAnimeDetails(row rowScanner) (*AnimeDetails, error) {
	var (
		d                       AnimeDetails
		immutable, mutable, upd string
	)
	err := row.Scan(&d.ID, &d.AnimeID, &d.PayloadJSON, &immutable, &mutable, &upd)
	if err != nil {
		return nil, err
	}
	if d.ImmutableCachedAt, err = parseTime(immutable); err != nil {
		return nil, err
	}
	if d.MutableUpdatedAt, err = parseTime(mutable); err != nil {
		return nil, err
	}
	if d.UpdatedAt, err = parseTime(upd); err != nil {
		return nil, err
	}
	return &d, nil
}
