package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Delivery is the completion callback of one terminal request.
type Delivery struct {
	JobID string
	URL   string
	// Event names the terminal transition: request.completed,
	// request.failed or request.cancelled.
	Event    string
	Attempts int
	// NextAttemptAt is the earliest time of the next attempt; nil once the
	// delivery succeeded or was given up.
	NextAttemptAt *time.Time
	DeliveredAt   *time.Time
	LastStatus    *int
	LastError     string
}

// Delivery states as reported by the API.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// State derives the delivery's state from its timestamps.
func (d *Delivery) State() string {
	switch {
	case d.DeliveredAt != nil:
		return DeliveryDelivered
	case d.NextAttemptAt == nil:
		return DeliveryFailed
	default:
		return DeliveryPending
	}
}

// EventFor names the callback event of a terminal state.
func EventFor(state string) string { return "request." + state }

// enqueueCallbackTx creates the delivery row for a job that just became
// terminal, when it asked for a callback. Called inside the transition's
// transaction, so the row exists exactly when the state does.
func enqueueCallbackTx(ctx context.Context, tx *sql.Tx, id, toState string, now time.Time) error {
	var url sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT callback_url FROM jobs WHERE id = ?`, id).Scan(&url); err != nil {
		return fmt.Errorf("read callback url of %s: %w", id, err)
	}
	if !url.Valid || url.String == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO callback_deliveries (job_id, url, event, attempts, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?)`, id, url.String, EventFor(toState), ts(now), ts(now), ts(now))
	if err != nil {
		return fmt.Errorf("enqueue callback for %s: %w", id, err)
	}
	return nil
}

const deliveryColumns = `job_id, url, event, attempts, next_attempt_at, delivered_at, last_status, last_error`

// GetDelivery returns a job's callback delivery or ErrNotFound (the job has
// no callback, or is not terminal yet).
func (s *Store) GetDelivery(ctx context.Context, jobID string) (*Delivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM callback_deliveries WHERE job_id = ?`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// DueDeliveries lists undelivered callbacks whose next attempt is due.
func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM callback_deliveries
		WHERE delivered_at IS NULL AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?
		ORDER BY next_attempt_at, rowid LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, fmt.Errorf("query due deliveries: %w", err)
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordAttempt stores the outcome of one delivery attempt: delivered, or
// failed with the next attempt time (nil to give up). It is a compare-and-set
// on the attempt count, so a stale deliverer cannot overwrite a newer result.
func (s *Store) RecordAttempt(ctx context.Context, jobID string, prevAttempts int, status *int, lastErr string, delivered bool, next *time.Time) error {
	now := time.Now()
	var deliveredAt, nextAt, st any
	if delivered {
		deliveredAt = ts(now)
	}
	if next != nil {
		nextAt = ts(*next)
	}
	if status != nil {
		st = *status
	}
	res, err := s.db.ExecContext(ctx, `UPDATE callback_deliveries
		SET attempts = attempts + 1, last_status = ?, last_error = ?, delivered_at = ?, next_attempt_at = ?, updated_at = ?
		WHERE job_id = ? AND attempts = ?`, st, lastErr, deliveredAt, nextAt, ts(now), jobID, prevAttempts)
	if err != nil {
		return fmt.Errorf("record delivery attempt for %s: %w", jobID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery for %s: %w", jobID, ErrNotFound)
	}
	return nil
}

func scanDelivery(r rowScanner) (*Delivery, error) {
	var d Delivery
	var next, delivered, lastErr sql.NullString
	var status sql.NullInt64
	if err := r.Scan(&d.JobID, &d.URL, &d.Event, &d.Attempts, &next, &delivered, &status, &lastErr); err != nil {
		return nil, err
	}
	d.LastError = lastErr.String
	if status.Valid {
		v := int(status.Int64)
		d.LastStatus = &v
	}
	for _, f := range []struct {
		raw sql.NullString
		dst **time.Time
	}{{next, &d.NextAttemptAt}, {delivered, &d.DeliveredAt}} {
		if !f.raw.Valid {
			continue
		}
		t, err := parseTS(f.raw.String)
		if err != nil {
			return nil, fmt.Errorf("delivery %s: %w", d.JobID, err)
		}
		*f.dst = &t
	}
	return &d, nil
}
