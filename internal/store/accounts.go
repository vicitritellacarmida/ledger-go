package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

const accountColumns = `id, code, name, type, currency, status, owner_ref, allow_overdraft,
	posted_balance, held_amount, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccount(r rowScanner) (*domain.Account, error) {
	var a domain.Account
	var owner sql.NullString
	var typ, status string
	if err := r.Scan(&a.ID, &a.Code, &a.Name, &typ, &a.Currency, &status, &owner, &a.AllowOverdraft,
		&a.PostedBalance, &a.HeldAmount, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Type = domain.AccountType(typ)
	a.Status = domain.AccountStatus(status)
	a.OwnerRef = owner.String
	return &a, nil
}

// InsertAccount stores a new account and sets its ID. A duplicate code is
// reported as a domain conflict error.
func (s *Store) InsertAccount(ctx context.Context, a *domain.Account) error {
	res, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO accounts
		(code, name, type, currency, status, owner_ref, allow_overdraft, posted_balance, held_amount, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Code, a.Name, string(a.Type), a.Currency, string(a.Status), nullString(a.OwnerRef), a.AllowOverdraft,
		a.PostedBalance, a.HeldAmount, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if isDuplicate(err) {
			return domain.NewError(domain.KindConflict, domain.CodeAlreadyExists, "account %s already exists", a.Code).
				WithDetail("account_code", a.Code)
		}
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// GetAccountByCode loads one account without locking it.
func (s *Store) GetAccountByCode(ctx context.Context, code string) (*domain.Account, error) {
	a, err := scanAccount(s.conn(ctx).QueryRowContext(ctx,
		"SELECT "+accountColumns+" FROM accounts WHERE code = ?", code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(domain.CodeAccountNotFound, "account %s not found", code)
	}
	return a, err
}

// GetAccountByID loads one account without locking it.
func (s *Store) GetAccountByID(ctx context.Context, id int64) (*domain.Account, error) {
	a, err := scanAccount(s.conn(ctx).QueryRowContext(ctx,
		"SELECT "+accountColumns+" FROM accounts WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(domain.CodeAccountNotFound, "account %d not found", id)
	}
	return a, err
}

// ResolveAccountIDs maps account codes to ids. Any unknown code is an error
// that names the first missing code.
func (s *Store) ResolveAccountIDs(ctx context.Context, codes []string) (map[string]int64, error) {
	if len(codes) == 0 {
		return map[string]int64{}, nil
	}
	args := make([]any, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	rows, err := s.conn(ctx).QueryContext(ctx,
		"SELECT code, id FROM accounts WHERE code IN ("+placeholders(len(codes))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64, len(codes))
	for rows.Next() {
		var code string
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			return nil, err
		}
		out[code] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range codes {
		if _, ok := out[c]; !ok {
			return nil, notFound(domain.CodeAccountNotFound, "account %s not found", c).WithDetail("account_code", c)
		}
	}
	return out, nil
}

// LockAccounts loads the given accounts with SELECT ... FOR UPDATE.
//
// Rows are always locked in ascending id order. If every transaction takes
// its locks in the same global order, two transactions can never each hold a
// lock the other one waits for, which is the classic cause of deadlocks
// (A->B transfer racing a B->A transfer).
func (s *Store) LockAccounts(ctx context.Context, ids []int64) (map[int64]*domain.Account, error) {
	if !InTransaction(ctx) {
		return nil, errors.New("store: LockAccounts requires a transaction")
	}
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i] < uniq[j] })
	out := make(map[int64]*domain.Account, len(uniq))
	// One statement per row makes the lock order explicit and independent
	// of the query plan InnoDB picks for an IN list.
	for _, id := range uniq {
		a, err := scanAccount(s.conn(ctx).QueryRowContext(ctx,
			"SELECT "+accountColumns+" FROM accounts WHERE id = ? FOR UPDATE", id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound(domain.CodeAccountNotFound, "account %d not found", id)
		}
		if err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, nil
}

// UpdateAccountBalances writes the posted balance and held amount.
func (s *Store) UpdateAccountBalances(ctx context.Context, a *domain.Account) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"UPDATE accounts SET posted_balance = ?, held_amount = ?, updated_at = ? WHERE id = ?",
		a.PostedBalance, a.HeldAmount, a.UpdatedAt, a.ID)
	return err
}

// UpdateAccountStatus writes the status.
func (s *Store) UpdateAccountStatus(ctx context.Context, a *domain.Account) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?", string(a.Status), a.UpdatedAt, a.ID)
	return err
}

// AccountFilter narrows ListAccounts. Zero values mean "any".
type AccountFilter struct {
	Type     domain.AccountType
	Currency string
	Status   domain.AccountStatus
	OwnerRef string
	// CodePrefix matches codes that start with the given text.
	CodePrefix string
	AfterID    int64
	Limit      int
}

// ListAccounts returns accounts ordered by id, after AfterID.
func (s *Store) ListAccounts(ctx context.Context, f AccountFilter) ([]*domain.Account, error) {
	var where []string
	var args []any
	where = append(where, "id > ?")
	args = append(args, f.AfterID)
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, string(f.Type))
	}
	if f.Currency != "" {
		where = append(where, "currency = ?")
		args = append(args, f.Currency)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.OwnerRef != "" {
		where = append(where, "owner_ref = ?")
		args = append(args, f.OwnerRef)
	}
	if f.CodePrefix != "" {
		where = append(where, "code LIKE ?")
		args = append(args, escapeLike(f.CodePrefix)+"%")
	}
	args = append(args, f.Limit)
	rows, err := s.conn(ctx).QueryContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE "+
		strings.Join(where, " AND ")+" ORDER BY id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnsureAccount inserts the account if no account with its code exists.
// It reports whether a row was created.
func (s *Store) EnsureAccount(ctx context.Context, a *domain.Account, now time.Time) (bool, error) {
	// Check first so that restarts do not burn AUTO_INCREMENT values on
	// failed inserts; the insert below still handles a concurrent creator.
	if _, err := s.GetAccountByCode(ctx, a.Code); err == nil {
		return false, nil
	} else if !domain.HasCode(err, domain.CodeAccountNotFound) {
		return false, err
	}
	a.CreatedAt, a.UpdatedAt = now, now
	err := s.InsertAccount(ctx, a)
	if domain.HasCode(err, domain.CodeAlreadyExists) {
		return false, nil
	}
	return err == nil, err
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
