package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func timeNow() time.Time { return time.Now() }

// Databases tentacron creates use incremental auto-vacuum, so the sweeper's
// Vacuum hands freed pages back; a backup is a consistent copy that opens
// as a store of its own and is never overwritten.
func TestVacuumAndBackup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if mode, err := s.AutoVacuumMode(ctx); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d (err %v), want 2 (incremental)", mode, err)
	}
	ids := insertTerminalRows(t, s, 2000, timeNow().Add(-time.Hour))
	if err := s.DeleteJobs(ctx, ids); err != nil {
		t.Fatal(err)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	keep := newJob(t, "meme")
	mustCreate(t, s, keep)

	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := s.BackupTo(ctx, dst); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if err := s.BackupTo(ctx, dst); err == nil {
		t.Error("BackupTo must refuse to overwrite an existing file")
	}
	if _, err := os.Stat(dst + "-wal"); err == nil {
		t.Error("VACUUM INTO must produce a single self-contained file at rest")
	}
	copyStore, err := Open(dst)
	if err != nil {
		t.Fatalf("the backup must open as a database: %v", err)
	}
	defer func() { _ = copyStore.Close() }()
	if got, err := copyStore.GetJob(ctx, keep.ID); err != nil || got.Target != "meme" {
		t.Errorf("backup lacks the live row: %v (err %v)", got, err)
	}
	if counts, _ := copyStore.CountByState(ctx); counts[StateReceived] != 1 || counts[StateCompleted] != 0 {
		t.Errorf("backup counts = %v, want only the one kept job", counts)
	}
}
