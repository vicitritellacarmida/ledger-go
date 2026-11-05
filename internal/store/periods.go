package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// ClosedPeriod is a closed accounting month.
type ClosedPeriod struct {
	Period   string
	ClosedAt time.Time
}

// IsPeriodClosed reports whether a period (YYYY-MM) has been closed.
func (s *Store) IsPeriodClosed(ctx context.Context, period string) (bool, error) {
	var one int
	err := s.conn(ctx).QueryRowContext(ctx, "SELECT 1 FROM accounting_periods WHERE period = ?", period).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// InsertClosedPeriod records a period as closed.
func (s *Store) InsertClosedPeriod(ctx context.Context, period string, at time.Time) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"INSERT INTO accounting_periods (period, closed_at) VALUES (?, ?)", period, at)
	if isDuplicate(err) {
		return domain.NewError(domain.KindConflict, domain.CodeAlreadyExists, "period %s is already closed", period)
	}
	return err
}

// ListClosedPeriods returns closed periods, latest first.
func (s *Store) ListClosedPeriods(ctx context.Context) ([]ClosedPeriod, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, "SELECT period, closed_at FROM accounting_periods ORDER BY period DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosedPeriod
	for rows.Next() {
		var p ClosedPeriod
		if err := rows.Scan(&p.Period, &p.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
