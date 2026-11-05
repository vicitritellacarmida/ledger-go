package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IdempotencyRecord is a stored request fingerprint and its response.
type IdempotencyRecord struct {
	Key            string
	Method         string
	Path           string
	RequestHash    string
	ResponseStatus int
	ResponseBody   []byte
	CreatedAt      time.Time
}

// ReserveIdempotencyKey inserts a placeholder row for key inside the current
// transaction. If another transaction holds the same key uncommitted, MySQL
// makes this insert wait for it; once that transaction commits, the insert
// fails with ErrDuplicate and the caller replays the stored response. If it
// rolls back, this insert succeeds. Either way a key can never produce two
// committed side effects.
func (s *Store) ReserveIdempotencyKey(ctx context.Context, r *IdempotencyRecord) error {
	if !InTransaction(ctx) {
		return errors.New("store: ReserveIdempotencyKey requires a transaction")
	}
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO idempotency_keys
		(idem_key, method, path, request_hash, response_status, response_body, created_at)
		VALUES (?, ?, ?, ?, 0, '', ?)`, r.Key, r.Method, r.Path, r.RequestHash, r.CreatedAt)
	if isDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

// CompleteIdempotencyKey stores the final response for a reserved key.
func (s *Store) CompleteIdempotencyKey(ctx context.Context, key string, status int, body []byte) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"UPDATE idempotency_keys SET response_status = ?, response_body = ? WHERE idem_key = ?", status, body, key)
	return err
}

// GetIdempotencyKey loads a stored record. It returns (nil, nil) if the key
// does not exist.
func (s *Store) GetIdempotencyKey(ctx context.Context, key string) (*IdempotencyRecord, error) {
	var r IdempotencyRecord
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT idem_key, method, path, request_hash, response_status,
		response_body, created_at FROM idempotency_keys WHERE idem_key = ?`, key).
		Scan(&r.Key, &r.Method, &r.Path, &r.RequestHash, &r.ResponseStatus, &r.ResponseBody, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteIdempotencyKeysBefore removes records older than cutoff and returns
// how many were deleted. Keys are typically kept for 24 hours to a few days.
func (s *Store) DeleteIdempotencyKeysBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := s.conn(ctx).ExecContext(ctx,
		"DELETE FROM idempotency_keys WHERE created_at < ? ORDER BY created_at LIMIT ?", cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
