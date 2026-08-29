package store

import (
	"context"
	"database/sql"
	"time"
)

// Event is one audit-trail entry for a job.
type Event struct {
	ID        int64
	JobID     string
	FromState string
	ToState   string
	Detail    string
	CreatedAt time.Time
}

func appendEventTx(ctx context.Context, tx *sql.Tx, jobID, from, to, detail string, now time.Time) error {
	var fromVal any
	if from != "" {
		fromVal = from
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, from_state, to_state, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`, jobID, fromVal, to, detail, ts(now))
	return err
}

// ListEvents returns a job's audit trail in chronological order.
func (s *Store) ListEvents(ctx context.Context, jobID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, from_state, to_state, detail, created_at
		FROM job_events WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var from, detail sql.NullString
		var created string
		if err := rows.Scan(&e.ID, &e.JobID, &from, &e.ToState, &detail, &created); err != nil {
			return nil, err
		}
		e.FromState = from.String
		e.Detail = detail.String
		if e.CreatedAt, err = parseTS(created); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
