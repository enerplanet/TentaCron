package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job states.
const (
	StateReceived       = "received"
	StateResolving      = "resolving"
	StateForwarding     = "forwarding"
	StateAwaitingTarget = "awaiting_target"
	StateCompleted      = "completed"
	StateFailed         = "failed"
)

// ErrNotFound is returned when a job id does not exist.
var ErrNotFound = errors.New("job not found")

// Job is one orchestration request and its processing state.
type Job struct {
	ID                string
	IdempotencyKey    string
	Target            string
	State             string
	Attempts          int
	MaxAttempts       int
	NextAttemptAt     *time.Time
	Payload           []byte
	ResolvedPayload   []byte
	TargetJobID       string
	PollDeadline      *time.Time
	TargetStatus      *int
	TargetResponse    []byte
	ResultPath        string
	ResultContentType string
	ErrorCode         string
	ErrorMessage      string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       *time.Time
}

// NewID returns a 32-character random hex id.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

const jobColumns = `id, idempotency_key, target, state, attempts, max_attempts,
	next_attempt_at, payload, resolved_payload, target_job_id, poll_deadline,
	target_status, target_response, result_path, result_content_type,
	error_code, error_message, created_at, updated_at, completed_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (*Job, error) {
	var (
		j                                      Job
		idem, nextAt, targetJobID, pollDL      sql.NullString
		resultPath, resultCT, errCode, errMsg  sql.NullString
		createdAt, updatedAt, completedAt      sql.NullString
		targetStatus                           sql.NullInt64
	)
	err := r.Scan(&j.ID, &idem, &j.Target, &j.State, &j.Attempts, &j.MaxAttempts,
		&nextAt, &j.Payload, &j.ResolvedPayload, &targetJobID, &pollDL,
		&targetStatus, &j.TargetResponse, &resultPath, &resultCT,
		&errCode, &errMsg, &createdAt, &updatedAt, &completedAt)
	if err != nil {
		return nil, err
	}
	j.IdempotencyKey = idem.String
	j.TargetJobID = targetJobID.String
	j.ResultPath = resultPath.String
	j.ResultContentType = resultCT.String
	j.ErrorCode = errCode.String
	j.ErrorMessage = errMsg.String
	if targetStatus.Valid {
		v := int(targetStatus.Int64)
		j.TargetStatus = &v
	}
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{nextAt, &j.NextAttemptAt}, {pollDL, &j.PollDeadline}, {completedAt, &j.CompletedAt}} {
		if p.src.Valid {
			t, err := parseTS(p.src.String)
			if err != nil {
				return nil, fmt.Errorf("job %s: bad timestamp %q: %w", j.ID, p.src.String, err)
			}
			*p.dst = &t
		}
	}
	if j.CreatedAt, err = parseTS(createdAt.String); err != nil {
		return nil, fmt.Errorf("job %s: bad created_at: %w", j.ID, err)
	}
	if j.UpdatedAt, err = parseTS(updatedAt.String); err != nil {
		return nil, fmt.Errorf("job %s: bad updated_at: %w", j.ID, err)
	}
	return &j, nil
}

// CreateJob inserts a new job in state "received" and records the accept
// event. If the job carries an idempotency key that already exists, the
// stored job is returned instead with created == false.
func (s *Store) CreateJob(ctx context.Context, j *Job) (created bool, existing *Job, err error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var idem any
	if j.IdempotencyKey != "" {
		idem = j.IdempotencyKey
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs
		(id, idempotency_key, target, state, attempts, max_attempts, payload, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		j.ID, idem, j.Target, StateReceived, j.MaxAttempts, j.Payload, ts(now), ts(now))
	if err != nil {
		// SQLite's stable message for the partial unique index on idempotency_key.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: jobs.idempotency_key") {
			_ = tx.Rollback()
			ex, gerr := s.GetJobByIdempotencyKey(ctx, j.IdempotencyKey)
			if gerr != nil {
				return false, nil, gerr
			}
			return false, ex, nil
		}
		return false, nil, fmt.Errorf("insert job: %w", err)
	}
	if err = appendEventTx(ctx, tx, j.ID, "", StateReceived, "job accepted", now); err != nil {
		return false, nil, err
	}
	if err = tx.Commit(); err != nil {
		return false, nil, err
	}
	j.State = StateReceived
	j.CreatedAt = now
	j.UpdatedAt = now
	return true, j, nil
}

// GetJob fetches one job by id.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// GetJobByIdempotencyKey fetches the job stored under an idempotency key.
func (s *Store) GetJobByIdempotencyKey(ctx context.Context, key string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = ?`, key)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListJobs returns jobs newest-first, optionally filtered by state.
func (s *Store) ListJobs(ctx context.Context, state string, limit int) ([]*Job, error) {
	q := `SELECT ` + jobColumns + ` FROM jobs`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ClaimNext atomically claims the next eligible job. Jobs in "received" move
// to "resolving" (attempts incremented). Jobs in "awaiting_target" whose poll
// time is due are claimed by pushing next_attempt_at forward (CAS), so no
// other worker picks the same poll tick; pollNext supplies the per-target
// poll interval. Returns nil when no work is eligible.
func (s *Store) ClaimNext(ctx context.Context, pollNext func(target string) time.Duration) (*Job, error) {
	now := time.Now()
	nowS := ts(now)
	rows, err := s.db.QueryContext(ctx, `SELECT id, state, target, next_attempt_at FROM jobs
		WHERE (state = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
		   OR (state = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?)
		ORDER BY created_at LIMIT 8`,
		StateReceived, nowS, StateAwaitingTarget, nowS)
	if err != nil {
		return nil, err
	}
	type cand struct{ id, state, target, nextAt string }
	var cands []cand
	for rows.Next() {
		var c cand
		var nextAt sql.NullString
		if err := rows.Scan(&c.id, &c.state, &c.target, &nextAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.nextAt = nextAt.String
		cands = append(cands, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, c := range cands {
		claimed, err := s.claimOne(ctx, c.id, c.state, c.target, c.nextAt, now, pollNext)
		if err != nil {
			return nil, err
		}
		if claimed {
			return s.GetJob(ctx, c.id)
		}
	}
	return nil, nil
}

func (s *Store) claimOne(ctx context.Context, id, state, target, nextAt string, now time.Time, pollNext func(string) time.Duration) (bool, error) {
	switch state {
	case StateReceived:
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE jobs
			SET state = ?, attempts = attempts + 1, next_attempt_at = NULL, updated_at = ?
			WHERE id = ? AND state = ?`,
			StateResolving, ts(now), id, StateReceived)
		if err != nil {
			_ = tx.Rollback()
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			_ = tx.Rollback()
			return false, nil
		}
		if err := appendEventTx(ctx, tx, id, StateReceived, StateResolving, "claimed by worker", now); err != nil {
			_ = tx.Rollback()
			return false, err
		}
		return true, tx.Commit()
	case StateAwaitingTarget:
		next := now.Add(pollNext(target))
		res, err := s.db.ExecContext(ctx, `UPDATE jobs SET next_attempt_at = ?, updated_at = ?
			WHERE id = ? AND state = ? AND next_attempt_at = ?`,
			ts(next), ts(now), id, StateAwaitingTarget, nextAt)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n == 1, nil
	}
	return false, nil
}

// SetResolved stores the resolved payload and moves the job to "forwarding".
func (s *Store) SetResolved(ctx context.Context, id string, resolvedPayload []byte, detail string) error {
	return s.transition(ctx, id, StateResolving, StateForwarding, detail, func(q *updateBuilder) {
		q.set("resolved_payload = ?", resolvedPayload)
	})
}

// MarkAwaitingTarget records the target's accept response and schedules polling.
func (s *Store) MarkAwaitingTarget(ctx context.Context, id, targetJobID string, targetStatus int, acceptBody []byte, firstPollAt, pollDeadline time.Time) error {
	detail := fmt.Sprintf("target accepted (status %d), polling job %s", targetStatus, targetJobID)
	return s.transition(ctx, id, StateForwarding, StateAwaitingTarget, detail, func(q *updateBuilder) {
		q.set("target_job_id = ?", targetJobID)
		q.set("target_status = ?", targetStatus)
		q.set("target_response = ?", acceptBody)
		q.set("next_attempt_at = ?", ts(firstPollAt))
		q.set("poll_deadline = ?", ts(pollDeadline))
	})
}

// MarkCompleted finishes a job successfully.
func (s *Store) MarkCompleted(ctx context.Context, id string, targetStatus int, response []byte, resultPath, resultContentType, detail string) error {
	now := time.Now()
	return s.transition(ctx, id, "", StateCompleted, detail, func(q *updateBuilder) {
		q.set("target_status = ?", targetStatus)
		if response != nil {
			q.set("target_response = ?", response)
		}
		if resultPath != "" {
			q.set("result_path = ?", resultPath)
			q.set("result_content_type = ?", resultContentType)
		}
		q.set("next_attempt_at = NULL")
		q.set("completed_at = ?", ts(now))
	})
}

// MarkFailed finishes a job with a permanent error.
func (s *Store) MarkFailed(ctx context.Context, id, code, message string) error {
	now := time.Now()
	detail := code + ": " + message
	return s.transition(ctx, id, "", StateFailed, detail, func(q *updateBuilder) {
		q.set("error_code = ?", code)
		q.set("error_message = ?", message)
		q.set("next_attempt_at = NULL")
		q.set("completed_at = ?", ts(now))
	})
}

// Requeue schedules a retry after a transient error.
func (s *Store) Requeue(ctx context.Context, id string, nextAttemptAt time.Time, detail string) error {
	return s.transition(ctx, id, "", StateReceived, detail, func(q *updateBuilder) {
		q.set("next_attempt_at = ?", ts(nextAttemptAt))
	})
}

// RecoverInFlight requeues jobs interrupted by a restart. Jobs stuck in
// resolving/forwarding restart from the original payload (cheap thanks to the
// series cache); awaiting_target jobs keep their poll schedule.
func (s *Store) RecoverInFlight(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE state IN (?, ?)`,
		StateResolving, StateForwarding)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.transition(ctx, id, "", StateReceived, "recovered after restart", func(q *updateBuilder) {
			q.set("next_attempt_at = NULL")
		}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// PruneTerminal deletes completed/failed jobs finished before cutoff and
// returns the result file paths that belonged to them.
func (s *Store) PruneTerminal(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT result_path FROM jobs
		WHERE state IN (?, ?) AND completed_at < ? AND result_path IS NOT NULL AND result_path != ''`,
		StateCompleted, StateFailed, ts(cutoff))
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM jobs WHERE state IN (?, ?) AND completed_at < ?`,
		StateCompleted, StateFailed, ts(cutoff))
	return paths, err
}

// updateBuilder accumulates SET clauses for a transition.
type updateBuilder struct {
	clauses []string
	args    []any
}

func (u *updateBuilder) set(clause string, args ...any) {
	u.clauses = append(u.clauses, clause)
	u.args = append(u.args, args...)
}

// transition applies a state change plus extra column updates and appends the
// audit event, all in one transaction. fromState "" accepts any current state.
func (s *Store) transition(ctx context.Context, id, fromState, toState, detail string, build func(*updateBuilder)) (err error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var current string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return err
	}
	if fromState != "" && current != fromState {
		return fmt.Errorf("job %s: cannot transition %s -> %s (state is %s)", id, fromState, toState, current)
	}

	b := &updateBuilder{}
	b.set("state = ?", toState)
	b.set("updated_at = ?", ts(now))
	if build != nil {
		build(b)
	}
	args := make([]any, 0, len(b.args)+1)
	args = append(args, b.args...)
	args = append(args, id)
	// The clause strings are compile-time constants; all values are bound parameters.
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET `+strings.Join(b.clauses, ", ")+` WHERE id = ?`, args...); err != nil { //nolint:gosec // G202: constant clauses, bound values

		return fmt.Errorf("update job %s: %w", id, err)
	}
	if err = appendEventTx(ctx, tx, id, current, toState, detail, now); err != nil {
		return err
	}
	return tx.Commit()
}
