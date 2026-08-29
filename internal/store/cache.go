package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GetSeries returns the cached resource-API response for a resolvent
// parameter hash, if present and not expired.
func (s *Store) GetSeries(ctx context.Context, paramHash string) ([]byte, bool, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT response FROM series_cache WHERE param_hash = ? AND expires_at > ?`,
		paramHash, ts(time.Now())).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// PutSeries stores (or refreshes) a resource-API response with a TTL.
func (s *Store) PutSeries(ctx context.Context, paramHash, resolventType string, response []byte, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO series_cache (param_hash, resolvent_type, response, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(param_hash) DO UPDATE SET response = excluded.response,
			created_at = excluded.created_at, expires_at = excluded.expires_at`,
		paramHash, resolventType, response, ts(now), ts(now.Add(ttl)))
	return err
}

// PurgeExpiredSeries deletes expired cache rows and reports how many.
func (s *Store) PurgeExpiredSeries(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM series_cache WHERE expires_at <= ?`, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
