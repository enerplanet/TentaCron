package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// previousReleaseVersion is the schema of 0.2.0-alpha; the migrations after
// it are the ones this release adds.
const previousReleaseVersion = 7

// databaseAt creates a database migrated through the real runner up to
// version upTo and returns a raw connection to populate it. No binary
// fixture: the old schema is rebuilt from the migration files, so the test
// cannot rot.
func databaseAt(t *testing.T, path string, upTo int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			t.Fatal(err)
		}
		if v > upTo {
			continue
		}
		if err := s.applyMigration(ctx, name, v); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
	return db
}

// schemaVersion is the highest migration recorded in the database.
func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// columnsOf lists a table's columns with their declared types.
func columnsOf(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		cols[name] = typ
	}
	return cols
}

// populatePreviousRelease fills every table of the 0.2.0-alpha schema with
// rows in every state a live service can hold: a queued, a claimed, a
// waiting, a completed (with a result file), a failed and a cancelled job
// with their events, a cached series, a client's claim stamp, a schedule
// that is due, and a pending callback delivery.
func populatePreviousRelease(t *testing.T, db *sql.DB) {
	t.Helper()
	const at = "'2026-09-01T00:00:00.000Z'"
	for _, stmt := range []string{
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, created_at, updated_at) VALUES
		 ('queued', 'frontend', 'demo', 'received', 0, 3, 0, '{}', '{}', ` + at + `, ` + at + `)`,
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, created_at, updated_at) VALUES
		 ('claimed', 'frontend', 'demo', 'resolving', 1, 3, 0, '{}', '{}', ` + at + `, ` + at + `)`,
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, target_job_id, next_attempt_at, poll_deadline, created_at, updated_at) VALUES
		 ('waiting', 'frontend', 'meme', 'awaiting_target', 1, 3, 0, '{"cache":"refresh"}', '{}', 'm-1', ` + at + `, '2026-09-01T01:00:00.000Z', ` + at + `, ` + at + `)`,
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, target_status, result_path, result_content_type, created_at, updated_at, completed_at) VALUES
		 ('done', 'frontend', 'meme', 'completed', 1, 3, 2, '{}', '{}', 200, '/data/results/done.zip', 'application/zip', ` + at + `, ` + at + `, ` + at + `)`,
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, error_code, error_message, callback_url, created_at, updated_at, completed_at) VALUES
		 ('broken', 'batch', 'demo', 'failed', 3, 3, 0, '{}', '{}', 'target_error', 'boom', 'https://hooks.example.org/x', ` + at + `, ` + at + `, ` + at + `)`,
		`INSERT INTO jobs (id, client, target, state, attempts, max_attempts, priority, options, payload, not_before, created_at, updated_at, completed_at) VALUES
		 ('stopped', 'batch', 'demo', 'cancelled', 0, 3, 0, '{}', '{}', '2026-09-02T00:00:00.000Z', ` + at + `, ` + at + `, ` + at + `)`,
		`INSERT INTO job_events (job_id, from_state, to_state, detail, created_at) VALUES ('queued', NULL, 'received', 'job accepted', ` + at + `)`,
		`INSERT INTO job_events (job_id, from_state, to_state, detail, created_at) VALUES ('done', 'forwarding', 'completed', 'done', ` + at + `)`,
		`INSERT INTO series_cache (param_hash, resolvent_type, response, created_at, expires_at) VALUES ('h1', 'resolvent-pv1', '{"type":"time-series"}', ` + at + `, '2027-01-01T00:00:00.000Z')`,
		`INSERT INTO client_claims (client, last_claimed_at) VALUES ('frontend', ` + at + `)`,
		`INSERT INTO schedules (id, client, target, payload, cron, timezone, priority, options, enabled, next_run_at, created_at, updated_at) VALUES
		 ('sched', 'frontend', 'demo', '{}', '@daily', 'UTC', 0, '{}', 1, ` + at + `, ` + at + `, ` + at + `)`,
		`INSERT INTO callback_deliveries (job_id, url, event, attempts, next_attempt_at, created_at, updated_at) VALUES
		 ('broken', 'https://hooks.example.org/x', 'request.failed', 0, ` + at + `, ` + at + `, ` + at + `)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", strings.Fields(stmt)[2], err)
		}
	}
}

// TestUpgradeFromPreviousReleaseWithData rehearses what production does on
// the first start of this release: a database at the 0.2.0-alpha schema,
// with rows in every table and every state, is opened by the new code. The
// rows survive, the migrations are additive so the old release could still
// open the file, the new indexes exist, and every query a live service
// runs still answers.
func TestUpgradeFromPreviousReleaseWithData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "previous.db")
	db := databaseAt(t, path, previousReleaseVersion)
	populatePreviousRelease(t, db)
	if v := schemaVersion(t, db); v != previousReleaseVersion {
		t.Fatalf("fixture at version %d, want %d", v, previousReleaseVersion)
	}
	before := map[string]map[string]string{}
	for _, table := range []string{"jobs", "job_events", "series_cache", "client_claims", "schedules", "callback_deliveries"} {
		before[table] = columnsOf(t, db, table)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a %d database: %v", previousReleaseVersion, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if v := schemaVersion(t, s.db); v <= previousReleaseVersion {
		t.Fatalf("no migration applied: still at %d", v)
	}
	// The store reports what it did, so the start-up log can say so.
	latest, err := latestMigrationVersion()
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	for v := previousReleaseVersion + 1; v <= latest; v++ {
		want = append(want, v)
	}
	if got := s.MigrationsApplied(); !slices.Equal(got, want) {
		t.Errorf("MigrationsApplied() = %v, want %v", got, want)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != latest {
		t.Errorf("SchemaVersion() = %d, %v; want %d", v, err, latest)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.MigrationsApplied(); got == nil || len(got) != 0 {
		t.Errorf("a current database must report an empty, non-nil list, got %#v", got)
	}
	_ = again.Close()
	// Additive: every old column is still there with its type.
	for table, cols := range before {
		now := columnsOf(t, s.db, table)
		for name, typ := range cols {
			if now[name] != typ {
				t.Errorf("%s.%s changed from %q to %q: a previous binary could no longer open the file", table, name, typ, now[name])
			}
		}
	}
	for _, index := range []string{"jobs_terminal_ix", "schedules_idem_uq"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing after the upgrade (err %v)", index, err)
		}
	}
	// Every row is still there, in its state.
	for id, want := range map[string]string{"queued": StateReceived, "claimed": StateResolving, "waiting": StateAwaitingTarget,
		"done": StateCompleted, "broken": StateFailed, "stopped": StateCancelled} {
		if j, err := s.GetJob(ctx, id); err != nil || j.State != want {
			t.Errorf("job %s after upgrade: %v (err %v)", id, j, err)
		}
	}
	// And every query a live service runs answers on the migrated file.
	if c, err := s.ClaimNext(ctx, noPoll); err != nil || c == nil || c.ID != "queued" {
		t.Errorf("claim after upgrade: %v %v", c, err)
	}
	if ids, paths, err := s.TerminalBefore(ctx, time.Now(), 10); err != nil || len(ids) != 3 || len(paths) != 1 {
		t.Errorf("retention scan after upgrade: ids=%v paths=%v err=%v", ids, paths, err)
	}
	if due, err := s.DueSchedules(ctx, time.Now(), 10); err != nil || len(due) != 1 || due[0].IdempotencyKey != "" {
		t.Errorf("due schedules after upgrade: %v err=%v", due, err)
	}
	if due, err := s.DueDeliveries(ctx, time.Now(), 10); err != nil || len(due) != 1 {
		t.Errorf("due deliveries after upgrade: %v err=%v", due, err)
	}
	if body, hit, err := s.GetSeries(ctx, "h1"); err != nil || !hit || len(body) == 0 {
		t.Errorf("series cache after upgrade: hit=%v err=%v", hit, err)
	}
	// Two rows are in flight now: the fixture's claimed job and the one
	// claimed just above; a cutoff in the future rescues both.
	if n, err := s.RescueStuck(ctx, cutoffAt(time.Now().Add(time.Second))); err != nil || n != 2 {
		t.Errorf("rescue after upgrade: n=%d err=%v, want 2", n, err)
	}
}

// A backup copies the schema that runs today: opening for backup applies no
// migration, the copy is at the same version, and the copy can be opened
// and migrated by the new code afterwards.
func TestOpenForBackupDoesNotMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	db := databaseAt(t, path, previousReleaseVersion)
	populatePreviousRelease(t, db)
	_ = db.Close()

	s, err := OpenForBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, s.db); v != previousReleaseVersion {
		t.Fatalf("opening for backup migrated the live database to %d", v)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := s.BackupTo(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	copyDB, err := sql.Open("sqlite", dsn(dst))
	if err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, copyDB); v != previousReleaseVersion {
		t.Fatalf("the copy is at version %d, want the live schema %d", v, previousReleaseVersion)
	}
	_ = copyDB.Close()
	migrated, err := Open(dst)
	if err != nil {
		t.Fatalf("the copy must open and migrate under the new code: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if j, err := migrated.GetJob(context.Background(), "waiting"); err != nil || j.State != StateAwaitingTarget {
		t.Fatalf("job in the migrated copy: %v (err %v)", j, err)
	}
}

// A database from a newer release is refused for backup, naming both
// versions, so an old binary never writes a copy it does not understand.
func TestOpenForBackupRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (999, '2027-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_, err = OpenForBackup(path)
	if err == nil || !strings.Contains(err.Error(), "schema version 999, newer than this binary knows") {
		t.Fatalf("err = %v, want a refusal naming the versions", err)
	}
	if _, err := OpenForBackup(filepath.Join(t.TempDir(), "empty.db")); err == nil || !strings.Contains(err.Error(), "not a tentacron database") {
		t.Fatalf("an empty file must be refused: %v", err)
	}
}
