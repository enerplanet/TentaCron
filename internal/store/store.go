// Package store owns all SQLite persistence: the job queue with its audit
// trail and the resolved-series cache.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

// timeLayout is fixed-width so stored UTC timestamps compare correctly as
// strings (RFC3339Nano trims zeros and would break lexicographic ordering).
const timeLayout = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTS(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. The parent directory is created if missing.
func Open(path string) (*Store, error) {
	s, err := open(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

// OpenForBackup connects without migrating, for tentacron backup: a backup
// must copy the database as it is, never after applying migrations first,
// or the advice "back up before upgrading" would be defeated by the very
// command that follows it. A database recorded at a schema version newer
// than this binary knows is refused, naming both versions.
func OpenForBackup(path string) (*Store, error) {
	s, err := open(path)
	if err != nil {
		return nil, err
	}
	recorded, err := s.recordedVersion(context.Background())
	if err != nil {
		_ = s.db.Close()
		return nil, err
	}
	known, err := latestMigrationVersion()
	if err != nil {
		_ = s.db.Close()
		return nil, err
	}
	if recorded > known {
		_ = s.db.Close()
		return nil, fmt.Errorf("database %s is at schema version %d, newer than this binary knows (%d): back up with the binary that runs it, or upgrade this one", path, recorded, known)
	}
	return s, nil
}

// recordedVersion is the highest migration the database has applied; a
// database without the migrations table was never started by tentacron.
func (s *Store) recordedVersion(ctx context.Context) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("inspect schema: %w", err)
	}
	if exists == 0 {
		return 0, errors.New("not a tentacron database: no schema_migrations table")
	}
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// dsn is the connection string for path. _txlock=immediate makes every
// transaction take the write lock at BEGIN: all transactions here write,
// and the default deferred mode upgrades a read snapshot to a write lock
// mid-transaction — which fails immediately with SQLITE_BUSY (no
// busy_timeout retry) when another writer committed in between, stranding
// jobs mid-transition under concurrent workers.
func dsn(path string) string {
	return "file:" + path + "?" + url.Values{
		"_txlock": []string{"immediate"},
		"_pragma": []string{
			"busy_timeout(5000)",
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"foreign_keys(1)",
			// Takes effect on databases created by tentacron (it must be
			// set before the first table exists); with it, the sweeper's
			// incremental vacuum hands freed pages back to the filesystem.
			// A database created before this setting needs one manual
			// VACUUM to adopt it — see docs/operations.md.
			"auto_vacuum(INCREMENTAL)",
		},
	}.Encode()
}

// open connects to the database without migrating it.
func open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite permits one writer at a time; a small pool lets reads run
	// concurrently under WAL while busy_timeout absorbs write contention.
	db.SetMaxOpenConns(4)
	return &Store{db: db}, nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }
