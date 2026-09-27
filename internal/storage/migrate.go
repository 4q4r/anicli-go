package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Migration is one versioned schema step. SQL may contain multiple
// statements; each Migration applies inside its own transaction.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

// migrations is the ordered, embedded migration chain. Version 1 is the
// full schema: fresh installs create every table in one step. Existing
// Python (anicli-py) databases are intentionally NOT migrated — the Go
// build starts fresh; data import is a later, separate feature.
var migrations = []Migration{
	{Version: 1, Name: "initial schema", SQL: schemaV1},
	{Version: 2, Name: "anime mal id map", SQL: schemaV2},
}

// migrate applies pending migrations to the store's database.
func (s *Store) migrate(ctx context.Context) error {
	return migrateDB(ctx, s.db, migrations)
}

// migrateDB applies every migration in list that the database has not
// recorded yet, one transaction per migration (BeginTx, deferred Rollback,
// Commit). Applied versions must form an exact prefix of list; anything
// else is an out-of-order or newer-than-binary database and fails loud.
func migrateDB(ctx context.Context, db *sql.DB, list []Migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create _migrations: %w", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT version FROM _migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	var applied []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate applied migrations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close applied migrations: %w", err)
	}

	if len(applied) > len(list) {
		return fmt.Errorf(
			"database schema is newer than this binary: %d migrations applied, binary knows %d",
			len(applied), len(list))
	}
	for i, v := range applied {
		if want := list[i].Version; v != want {
			return fmt.Errorf(
				"applied migration mismatch: database has version %d at position %d, binary expects %d",
				v, i, want)
		}
	}

	for _, m := range list[len(applied):] {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration inside its own transaction and records
// it in _migrations within that same transaction: either the schema change
// and its bookkeeping land together, or neither does.
func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec migration %d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, fmtTime(time.Now()),
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.Version, err)
	}
	return nil
}
