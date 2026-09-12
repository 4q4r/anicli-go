package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// deref renders a nullable string for assertions.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// openTestStore opens an in-memory migrated Store and closes it on test end.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func TestBuildDSN(t *testing.T) {
	t.Parallel()

	const suffix = "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&" +
		"_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	cases := []struct {
		path string
		want string
	}{
		{"anicli.db", "file:anicli.db" + suffix},
		{"/home/u/data/anicli.db", "file:/home/u/data/anicli.db" + suffix},
		{":memory:", "file::memory:" + suffix},
	}
	for _, tc := range cases {
		if got := buildDSN(tc.path); got != tc.want {
			t.Errorf("buildDSN(%q) =\n  %q\nwant\n  %q", tc.path, got, tc.want)
		}
	}
}

func TestOpenMemoryRunsMigrations(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM _migrations`,
	).Scan(&n); err != nil {
		t.Fatalf("count _migrations: %v", err)
	}
	if n != len(migrations) {
		t.Errorf("applied migrations = %d, want %d", n, len(migrations))
	}

	// The DSN pragmas must be active; foreign_keys is the load-bearing one.
	var fk int
	if err := st.db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1 (DSN pragma not applied)", fk)
	}
}

func TestOpenCreatesParentDir(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "deeper", "anicli.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	// Second run must be a no-op, not an error (CREATE TABLE IF NOT EXISTS is
	// not used on purpose; the applied-check must skip everything).
	if err := st.migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM _migrations`,
	).Scan(&n); err != nil {
		t.Fatalf("count _migrations: %v", err)
	}
	if n != len(migrations) {
		t.Errorf("applied migrations after re-run = %d, want %d", n, len(migrations))
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO _migrations (version, name, applied_at) VALUES (99, 'future', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed bogus migration: %v", err)
	}

	err := st.migrate(context.Background())
	if err == nil {
		t.Fatal("migrate against newer schema: want error, got nil")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error = %v, want it to mention 'newer'", err)
	}
}

func TestMigrateRejectsOutOfOrderDatabase(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE _migrations SET version = 42 WHERE version = 1`,
	); err != nil {
		t.Fatalf("tamper migration row: %v", err)
	}

	err := st.migrate(context.Background())
	if err == nil {
		t.Fatal("migrate with unexpected applied version: want error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error = %v, want it to mention 'mismatch'", err)
	}
}

func TestMigrateEachMigrationTransactional(t *testing.T) {
	t.Parallel()

	// A fresh raw connection: the custom list must run against a clean DB.
	raw, err := openRaw(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open raw memory db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	list := []Migration{
		{Version: 1, Name: "good", SQL: `CREATE TABLE t1 (a INTEGER)`},
		{Version: 2, Name: "broken", SQL: `CREATE TABLE t2 (a INTEGER);
CREATE TABLE t3 (a INTEGER;
-- deliberate syntax error above: unbalanced paren`},
	}
	err = migrateDB(context.Background(), raw, list)
	if err == nil {
		t.Fatal("migrate with broken SQL: want error, got nil")
	}

	// Version 1 committed; version 2 fully rolled back (t2 and t3 absent).
	for _, table := range []string{"t2", "t3"} {
		var n int
		if err := raw.QueryRowContext(context.Background(),
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s exists after failed migration; transaction not rolled back", table)
		}
	}
	var v int64
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(*) FROM _migrations WHERE version = 1`,
	).Scan(&v); err != nil {
		t.Fatalf("count version 1: %v", err)
	}
	if v != 1 {
		t.Errorf("version 1 recorded = %d rows, want 1 (earlier migrations must commit)", v)
	}
}

func TestFreshSchemaObjects(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	wantTables := []string{
		"_migrations",
		"anime_progress",
		"anime_episode_progress",
		"predicted_skip_time",
		"shikimori_anime_details_cache",
		"anime_source",
		"auto_download_rule",
		"api_auth_session",
		"provider_search_stat",
	}
	wantIndexes := []string{
		"uix_source_anime",
		"ix_anime_progress_title",
		"ix_anime_progress_shikimori_id",
		"ix_anime_progress_shikimori_status",
		"ix_anime_progress_updated_at",
		"uix_anime_episode_progress_anime_id",
		"ix_anime_episode_progress_anime_id",
		"ix_anime_episode_progress_episode",
		"uix_skip",
		"ix_predicted_skip_time_shikimori_id",
		"ix_shikimori_anime_details_cache_anime_id",
		"uix_shikimori_anime_details_cache_anime_id",
		"uix_anime_source",
		"ix_anime_source_anime_progress_id",
		"uix_auto_download_rule_anime_id",
		"ix_auto_download_rule_anime_id",
		"uix_api_auth_session_id",
		"ix_api_auth_session_session_id",
		"ix_api_auth_session_user_login",
		"ix_api_auth_session_refresh_token_hash",
		"ix_api_auth_session_expires_at",
	}

	got := map[string]bool{}
	rows, err := st.db.QueryContext(context.Background(),
		`SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`,
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		got[name] = true
		_ = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}

	for _, table := range wantTables {
		if !got[table] {
			t.Errorf("table %q missing from fresh schema", table)
		}
	}
	for _, idx := range wantIndexes {
		if !got[idx] {
			t.Errorf("index %q missing from fresh schema", idx)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO anime_episode_progress (anime_id, episode, updated_at)
		 VALUES (999, '1', '2026-01-01T00:00:00Z')`,
	)
	if err == nil {
		t.Fatal("insert episode progress for missing anime: want FK error, got nil")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("error = %v, want FOREIGN KEY constraint failure", err)
	}
}

func TestConcurrentUpsertsSingleConn(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)

	const workers = 8
	const perWorker = 25

	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				p := &AnimeProgress{
					Title:          "concurrent",
					SourceID:       "src",
					SourceURL:      "w" + strconv.Itoa(w) + "-e" + strconv.Itoa(i),
					CurrentEpisode: "1",
				}
				if err := st.Progress.Upsert(context.Background(), p); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent upsert failed (SQLITE_BUSY class?): %v", err)
	}

	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM anime_progress`,
	).Scan(&n); err != nil {
		t.Fatalf("count anime_progress: %v", err)
	}
	if n != workers*perWorker {
		t.Errorf("rows after concurrent upserts = %d, want %d", n, workers*perWorker)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := st.Close(); err != nil && !errors.Is(err, context.Canceled) {
		// sql.DB.Close is documented safe to call repeatedly.
		t.Errorf("second close: %v", err)
	}
}
