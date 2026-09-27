package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MALMapRepo is the durable shikimori_id -> myanimelist_id mapping
// cache (PR112). The mapping is resolved from the Shikimori anime card
// once per title and persisted here, so the MAL sync never re-fetches
// a card it has already mapped. Existing history tables are untouched:
// the cache is an independent table (owner ruling: the local lists
// stay).
type MALMapRepo struct {
	db *sql.DB
}

// Get resolves the MAL id cached for the shikimori id. A miss is
// (0, false, nil) — never an error.
func (r *MALMapRepo) Get(ctx context.Context, shikimoriID int64) (int64, bool, error) {
	var malID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT mal_id FROM anime_mal_map WHERE shikimori_id = ?`, shikimoriID).
		Scan(&malID)
	switch {
	case err == sql.ErrNoRows:
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("mal map get %d: %w", shikimoriID, err)
	}
	return malID, true, nil
}

// Set persists (and overwrites) the mapping for the shikimori id.
func (r *MALMapRepo) Set(ctx context.Context, shikimoriID, malID int64) error {
	if malID <= 0 {
		return fmt.Errorf("mal map set %d: invalid mal id %d", shikimoriID, malID)
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO anime_mal_map (shikimori_id, mal_id, updated_at)
VALUES (?,?,?)
ON CONFLICT (shikimori_id) DO UPDATE SET
	mal_id     = excluded.mal_id,
	updated_at = excluded.updated_at`,
		shikimoriID, malID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("mal map set %d->%d: %w", shikimoriID, malID, err)
	}
	return nil
}
