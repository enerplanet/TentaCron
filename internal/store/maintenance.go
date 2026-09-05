package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Vacuum reclaims freed pages incrementally and checkpoints the WAL. With
// auto_vacuum set to INCREMENTAL (the mode tentacron creates databases
// with) the file shrinks after pruning instead of only reusing pages; on a
// database without it the call is a harmless no-op. Cheap enough for every
// housekeeping sweep.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum(256)`); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}

// BackupTo writes a consistent, compacted copy of the database to path
// with VACUUM INTO, which is safe while the service runs and never blocks
// writers for long. It refuses to overwrite an existing file.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup target %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup target %s: %w", path, err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backup to %s: %w", path, err)
	}
	return nil
}

// AutoVacuumMode reports the database's auto_vacuum setting: 0 none,
// 1 full, 2 incremental.
func (s *Store) AutoVacuumMode(ctx context.Context) (int, error) {
	var mode int
	err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode)
	return mode, err
}
