package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/store"
)

func TestBackupSubcommand(t *testing.T) {
	t.Setenv("TC_CLI_TEST_KEY", "k")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	cfgPath := writeConfig(t, validConfig+"storage:\n  path: "+dbPath+"\n  results_dir: "+filepath.Join(dir, "results")+"\n")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := store.NewID()
	if _, _, err := st.CreateJob(context.Background(), &store.Job{ID: id, Client: "frontend", Target: "demo", MaxAttempts: 1, Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	dst := filepath.Join(dir, "copy.db")
	code, out, errOut := runCLI(t, "backup", "-config", cfgPath, dst)
	if code != 0 || !strings.Contains(out, "backup written: "+dst) {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	copyStore, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copyStore.Close() }()
	if job, err := copyStore.GetJob(context.Background(), id); err != nil || job.Client != "frontend" {
		t.Errorf("backup lacks the job: %v (err %v)", job, err)
	}
	if code, _, errOut := runCLI(t, "backup", "-config", cfgPath, dst); code != 1 || !strings.Contains(errOut, "already exists") {
		t.Errorf("overwrite must be refused: exit %d, %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "backup", "-config", cfgPath); code != 2 || !strings.Contains(errOut, "expected 1 argument") {
		t.Errorf("missing destination: exit %d, %q", code, errOut)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error(err)
	}
}
