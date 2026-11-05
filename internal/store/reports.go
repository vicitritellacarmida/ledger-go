package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// AccountTotal is the signed sum of postings for one account.
type AccountTotal struct {
	Account *domain.Account
	// Signed is debits minus credits up to the as-of date.
	Signed int64
}

// TrialBalance returns, for every account that has postings on or before
// asOf, the signed sum of those postings. currency filters when non-empty.
func (s *Store) TrialBalance(ctx context.Context, asOf time.Time, currency string) ([]AccountTotal, error) {
	q := `SELECT ` + prefixed("a.", accountColumns) + `, t.total
		FROM (SELECT account_id, SUM(amount) AS total FROM postings WHERE effective_date <= ? GROUP BY account_id) t
		JOIN accounts a ON a.id = t.account_id`
	args := []any{asOf}
	if currency != "" {
		q += " WHERE a.currency = ?"
		args = append(args, currency)
	}
	q += " ORDER BY a.currency, a.code"
	rows, err := s.conn(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountTotal
	for rows.Next() {
		var a domain.Account
		var owner sql.NullString
		var typ, status string
		var total int64
		if err := rows.Scan(&a.ID, &a.Code, &a.Name, &typ, &a.Currency, &status, &owner, &a.AllowOverdraft,
			&a.PostedBalance, &a.HeldAmount, &a.CreatedAt, &a.UpdatedAt, &total); err != nil {
			return nil, err
		}
		a.Type, a.Status, a.OwnerRef = domain.AccountType(typ), domain.AccountStatus(status), owner.String
		out = append(out, AccountTotal{Account: &a, Signed: total})
	}
	return out, rows.Err()
}

func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// BalanceAsOf returns the signed sum of an account's postings with an
// effective date on or before asOf.
func (s *Store) BalanceAsOf(ctx context.Context, accountID int64, asOf time.Time) (int64, error) {
	var total sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx,
		"SELECT SUM(amount) FROM postings WHERE account_id = ? AND effective_date <= ?", accountID, asOf).Scan(&total)
	return total.Int64, err
}

// SumPostings returns the signed sum of all postings of an account. It is
// used to check that accounts.posted_balance matches the journal.
func (s *Store) SumPostings(ctx context.Context, accountID int64) (int64, error) {
	var total sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx,
		"SELECT SUM(amount) FROM postings WHERE account_id = ?", accountID).Scan(&total)
	return total.Int64, err
}

// StatementPosition is a point in an account's posting order. Statements are
// ordered by (effective_date, posting id): by business date first, then by
// the order postings were written.
type StatementPosition struct {
	Date      time.Time
	PostingID int64
}

// StatementRow is one posting as it appears on an account statement.
type StatementRow struct {
	PostingID     int64
	EntryID       int64
	EntryType     domain.EntryType
	Description   string
	ExternalRef   string
	EffectiveDate time.Time
	Amount        int64
	CreatedAt     time.Time
}

// StatementQuery selects one page of an account statement.
type StatementQuery struct {
	AccountID int64
	From, To  *time.Time
	After     *StatementPosition
	Limit     int
}

// StatementRows returns postings of an account in statement order.
func (s *Store) StatementRows(ctx context.Context, q StatementQuery) ([]StatementRow, error) {
	where := []string{"p.account_id = ?"}
	args := []any{q.AccountID}
	if q.From != nil {
		where = append(where, "p.effective_date >= ?")
		args = append(args, *q.From)
	}
	if q.To != nil {
		where = append(where, "p.effective_date <= ?")
		args = append(args, *q.To)
	}
	if q.After != nil {
		where = append(where, "(p.effective_date > ? OR (p.effective_date = ? AND p.id > ?))")
		args = append(args, q.After.Date, q.After.Date, q.After.PostingID)
	}
	args = append(args, q.Limit)
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT p.id, e.id, e.type, e.description, e.external_ref,
		p.effective_date, p.amount, e.created_at
		FROM postings p JOIN journal_entries e ON e.id = p.entry_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY p.effective_date, p.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementRow
	for rows.Next() {
		var r StatementRow
		var typ string
		var ext sql.NullString
		if err := rows.Scan(&r.PostingID, &r.EntryID, &typ, &r.Description, &ext, &r.EffectiveDate,
			&r.Amount, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.EntryType, r.ExternalRef = domain.EntryType(typ), ext.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// SumBefore returns the signed sum of an account's postings that come
// strictly before pos in statement order. It gives the opening balance of a
// statement page.
func (s *Store) SumBefore(ctx context.Context, accountID int64, pos StatementPosition) (int64, error) {
	var total sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT SUM(amount) FROM postings
		WHERE account_id = ? AND (effective_date < ? OR (effective_date = ? AND id < ?))`,
		accountID, pos.Date, pos.Date, pos.PostingID).Scan(&total)
	return total.Int64, err
}
