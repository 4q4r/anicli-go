package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/an0nx/anicli-go/internal/contracts"

	// Pure-Go SQLite driver (no cgo).
	_ "modernc.org/sqlite"
)

// dsnParams are applied per connection by the modernc.org/sqlite driver.
const dsnParams = "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&" +
	"_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

// Store owns the SQLite connection and exposes one repository per
// aggregate. Construction goes through Open, which also migrates.
type Store struct {
	db *sql.DB

	// Progress is the anime_progress aggregate repository.
	Progress *ProgressRepo
	// Episodes is the anime_episode_progress aggregate repository.
	Episodes *EpisodeProgressRepo
	// Skips is the predicted_skip_time aggregate repository.
	Skips *SkipPredictionRepo
	// Details is the shikimori_anime_details_cache repository.
	Details *DetailsCacheRepo
	// Sources is the anime_source aggregate repository.
	Sources *SourceRepo
	// AutoRules is the auto_download_rule repository.
	AutoRules *AutoRuleRepo
	// ProviderStats is the provider_search_stat repository.
	ProviderStats *ProviderStatRepo
	// AuthSessions is the api_auth_session repository (API token auth).
	AuthSessions *AuthSessionRepo
}

// buildDSN converts a filesystem path (or ":memory:") into a
// modernc.org/sqlite DSN. The "file:<path>?..." relative form is mandatory:
// the "file://<path>" URI-authority form breaks modernc on write.
func buildDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:" + dsnParams
	}
	return "file:" + path + dsnParams
}

// Open opens (creating if needed) the database at path, applies pending
// migrations and returns the Store.
//
// Real paths get their parent directory created (0o750). ":memory:" opens a
// private in-memory database. The pool is pinned to exactly one connection:
// the app's own writers can never race into SQLITE_BUSY against each other,
// and an in-memory database cannot evaporate behind a reconnect.
func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("create db parent dir %q: %w", dir, err)
			}
		}
	}
	db, err := openRaw(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	st := &Store{db: db}
	if err := st.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}

	st.Progress = &ProgressRepo{db: db}
	st.Episodes = &EpisodeProgressRepo{db: db}
	st.Skips = &SkipPredictionRepo{db: db}
	st.Details = &DetailsCacheRepo{db: db}
	st.Sources = &SourceRepo{db: db}
	st.AutoRules = &AutoRuleRepo{db: db}
	st.ProviderStats = &ProviderStatRepo{db: db}
	st.AuthSessions = &AuthSessionRepo{db: db}
	return st, nil
}

// Close releases the database connection. It is safe to call repeatedly.
func (s *Store) Close() error {
	return s.db.Close()
}

// openRaw opens a single-connection pool with the package DSN semantics.
func openRaw(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// notFound maps sql.ErrNoRows onto contracts.ErrNotFound, preserving the
// rest of the error chain.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.ErrNotFound
	}
	return err
}
