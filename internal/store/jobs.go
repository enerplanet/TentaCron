package store

import (
	"bytes"
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

// ErrNotFound is returned when no matching job exists.
var ErrNotFound = errors.New("job not found")

// ErrTerminalState is returned when a transition would move a job out of
// completed/failed. Terminal states are final: a stale worker (e.g. an
// overlapping poll tick) must never overwrite a result the client may have
// already observed.
var ErrTerminalState = errors.New("job is in a terminal state")

// ErrIdempotencyConflict is returned when an idempotency key is reused with a
// different target or payload than the stored request.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// Job is one orchestration request and its processing state. Field order
// mirrors jobColumns; keep the two in sync.
type Job struct {
	// Seq is the row's insertion order (SQLite rowid): the tiebreaker for
	// jobs created within one millisecond and the second half of a list
	// cursor. Never exposed as an identifier.
	Seq int64

	// Identity and queue state.
	ID             string
	Client         string // authenticated client name; scopes the idempotency key
	IdempotencyKey string
	Target         string
	State          string
	Attempts       int
	MaxAttempts    int
	NextAttemptAt  *time.Time
	// Priority orders claims: higher first, -10..10, default 0.
	Priority int

	// Payload as accepted, and after resolvent substitution.
	Payload         []byte
	ResolvedPayload []byte

	// Async-target polling (poll-mode targets only).
	TargetJobID  string
	PollDeadline *time.Time

	// Target response (accept response while polling, final response on
	// completion) and terminal outcome: result on completion, error on
	// permanent failure.
	TargetStatus      *int
	TargetResponse    []byte
	ResultPath        string
	ResultContentType string
	ErrorCode         string
	ErrorMessage      string

	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// NewID returns a 32-character random hex id.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

const jobColumns = `rowid, id, client, idempotency_key, target, state, attempts, max_attempts, priority,
	next_attempt_at, payload, resolved_payload, target_job_id, poll_deadline,
	target_status, target_response, result_path, result_content_type,
	error_code, error_message, created_at, updated_at, completed_at`

type rowScanner interface{ Scan(dest ...any) error }

// jobRow is the scan target for jobColumns: nullable columns land in
// sql.Null* fields and are folded into the job by toJob.
type jobRow struct {
	job                                   Job
	idem, nextAt, targetJobID, pollDL     sql.NullString
	resultPath, resultCT, errCode, errMsg sql.NullString
	createdAt, updatedAt, completedAt     sql.NullString
	targetStatus                          sql.NullInt64
}

func scanJob(r rowScanner) (*Job, error) {
	var row jobRow
	j := &row.job
	err := r.Scan(&j.Seq, &j.ID, &j.Client, &row.idem, &j.Target, &j.State, &j.Attempts, &j.MaxAttempts, &j.Priority,
		&row.nextAt, &j.Payload, &j.ResolvedPayload, &row.targetJobID, &row.pollDL,
		&row.targetStatus, &j.TargetResponse, &row.resultPath, &row.resultCT,
		&row.errCode, &row.errMsg, &row.createdAt, &row.updatedAt, &row.completedAt)
	if err != nil {
		return nil, err
	}
	return row.toJob()
}

// toJob folds the nullable columns into the job and parses its timestamps.
func (row *jobRow) toJob() (*Job, error) {
	j := &row.job
	j.IdempotencyKey = row.idem.String
	j.TargetJobID = row.targetJobID.String
	j.ResultPath = row.resultPath.String
	j.ResultContentType = row.resultCT.String
	j.ErrorCode = row.errCode.String
	j.ErrorMessage = row.errMsg.String
	if row.targetStatus.Valid {
		v := int(row.targetStatus.Int64)
		j.TargetStatus = &v
	}
	if err := row.parseTimes(j); err != nil {
		return nil, err
	}
	return j, nil
}

func (row *jobRow) parseTimes(j *Job) error {
	var err error
	if j.CreatedAt, err = parseTS(row.createdAt.String); err != nil {
		return fmt.Errorf("job %s: bad created_at: %w", j.ID, err)
	}
	if j.UpdatedAt, err = parseTS(row.updatedAt.String); err != nil {
		return fmt.Errorf("job %s: bad updated_at: %w", j.ID, err)
	}
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{row.nextAt, &j.NextAttemptAt}, {row.pollDL, &j.PollDeadline}, {row.completedAt, &j.CompletedAt}} {
		if !p.src.Valid {
			continue
		}
		t, err := parseTS(p.src.String)
		if err != nil {
			return fmt.Errorf("job %s: bad timestamp %q: %w", j.ID, p.src.String, err)
		}
		*p.dst = &t
	}
	return nil
}

// CreateJob inserts a new job in state "received" and records the accept
// event. Idempotency keys are scoped per client: replaying a key with an
// identical target and payload returns the stored job with created == false;
// reusing it with a different request returns ErrIdempotencyConflict. On
// creation the returned job is j itself with state and timestamps filled in.
func (s *Store) CreateJob(ctx context.Context, j *Job) (created bool, stored *Job, err error) {
	// The unique-violation fallback reads outside the failed transaction, so
	// the stored job can vanish (retention prune) in between; retry the
	// insert rather than failing a valid request.
	for attempt := 0; attempt < 3; attempt++ {
		created, stored, err = s.createJobOnce(ctx, j)
		if !errors.Is(err, ErrNotFound) {
			return created, stored, err
		}
	}
	return false, nil, fmt.Errorf("create job %s: idempotency race did not settle", j.ID)
}

func (s *Store) createJobOnce(ctx context.Context, j *Job) (created bool, stored *Job, err error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	if err := insertJob(ctx, tx, j, now); err != nil {
		_ = tx.Rollback()
		if isIdempotencyViolation(err) {
			return s.replayIdempotent(ctx, j)
		}
		return false, nil, fmt.Errorf("insert job: %w", err)
	}
	if err := appendEventTx(ctx, tx, j.ID, "", StateReceived, "job accepted", now); err != nil {
		_ = tx.Rollback()
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	j.State, j.CreatedAt, j.UpdatedAt = StateReceived, now, now
	return true, j, nil
}

func insertJob(ctx context.Context, tx *sql.Tx, j *Job, now time.Time) error {
	var idem any
	if j.IdempotencyKey != "" {
		idem = j.IdempotencyKey
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, client, idempotency_key, target, state, attempts, max_attempts, priority, payload, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		j.ID, j.Client, idem, j.Target, StateReceived, j.MaxAttempts, j.Priority, j.Payload, ts(now), ts(now))
	return err
}

// isIdempotencyViolation matches SQLite's stable message for the partial
// unique index on (client, idempotency_key).
func isIdempotencyViolation(err error) bool {
	return strings.Contains(err.Error(), "jobs.idempotency_key")
}

// replayIdempotent settles a key collision: the stored job is returned when
// the request is identical, ErrIdempotencyConflict otherwise.
func (s *Store) replayIdempotent(ctx context.Context, j *Job) (bool, *Job, error) {
	existing, err := s.GetJobByIdempotency(ctx, j.Client, j.IdempotencyKey)
	if err != nil {
		return false, nil, err
	}
	if existing.Target != j.Target || !bytes.Equal(existing.Payload, j.Payload) {
		return false, nil, fmt.Errorf("key %q: %w", j.IdempotencyKey, ErrIdempotencyConflict)
	}
	return false, existing, nil
}

// GetJob fetches one job by id. It returns ErrNotFound if no such job exists.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// GetJobByIdempotency fetches the job a client stored under an idempotency
// key. It returns ErrNotFound if none exists.
func (s *Store) GetJobByIdempotency(ctx context.Context, client, key string) (*Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE client = ? AND idempotency_key = ?`, client, key)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Cursor addresses a position in the newest-first listing: the created_at
// and insertion sequence of the last item a client has seen. Items strictly
// older than it form the next page, so pagination never skips or repeats a
// job even when many were created in the same millisecond.
type Cursor struct {
	CreatedAt time.Time
	Seq       int64
}

// ListFilter narrows ListJobs. Empty strings and nil pointers mean no filter
// on that column; Limit is required. Since is inclusive, Until exclusive.
type ListFilter struct {
	State  string
	Client string
	Target string
	Since  *time.Time
	Until  *time.Time
	Before *Cursor
	Limit  int
}

// clauses renders the filter as SQL conditions with their bound values.
func (f ListFilter) clauses() (where []string, args []any) {
	if f.State != "" {
		where, args = append(where, "state = ?"), append(args, f.State)
	}
	if f.Client != "" {
		where, args = append(where, "client = ?"), append(args, f.Client)
	}
	if f.Target != "" {
		where, args = append(where, "target = ?"), append(args, f.Target)
	}
	if f.Since != nil {
		where, args = append(where, "created_at >= ?"), append(args, ts(*f.Since))
	}
	if f.Until != nil {
		where, args = append(where, "created_at < ?"), append(args, ts(*f.Until))
	}
	if f.Before != nil {
		where = append(where, "(created_at < ? OR (created_at = ? AND rowid < ?))")
		args = append(args, ts(f.Before.CreatedAt), ts(f.Before.CreatedAt), f.Before.Seq)
	}
	return where, args
}

// ListJobs returns jobs newest-first under the filter; rowid breaks
// created_at ties by insertion order (ids are random hex, so ordering by id
// would shuffle same-millisecond jobs run to run).
func (s *Store) ListJobs(ctx context.Context, f ListFilter) ([]*Job, error) {
	q := `SELECT ` + jobColumns + ` FROM jobs`
	where, args := f.clauses()
	if len(where) > 0 {
		// The clauses are compile-time constants; every value is bound.
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // G202
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, f.Limit)
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

// CountByState returns how many jobs sit in each state; states without jobs
// are absent. It backs the queue-depth gauge scraped by Prometheus.
func (s *Store) CountByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

// ClaimPolicy tells ClaimNext how to schedule: the poll cadence per target
// and the per-client ceiling on jobs in flight (0 means unlimited).
type ClaimPolicy struct {
	PollInterval  func(target string) time.Duration
	MaxConcurrent func(client string) int
}

func (p ClaimPolicy) pollInterval(target string) time.Duration {
	if p.PollInterval == nil {
		return time.Minute
	}
	return p.PollInterval(target)
}

func (p ClaimPolicy) maxConcurrent(client string) int {
	if p.MaxConcurrent == nil {
		return 0
	}
	return p.MaxConcurrent(client)
}

// ClaimNext atomically claims the next eligible job. Candidates are ordered
// by priority, then round-robin across clients — each client's oldest due
// job first, least recently served client first — then age, so one client's
// batch never starves another's interactive requests. Jobs in "received" move to
// "resolving"; attempts is incremented at claim time, not on failure, so an
// attempt cut short by a crash or restart is still counted after recovery.
// A received job whose attempts already reached max_attempts is failed here
// instead of claimed, so a poison payload cannot retry forever even when its
// attempts end in crashes. A client at its in-flight ceiling is skipped for
// another client's work. Jobs in "awaiting_target" whose poll time is due
// are claimed by pushing next_attempt_at forward (CAS), so no other worker
// picks the same poll tick. Returns nil when no work is eligible.
func (s *Store) ClaimNext(ctx context.Context, policy ClaimPolicy) (*Job, error) {
	now := time.Now()
	cands, err := s.claimCandidates(ctx, ts(now))
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		claimed, err := s.claimOne(ctx, c, now, policy)
		if err != nil {
			return nil, err
		}
		if claimed {
			// The claim is committed: the job is now this worker's to
			// process or park. Read it with a context that survives a
			// shutdown arriving right now, or the job would be orphaned
			// in resolving until restart recovery instead of being parked.
			return s.GetJob(context.WithoutCancel(ctx), c.id)
		}
	}
	return nil, nil
}

type claimCand struct {
	id, state, target, client, nextAt string
	attempts, maxAttempts             int
}

// claimCandidates lists jobs eligible for claiming right now — due received
// jobs and awaiting_target jobs whose poll time has arrived — in scheduling
// order: priority first; then each client's oldest due job (turn 1) before
// any client's second, the least recently served client first, so a client
// that was just handed a job yields to one that is waiting; then age. A
// small batch lets a worker that loses the claim race on one row (or finds
// a client at its ceiling) try the next without re-querying.
func (s *Store) claimCandidates(ctx context.Context, nowS string) ([]claimCand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, state, target, client, next_attempt_at, attempts, max_attempts FROM (
			SELECT j.id, j.state, j.target, j.client, j.next_attempt_at, j.attempts, j.max_attempts,
			       j.priority, j.created_at, j.rowid AS seq,
			       COALESCE(cc.last_claimed_at, '') AS last_served,
			       ROW_NUMBER() OVER (PARTITION BY j.client ORDER BY j.priority DESC, j.created_at, j.rowid) AS turn
			FROM jobs j LEFT JOIN client_claims cc ON cc.client = j.client
			WHERE (j.state = ? AND (j.next_attempt_at IS NULL OR j.next_attempt_at <= ?))
			   OR (j.state = ? AND j.next_attempt_at IS NOT NULL AND j.next_attempt_at <= ?))
		ORDER BY priority DESC, turn, last_served, created_at, seq LIMIT 8`,
		StateReceived, nowS, StateAwaitingTarget, nowS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cands []claimCand
	for rows.Next() {
		var c claimCand
		var nextAt sql.NullString
		if err := rows.Scan(&c.id, &c.state, &c.target, &c.client, &nextAt, &c.attempts, &c.maxAttempts); err != nil {
			return nil, err
		}
		c.nextAt = nextAt.String
		cands = append(cands, c)
	}
	return cands, rows.Err()
}

func (s *Store) claimOne(ctx context.Context, c claimCand, now time.Time, policy ClaimPolicy) (bool, error) {
	switch c.state {
	case StateReceived:
		if c.attempts >= c.maxAttempts {
			// The previous attempts were cut short (crash/restart) without a
			// chance to give up in-process; enforce the ceiling here. The
			// code string matches the worker's max_attempts_exceeded.
			err := s.transition(ctx, c.id, StateReceived, StateFailed,
				"max_attempts_exceeded: attempts exhausted, last attempt interrupted", func(q *updateBuilder) {
					q.set("error_code = ?", "max_attempts_exceeded")
					q.set("error_message = ?", fmt.Sprintf("gave up after %d attempts; the last attempt was interrupted", c.attempts))
					q.set("next_attempt_at = NULL")
					q.set("completed_at = ?", ts(now))
				})
			if err != nil && !errors.Is(err, ErrTerminalState) {
				return false, err
			}
			return false, nil
		}
		return s.claimReceived(ctx, c.id, c.client, policy.maxConcurrent(c.client), now)
	case StateAwaitingTarget:
		return s.claimPollTick(ctx, c.id, c.nextAt, now.Add(policy.pollInterval(c.target)), now)
	}
	return false, nil
}

// claimReceived moves a received job to resolving (incrementing attempts) and
// records the audit event, all in one transaction. The CAS re-checks the
// backoff schedule — a stale candidate that another worker just requeued
// with a future next_attempt_at must lose, or its backoff would be skipped —
// and the client's in-flight ceiling, counted inside the same write
// transaction so concurrent workers cannot overshoot it together.
func (s *Store) claimReceived(ctx context.Context, id, client string, maxConcurrent int, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs
		SET state = ?, attempts = attempts + 1, next_attempt_at = NULL, updated_at = ?
		WHERE id = ? AND state = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		  AND (? <= 0 OR (SELECT count(*) FROM jobs WHERE client = ? AND state IN (?, ?)) < ?)`,
		StateResolving, ts(now), id, StateReceived, ts(now),
		maxConcurrent, client, StateResolving, StateForwarding, maxConcurrent)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		_ = tx.Rollback()
		return false, nil
	}
	// Remember that this client was just served, so the next claim prefers
	// a client that is still waiting.
	if _, err := tx.ExecContext(ctx, `INSERT INTO client_claims (client, last_claimed_at) VALUES (?, ?)
		ON CONFLICT(client) DO UPDATE SET last_claimed_at = excluded.last_claimed_at`, client, ts(now)); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := appendEventTx(ctx, tx, id, StateReceived, StateResolving, "claimed by worker", now); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	return true, tx.Commit()
}

// claimPollTick claims a due poll tick by CAS-ing next_attempt_at forward so
// no other worker picks the same tick; 0 rows affected means another worker
// won. Deliberately no audit event and no transaction here: a job_events row
// per poll tick would flood the trail, and the single CAS UPDATE is already
// atomic; the outcome is recorded by the eventual completed/failed transition.
func (s *Store) claimPollTick(ctx context.Context, id, prevNextAt string, next, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET next_attempt_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND next_attempt_at = ?`,
		ts(next), ts(now), id, StateAwaitingTarget, prevNextAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
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

// MarkCompleted finishes a job successfully. It returns ErrTerminalState if
// the job already reached a terminal state (e.g. a faster overlapping poll
// tick finished it first).
func (s *Store) MarkCompleted(ctx context.Context, id string, targetStatus int, response []byte, resultPath, resultContentType, detail string) error {
	now := time.Now()
	return s.transition(ctx, id, anyState, StateCompleted, detail, func(q *updateBuilder) {
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

// MarkFailed finishes a job with a permanent error. It returns
// ErrTerminalState if the job already reached a terminal state.
func (s *Store) MarkFailed(ctx context.Context, id, code, message string) error {
	now := time.Now()
	detail := code + ": " + message
	return s.transition(ctx, id, anyState, StateFailed, detail, func(q *updateBuilder) {
		q.set("error_code = ?", code)
		q.set("error_message = ?", message)
		q.set("next_attempt_at = NULL")
		q.set("completed_at = ?", ts(now))
	})
}

// Requeue schedules a retry after a transient error.
func (s *Store) Requeue(ctx context.Context, id string, nextAttemptAt time.Time, detail string) error {
	return s.transition(ctx, id, anyState, StateReceived, detail, func(q *updateBuilder) {
		q.set("next_attempt_at = ?", ts(nextAttemptAt))
	})
}

// RecoverInFlight requeues jobs interrupted by a restart. Jobs stuck in
// resolving/forwarding restart from the original payload (cheap thanks to the
// series cache); awaiting_target jobs keep their poll schedule.
func (s *Store) RecoverInFlight(ctx context.Context) (int, error) {
	ids, err := s.queryStrings(ctx, `SELECT id FROM jobs WHERE state IN (?, ?)`,
		StateResolving, StateForwarding)
	if err != nil {
		return 0, err
	}
	return len(ids), s.requeueAll(ctx, ids, "recovered after restart")
}

// RescueStuck requeues jobs abandoned mid-processing while the process kept
// running — the rare case where a bookkeeping write failed and the worker
// could only log: such jobs sit in resolving/forwarding with no schedule and
// would otherwise never be claimed again. Only jobs untouched since olderThan
// are rescued so in-flight work is left alone.
func (s *Store) RescueStuck(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := s.queryStrings(ctx, `SELECT id FROM jobs WHERE state IN (?, ?) AND updated_at < ?`,
		StateResolving, StateForwarding, ts(olderThan))
	if err != nil {
		return 0, err
	}
	return len(ids), s.requeueAll(ctx, ids, "rescued stuck job")
}

func (s *Store) requeueAll(ctx context.Context, ids []string, detail string) error {
	for _, id := range ids {
		if err := s.transition(ctx, id, anyState, StateReceived, detail, func(q *updateBuilder) {
			q.set("next_attempt_at = NULL")
		}); err != nil {
			return err
		}
	}
	return nil
}

// TerminalBefore lists up to limit completed/failed jobs finished before
// cutoff, oldest first, with their result file paths. The caller removes
// the files first and then calls DeleteJobs — in that order a crash in
// between leaves rows that the next sweep re-selects, instead of orphaned
// files no row references anymore. The limit bounds one housekeeping pass;
// the sweeper loops until a pass comes back short.
func (s *Store) TerminalBefore(ctx context.Context, cutoff time.Time, limit int) (ids, paths []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, result_path FROM jobs
		WHERE state IN (?, ?) AND completed_at < ?
		ORDER BY completed_at LIMIT ?`,
		StateCompleted, StateFailed, ts(cutoff), limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var path sql.NullString
		if err := rows.Scan(&id, &path); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		if path.String != "" {
			paths = append(paths, path.String)
		}
	}
	return ids, paths, rows.Err()
}

// deleteChunk caps the ids per DELETE statement. One id is one bound
// parameter and SQLite refuses statements with more than 32766 of them, so
// an unchunked delete of a large backlog would fail on every sweep and the
// database would grow without bound.
const deleteChunk = 500

// DeleteJobs removes the given jobs (their events cascade), in chunks of
// deleteChunk ids. Each chunk is its own statement: a crash between chunks
// leaves rows the next sweep re-selects, never a half-applied statement.
func (s *Store) DeleteJobs(ctx context.Context, ids []string) error {
	for start := 0; start < len(ids); start += deleteChunk {
		if err := s.deleteJobChunk(ctx, ids[start:min(start+deleteChunk, len(ids))]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteJobChunk(ctx context.Context, ids []string) error {
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// placeholders is only "?" repetition; every id is a bound parameter.
	_, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (`+placeholders+`)`, args...) //nolint:gosec // G202: constant placeholders, bound values
	return err
}

// queryStrings runs a single-column query and returns all values.
func (s *Store) queryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

// anyState, passed as transition's fromState, accepts any non-terminal state.
const anyState = ""

// transition applies a state change plus extra column updates and appends the
// audit event, all in one transaction. fromState anyState accepts any current
// state — except the terminal ones: completed/failed are final and yield
// ErrTerminalState (callers treat that as "someone else finished first").
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
	current, err := currentState(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = checkTransition(id, current, fromState, toState); err != nil {
		return err
	}
	query, args := buildUpdate(id, toState, now, build)
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update job %s: %w", id, err)
	}
	if err = appendEventTx(ctx, tx, id, current, toState, detail, now); err != nil {
		return err
	}
	return tx.Commit()
}

// currentState reads the job's state inside the transaction; a missing job
// is ErrNotFound.
func currentState(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return current, err
}

// checkTransition enforces the two rules every transition obeys: terminal
// states are final — a repeat of the same terminal outcome is refused too,
// so an overlapping duplicate completion can never overwrite a result body
// a client may already have fetched — and an expected fromState must match.
func checkTransition(id, current, fromState, toState string) error {
	if current == StateCompleted || current == StateFailed {
		return fmt.Errorf("job %s: already %s, cannot transition to %s: %w", id, current, toState, ErrTerminalState)
	}
	if fromState != anyState && current != fromState {
		return fmt.Errorf("job %s: cannot transition %s -> %s (state is %s)", id, fromState, toState, current)
	}
	return nil
}

// buildUpdate assembles a transition's UPDATE: state and updated_at plus the
// caller's extra columns. The clause strings are compile-time constants;
// every value is a bound parameter.
func buildUpdate(id, toState string, now time.Time, build func(*updateBuilder)) (string, []any) {
	b := &updateBuilder{}
	b.set("state = ?", toState)
	b.set("updated_at = ?", ts(now))
	if build != nil {
		build(b)
	}
	args := make([]any, 0, len(b.args)+1)
	args = append(args, b.args...)
	args = append(args, id)
	return `UPDATE jobs SET ` + strings.Join(b.clauses, ", ") + ` WHERE id = ?`, args
}
