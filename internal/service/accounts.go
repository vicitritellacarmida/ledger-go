package service

import (
	"context"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// CreateAccountInput is the data needed to open an account.
type CreateAccountInput struct {
	Code           string
	Name           string
	Type           domain.AccountType
	Currency       string
	OwnerRef       string
	AllowOverdraft bool
}

// CreateAccount opens a new active account with zero balance.
func (s *Service) CreateAccount(ctx context.Context, in CreateAccountInput) (*domain.Account, error) {
	a, err := domain.NewAccount(in.Code, in.Name, in.Type, in.Currency, in.OwnerRef, in.AllowOverdraft)
	if err != nil {
		return nil, err
	}
	now := s.now()
	a.CreatedAt, a.UpdatedAt = now, now
	if err := s.store.InsertAccount(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetAccount returns one account by code.
func (s *Service) GetAccount(ctx context.Context, code string) (*domain.Account, error) {
	return s.store.GetAccountByCode(ctx, code)
}

// ListAccountsInput filters and pages the account list.
type ListAccountsInput struct {
	Type       domain.AccountType
	Currency   string
	Status     domain.AccountStatus
	OwnerRef   string
	CodePrefix string
	AfterID    int64
	Limit      int
}

// ListAccounts returns a page of accounts ordered by id and the id to pass as
// AfterID for the next page (0 when there are no more pages).
func (s *Service) ListAccounts(ctx context.Context, in ListAccountsInput) ([]*domain.Account, int64, error) {
	if in.Type != "" && !in.Type.Valid() {
		return nil, 0, domain.Invalid("type", "unknown account type %q", in.Type)
	}
	if in.Status != "" && !in.Status.Valid() {
		return nil, 0, domain.Invalid("status", "unknown account status %q", in.Status)
	}
	limit := clampLimit(in.Limit, 50, 200)
	// Fetch one extra row to learn whether another page exists.
	rows, err := s.store.ListAccounts(ctx, store.AccountFilter{
		Type: in.Type, Currency: in.Currency, Status: in.Status, OwnerRef: in.OwnerRef,
		CodePrefix: in.CodePrefix, AfterID: in.AfterID, Limit: limit + 1,
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].ID
	}
	return rows, next, nil
}

// ChangeAccountStatus freezes, unfreezes or closes an account.
func (s *Service) ChangeAccountStatus(ctx context.Context, code string, to domain.AccountStatus) (*domain.Account, error) {
	var out *domain.Account
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		accts, err := s.lockByCode(ctx, code)
		if err != nil {
			return err
		}
		a := accts[code]
		if err := a.ChangeStatus(to); err != nil {
			return err
		}
		a.UpdatedAt = s.now()
		if err := s.store.UpdateAccountStatus(ctx, a); err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// Balance is an account balance in the account's natural sign.
type Balance struct {
	Account *domain.Account
	// AsOf is set for historical balances. Historical balances only know
	// the posted amount, because holds are not dated.
	AsOf      *time.Time
	Posted    int64
	Held      *int64
	Available *int64
}

// GetBalance returns the current balance, or the posted balance as of the
// end of a given date when asOf is not nil.
func (s *Service) GetBalance(ctx context.Context, code string, asOf *time.Time) (*Balance, error) {
	a, err := s.store.GetAccountByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if asOf != nil {
		signed, err := s.store.BalanceAsOf(ctx, a.ID, *asOf)
		if err != nil {
			return nil, err
		}
		return &Balance{Account: a, AsOf: asOf, Posted: a.Natural(signed)}, nil
	}
	held, avail := a.HeldAmount, a.AvailableBalance()
	return &Balance{Account: a, Posted: a.NaturalBalance(), Held: &held, Available: &avail}, nil
}
