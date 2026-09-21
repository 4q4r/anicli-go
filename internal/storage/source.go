package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SourceRepo is the anime_source repository: the provider bindings of an
// anime_progress entry (python update_sources semantics).
type SourceRepo struct {
	db *sql.DB
}

// ListByAnime returns every provider source bound to one anime_progress
// row, in insertion order.
func (r *SourceRepo) ListByAnime(ctx context.Context, animeProgressID int64) ([]AnimeSource, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, anime_progress_id, source_id, source_url, video_dub, audio_dub, quality, last_resolved_at, created_at
		 FROM anime_source WHERE anime_progress_id = ? ORDER BY id`, animeProgressID)
	if err != nil {
		return nil, fmt.Errorf("list sources (anime %d): %w", animeProgressID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []AnimeSource
	for rows.Next() {
		s, err := scanAnimeSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source row: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListAll returns every provider source, in insertion order. The
// history-list endpoint batches with it instead of issuing one
// ListByAnime per row (the PR81-review P0 N+1: 1000 history rows made
// one /history call cost 1001 queries).
func (r *SourceRepo) ListAll(ctx context.Context) (map[int64][]AnimeSource, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, anime_progress_id, source_id, source_url, video_dub, audio_dub, quality, last_resolved_at, created_at
		 FROM anime_source ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list all sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	grouped := make(map[int64][]AnimeSource)
	for rows.Next() {
		s, err := scanAnimeSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source row: %w", err)
		}
		grouped[s.AnimeProgressID] = append(grouped[s.AnimeProgressID], *s)
	}
	return grouped, rows.Err()
}

// ReplaceForAnime atomically swaps the source list of one anime: existing
// rows are deleted and the given list inserted in a single transaction.
// AnimeProgressID of every inserted row is forced to animeProgressID; an
// empty (or nil) list clears all bindings.
func (r *SourceRepo) ReplaceForAnime(ctx context.Context, animeProgressID int64, sources []AnimeSource) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace sources (anime %d): begin: %w", animeProgressID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM anime_source WHERE anime_progress_id = ?`, animeProgressID,
	); err != nil {
		return fmt.Errorf("replace sources (anime %d): delete: %w", animeProgressID, err)
	}

	now := fmtTime(time.Now())
	for i := range sources {
		src := &sources[i]
		if _, err := tx.ExecContext(ctx, `
INSERT INTO anime_source
	(anime_progress_id, source_id, source_url, video_dub, audio_dub, quality, last_resolved_at, created_at)
VALUES (?,?,?,?,?,?,?,?)`,
			animeProgressID, src.SourceID, src.SourceURL,
			nullString(src.VideoDub), nullString(src.AudioDub), nullInt64(src.Quality),
			nullTimePtr(src.LastResolvedAt), now,
		); err != nil {
			return fmt.Errorf("replace sources (anime %d): insert %q: %w", animeProgressID, src.SourceID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace sources (anime %d): commit: %w", animeProgressID, err)
	}
	return nil
}

func scanAnimeSource(row rowScanner) (*AnimeSource, error) {
	var (
		s            AnimeSource
		vd, ad       sql.NullString
		quality      sql.NullInt64
		lastResolved sql.NullString
		created      string
	)
	err := row.Scan(&s.ID, &s.AnimeProgressID, &s.SourceID, &s.SourceURL,
		&vd, &ad, &quality, &lastResolved, &created)
	if err != nil {
		return nil, err
	}
	s.VideoDub = stringPtr(vd)
	s.AudioDub = stringPtr(ad)
	s.Quality = int64Ptr(quality)
	if s.LastResolvedAt, err = timePtr(lastResolved); err != nil {
		return nil, err
	}
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &s, nil
}
