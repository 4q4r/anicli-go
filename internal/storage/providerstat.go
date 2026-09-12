package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// ProviderStatRepo is the provider_search_stat repository: response
// frequency counters feeding the provider search priority.
type ProviderStatRepo struct {
	db *sql.DB
}

// RecordResult folds one provider response into its counters: success or
// failure count, a running latency average over all attempts, and the
// last-used stamp.
func (r *ProviderStatRepo) RecordResult(ctx context.Context, providerID string, success bool, latencyMS float64) error {
	var ok, fail int64
	if success {
		ok = 1
	} else {
		fail = 1
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO provider_search_stat (provider_id, successes, failures, avg_latency_ms, last_used_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (provider_id) DO UPDATE SET
	successes      = successes + ?,
	failures       = failures + ?,
	avg_latency_ms = (avg_latency_ms * (successes + failures) + excluded.avg_latency_ms)
	                 / (successes + failures + 1),
	last_used_at   = excluded.last_used_at`,
		providerID, ok, fail, latencyMS, fmtTime(time.Now()),
		ok, fail)
	if err != nil {
		return fmt.Errorf("record provider result %q: %w", providerID, err)
	}
	return nil
}

// TopProviders returns providers ordered by PriorityScore descending, most
// recently used first on ties; limit <= 0 returns everything.
func (r *ProviderStatRepo) TopProviders(ctx context.Context, limit int) ([]ProviderStat, error) {
	// Provider rows are few (the provider registry); the score sorts in Go
	// so its math stays unit-testable.
	rows, err := r.db.QueryContext(ctx,
		`SELECT provider_id, successes, failures, avg_latency_ms, last_used_at
		 FROM provider_search_stat ORDER BY provider_id`)
	if err != nil {
		return nil, fmt.Errorf("top providers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderStat
	for rows.Next() {
		var (
			p        ProviderStat
			lastUsed string
		)
		if err := rows.Scan(&p.ProviderID, &p.Successes, &p.Failures, &p.AvgLatencyMS, &lastUsed); err != nil {
			return nil, fmt.Errorf("scan provider stat row: %w", err)
		}
		if p.LastUsedAt, err = parseTime(lastUsed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider stats: %w", err)
	}

	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].PriorityScore(), out[j].PriorityScore()
		if si != sj {
			return si > sj
		}
		// Recency tiebreak: newer use wins.
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}
