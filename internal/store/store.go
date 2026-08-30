// Package store owns all SQLite persistence: the job queue with its audit
// trail and the resolved-series cache.
package store

import (
	"context"
	"database/sql"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	// _txlock=immediate makes every transaction take the write lock at BEGIN.
	// All transactions here write, and the default deferred mode upgrades a
	// read snapshot to a write lock mid-transaction — which fails immediately
	// with SQLITE_BUSY (no busy_timeout retry) when another writer committed
	// in between, stranding jobs mid-transition under concurrent workers.
	dsn := "file:" + path + "?" + url.Values{
		"_txlock": []string{"immediate"},
		"_pragma": []string{
			"busy_timeout(5000)",
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"foreign_keys(1)",
		},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite permits one writer at a time; a small pool lets reads run
	// concurrently under WAL while busy_timeout absorbs write contention.
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }
