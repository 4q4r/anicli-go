package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// AutoRuleRepo is the auto_download_rule repository (one rule per anime).
type AutoRuleRepo struct {
	db *sql.DB
}

// List returns every auto-download rule, oldest first.
func (r *AutoRuleRepo) List(ctx context.Context) ([]AutoDownloadRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, anime_id, enabled, preferred_source_id, preferred_video_dub,
			preferred_quality, max_new_episodes, title, created_at, updated_at
		 FROM auto_download_rule ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list auto rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AutoDownloadRule
	for rows.Next() {
		rule, err := scanAutoRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan auto rule row: %w", err)
		}
		out = append(out, *rule)
	}
	return out, rows.Err()
}

// Upsert inserts the rule or, when a rule already exists for the anime,
// replaces its preferences and updated_at (created_at keeps the original).
// The row ID is written back into rule.
func (r *AutoRuleRepo) Upsert(ctx context.Context, rule *AutoDownloadRule) error {
	res, err := r.db.ExecContext(ctx, `
INSERT INTO auto_download_rule
	(anime_id, enabled, preferred_source_id, preferred_video_dub,
	 preferred_quality, max_new_episodes, title, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT (anime_id) DO UPDATE SET
	enabled             = excluded.enabled,
	preferred_source_id = excluded.preferred_source_id,
	preferred_video_dub = excluded.preferred_video_dub,
	preferred_quality   = excluded.preferred_quality,
	max_new_episodes    = excluded.max_new_episodes,
	title               = excluded.title,
	updated_at          = excluded.updated_at`,
		rule.AnimeID, btoi(rule.Enabled), nullString(rule.PreferredSourceID),
		nullString(rule.PreferredVideoDub), rule.PreferredQuality,
		rule.MaxNewEpisodes, nullString(rule.Title),
		fmtTime(rule.CreatedAt), fmtTime(rule.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert auto rule (anime %d): %w", rule.AnimeID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("upsert auto rule (anime %d): row id: %w", rule.AnimeID, err)
	}
	rule.ID = id
	return nil
}

// Delete removes a rule by ID.
func (r *AutoRuleRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM auto_download_rule WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete auto rule %d: %w", id, err)
	}
	return requireAffected(res, fmt.Errorf("delete auto rule %d: %w", id, contracts.ErrNotFound))
}

func scanAutoRule(row rowScanner) (*AutoDownloadRule, error) {
	var (
		rule               AutoDownloadRule
		enabled            int64
		srcID, video, name sql.NullString
		created, updated   string
	)
	err := row.Scan(&rule.ID, &rule.AnimeID, &enabled, &srcID, &video,
		&rule.PreferredQuality, &rule.MaxNewEpisodes, &name, &created, &updated)
	if err != nil {
		return nil, err
	}
	rule.Enabled = enabled != 0
	rule.PreferredSourceID = stringPtr(srcID)
	rule.PreferredVideoDub = stringPtr(video)
	rule.Title = stringPtr(name)
	if rule.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if rule.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &rule, nil
}
