package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

const holdSelect = `SELECT h.id, h.account_id, a.code, h.destination_account_id, d.code, h.currency, h.amount,
	h.captured_amount, h.state, h.description, h.external_ref, h.expires_at, h.created_at, h.updated_at
	FROM holds h JOIN accounts a ON a.id = h.account_id JOIN accounts d ON d.id = h.destination_account_id`

func scanHold(r rowScanner) (*domain.Hold, error) {
	var h domain.Hold
	var state string
	var ext sql.NullString
	if err := r.Scan(&h.ID, &h.AccountID, &h.AccountCode, &h.DestinationAccountID, &h.DestinationAccountCode,
		&h.Currency, &h.Amount, &h.CapturedAmount, &state, &h.Description, &ext, &h.ExpiresAt,
		&h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.State = domain.HoldState(state)
	h.ExternalRef = ext.String
	return &h, nil
}

// InsertHold stores a new hold and sets its ID.
func (s *Store) InsertHold(ctx context.Context, h *domain.Hold) error {
	res, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO holds
		(account_id, destination_account_id, currency, amount, captured_amount, state, description, external_ref,
		 expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.AccountID, h.DestinationAccountID, h.Currency, h.Amount, h.CapturedAmount, string(h.State),
		h.Description, nullString(h.ExternalRef), h.ExpiresAt, h.CreatedAt, h.UpdatedAt)
	if err != nil {
		return err
	}
	h.ID, err = res.LastInsertId()
	return err
}

// GetHold loads a hold. With forUpdate the row is locked (FOR UPDATE), which
// requires a transaction.
func (s *Store) GetHold(ctx context.Context, id int64, forUpdate bool) (*domain.Hold, error) {
	q := holdSelect + " WHERE h.id = ?"
	if forUpdate {
		if !InTransaction(ctx) {
			return nil, errors.New("store: GetHold(forUpdate) requires a transaction")
		}
		// Lock only the hold row; account rows are locked separately in id order.
		q += " FOR UPDATE OF h"
	}
	h, err := scanHold(s.conn(ctx).QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(domain.CodeHoldNotFound, "hold %d not found", id)
	}
	return h, err
}

// UpdateHold writes the mutable hold fields.
func (s *Store) UpdateHold(ctx context.Context, h *domain.Hold) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"UPDATE holds SET captured_amount = ?, state = ?, updated_at = ? WHERE id = ?",
		h.CapturedAmount, string(h.State), h.UpdatedAt, h.ID)
	return err
}

// ListDueHoldIDs returns ids of open holds whose expires_at is at or before now.
func (s *Store) ListDueHoldIDs(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT id FROM holds
		WHERE state IN ('active', 'partially_captured') AND expires_at <= ? ORDER BY expires_at, id LIMIT ?`,
		now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListHoldsByAccount returns the holds placed on an account, newest first.
// If openOnly is true only active and partially captured holds are returned.
func (s *Store) ListHoldsByAccount(ctx context.Context, accountID int64, openOnly bool, limit int) ([]*domain.Hold, error) {
	q := holdSelect + " WHERE h.account_id = ?"
	if openOnly {
		q += " AND h.state IN ('active', 'partially_captured')"
	}
	q += " ORDER BY h.id DESC LIMIT ?"
	rows, err := s.conn(ctx).QueryContext(ctx, q, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SumOpenHolds returns the remaining amount of all open holds on an account.
// It is used by consistency checks; the hot path uses accounts.held_amount.
func (s *Store) SumOpenHolds(ctx context.Context, accountID int64) (int64, error) {
	var total sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT SUM(amount - captured_amount) FROM holds
		WHERE account_id = ? AND state IN ('active', 'partially_captured')`, accountID).Scan(&total)
	return total.Int64, err
}
