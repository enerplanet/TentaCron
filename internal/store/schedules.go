package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Schedule is a recurring submission: the same target and payload, run at
// every time its cron expression names.
type Schedule struct {
	ID       string
	Client   string
	Target   string
	Payload  []byte
	Cron     string
	Timezone string
	Priority int
	Options  JobOptions
	// NextRunAt is the due time of the next run; the scheduler advances it
	// with a compare-and-set, so two schedulers cannot both run one due time.
	NextRunAt *time.Time
	// LastRunAt and LastJobID describe the most recent materialised run.
	LastRunAt *time.Time
	LastJobID string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunKeyPrefix is the idempotency-key prefix of a schedule's runs; the due
// time follows it, so the key is unique per due time and identical across
// restarts and duplicate ticks.
func RunKeyPrefix(scheduleID string) string { return "schedule:" + scheduleID + ":" }

// RunKey is the idempotency key of the run due at t.
func RunKey(scheduleID string, t time.Time) string {
	return RunKeyPrefix(scheduleID) + t.UTC().Format(time.RFC3339)
}

const scheduleColumns = `id, client, target, payload, cron, timezone, priority, options, next_run_at, last_run_at, last_job_id, created_at, updated_at`

// CreateSchedule stores a schedule; NextRunAt must be set by the caller.
func (s *Store) CreateSchedule(ctx context.Context, sc *Schedule) error {
	now := time.Now()
	var next any
	if sc.NextRunAt != nil {
		next = ts(*sc.NextRunAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules (`+scheduleColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)`,
		sc.ID, sc.Client, sc.Target, sc.Payload, sc.Cron, sc.Timezone, sc.Priority, sc.Options.encode(), next, ts(now), ts(now))
	if err != nil {
		return fmt.Errorf("insert schedule: %w", err)
	}
	sc.CreatedAt, sc.UpdatedAt = now, now
	return nil
}

// GetSchedule returns one schedule or ErrNotFound.
func (s *Store) GetSchedule(ctx context.Context, id string) (*Schedule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id)
	sc, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sc, err
}

// ListSchedules returns schedules oldest first; client "" lists every
// client's.
func (s *Store) ListSchedules(ctx context.Context, client string, limit int) ([]*Schedule, error) {
	q := `SELECT ` + scheduleColumns + ` FROM schedules`
	var args []any
	if client != "" {
		q += ` WHERE client = ?`
		args = append(args, client)
	}
	q += ` ORDER BY created_at, rowid LIMIT ?`
	args = append(args, limit)
	return s.querySchedules(ctx, q, args...)
}

// DeleteSchedule removes a schedule; its runs stay as ordinary jobs.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueSchedules lists enabled schedules whose next run is due at now, the
// most overdue first.
func (s *Store) DueSchedules(ctx context.Context, now time.Time, limit int) ([]*Schedule, error) {
	return s.querySchedules(ctx, `SELECT `+scheduleColumns+` FROM schedules
		WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at, rowid LIMIT ?`, ts(now), limit)
}

// AdvanceSchedule records the run materialised for the due time and moves
// the schedule to its next one. It is a compare-and-set on next_run_at:
// false means another scheduler advanced it first.
func (s *Store) AdvanceSchedule(ctx context.Context, id string, due time.Time, jobID string, next time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE schedules
		SET last_run_at = ?, last_job_id = ?, next_run_at = ?, updated_at = ?
		WHERE id = ? AND next_run_at = ?`,
		ts(due), jobID, ts(next), ts(time.Now()), id, ts(due))
	if err != nil {
		return false, fmt.Errorf("advance schedule: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) querySchedules(ctx context.Context, q string, args ...any) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query schedules: %w", err)
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func scanSchedule(r rowScanner) (*Schedule, error) {
	var sc Schedule
	var options string
	var next, last, lastJob, created, updated sql.NullString
	if err := r.Scan(&sc.ID, &sc.Client, &sc.Target, &sc.Payload, &sc.Cron, &sc.Timezone, &sc.Priority, &options,
		&next, &last, &lastJob, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if sc.Options, err = decodeOptions(options); err != nil {
		return nil, fmt.Errorf("schedule %s: %w", sc.ID, err)
	}
	sc.LastJobID = lastJob.String
	for _, f := range []struct {
		raw  sql.NullString
		dst  **time.Time
		name string
	}{{next, &sc.NextRunAt, "next_run_at"}, {last, &sc.LastRunAt, "last_run_at"}} {
		if !f.raw.Valid {
			continue
		}
		t, err := parseTS(f.raw.String)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %s: %w", sc.ID, f.name, err)
		}
		*f.dst = &t
	}
	if sc.CreatedAt, err = parseTS(created.String); err != nil {
		return nil, fmt.Errorf("schedule %s: created_at: %w", sc.ID, err)
	}
	if sc.UpdatedAt, err = parseTS(updated.String); err != nil {
		return nil, fmt.Errorf("schedule %s: updated_at: %w", sc.ID, err)
	}
	return &sc, nil
}

// escapeLike makes a literal safe under LIKE ... ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
