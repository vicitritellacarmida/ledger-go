package domain

import (
	"regexp"
	"time"
)

// AccountType is one of the five classic accounting categories.
type AccountType string

// Account types.
const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountEquity    AccountType = "equity"
	AccountRevenue   AccountType = "revenue"
	AccountExpense   AccountType = "expense"
)

// Valid reports whether t is a known account type.
func (t AccountType) Valid() bool {
	switch t {
	case AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense:
		return true
	}
	return false
}

// NormalBalance returns the side on which the account type grows.
//
// The accounting equation is Assets + Expenses = Liabilities + Equity + Revenue.
// Accounts on the left side grow with debits, accounts on the right side grow
// with credits. That is the only reason the "normal balance" exists: it tells
// you which sign means "more" for this account.
func (t AccountType) NormalBalance() Side {
	switch t {
	case AccountAsset, AccountExpense:
		return Debit
	default:
		return Credit
	}
}

// Side is the debit or credit side of a posting.
type Side string

// Posting sides.
const (
	Debit  Side = "debit"
	Credit Side = "credit"
)

// Valid reports whether s is debit or credit.
func (s Side) Valid() bool { return s == Debit || s == Credit }

// Signed converts a positive amount on this side into the ledger's signed
// convention: debits are positive, credits are negative.
func (s Side) Signed(amount int64) int64 {
	if s == Credit {
		return -amount
	}
	return amount
}

// SideOf splits a signed amount into its side and positive magnitude.
func SideOf(signed int64) (Side, int64) {
	if signed < 0 {
		return Credit, -signed
	}
	return Debit, signed
}

// AccountStatus is the lifecycle state of an account.
type AccountStatus string

// Account statuses.
const (
	// StatusActive accounts accept postings and holds.
	StatusActive AccountStatus = "active"
	// StatusFrozen accounts reject new postings, holds and captures. Existing
	// holds can still be voided or expire. Freezing is reversible.
	StatusFrozen AccountStatus = "frozen"
	// StatusClosed accounts are permanently retired. Only empty accounts can
	// be closed.
	StatusClosed AccountStatus = "closed"
)

// Valid reports whether s is a known status.
func (s AccountStatus) Valid() bool {
	return s == StatusActive || s == StatusFrozen || s == StatusClosed
}

var codePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]{1,99}$`)

// Account is a ledger account. Balances are stored in the signed convention
// (debits minus credits); use NaturalBalance for the human-facing value.
type Account struct {
	ID       int64
	Code     string
	Name     string
	Type     AccountType
	Currency string
	Status   AccountStatus
	// OwnerRef optionally links the account to an outside party such as a
	// merchant or customer id. The ledger does not interpret it.
	OwnerRef string
	// AllowOverdraft lets the available balance go below zero. System
	// accounts such as suspense accounts usually allow it; customer wallets
	// never do.
	AllowOverdraft bool
	// PostedBalance is the sum of all postings: debits minus credits.
	PostedBalance int64
	// HeldAmount is the total of open holds, in natural units (always >= 0).
	HeldAmount int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewAccount validates the inputs and returns an active account with zero
// balances.
func NewAccount(code, name string, typ AccountType, currency, ownerRef string, allowOverdraft bool) (*Account, error) {
	if !codePattern.MatchString(code) {
		return nil, Invalid("code", "code must be 2-100 characters of a-z, 0-9, ':', '.', '_' or '-' and start with a letter or digit")
	}
	if name == "" || len(name) > 200 {
		return nil, Invalid("name", "name is required and must be at most 200 characters")
	}
	if !typ.Valid() {
		return nil, Invalid("type", "type must be one of asset, liability, equity, revenue, expense")
	}
	if _, err := LookupCurrency(currency); err != nil {
		return nil, err
	}
	if len(ownerRef) > 100 {
		return nil, Invalid("owner_ref", "owner_ref must be at most 100 characters")
	}
	return &Account{
		Code:           code,
		Name:           name,
		Type:           typ,
		Currency:       currency,
		Status:         StatusActive,
		OwnerRef:       ownerRef,
		AllowOverdraft: allowOverdraft,
	}, nil
}

// NormalBalance returns the side on which this account grows.
func (a *Account) NormalBalance() Side { return a.Type.NormalBalance() }

// Natural converts a signed amount into this account's natural sign, so a
// positive result always means "the account holds more".
func (a *Account) Natural(signed int64) int64 {
	if a.NormalBalance() == Credit {
		return -signed
	}
	return signed
}

// NaturalBalance is the posted balance in the account's natural sign.
func (a *Account) NaturalBalance() int64 { return a.Natural(a.PostedBalance) }

// AvailableBalance is the natural posted balance minus open holds.
func (a *Account) AvailableBalance() int64 { return a.NaturalBalance() - a.HeldAmount }

func (a *Account) requireActive() error {
	if a.Status != StatusActive {
		return NewError(KindRule, CodeAccountNotActive, "account %s is %s", a.Code, a.Status).
			WithDetail("account_code", a.Code).WithDetail("status", string(a.Status))
	}
	return nil
}

func (a *Account) insufficient(need int64) error {
	return NewError(KindRule, CodeInsufficientFunds, "account %s has insufficient available balance", a.Code).
		WithDetail("account_code", a.Code).
		WithDetail("available", a.AvailableBalance()).
		WithDetail("required", need)
}

// ApplyPosting adds a signed posting amount to the posted balance.
//
// If the account does not allow overdraft and the posting lowers the natural
// balance, the available balance after the posting must stay at or above
// zero. Postings that raise the balance are always accepted, so an account
// that is already short can still be refilled.
func (a *Account) ApplyPosting(currency string, signed int64) error {
	if err := a.requireActive(); err != nil {
		return err
	}
	if currency != a.Currency {
		return NewError(KindValidation, CodeCurrencyMismatch, "account %s is in %s, posting is in %s", a.Code, a.Currency, currency).
			WithDetail("account_code", a.Code)
	}
	next, err := AddChecked(a.PostedBalance, signed)
	if err != nil {
		return err
	}
	if !a.AllowOverdraft && a.Natural(signed) < 0 && a.Natural(next)-a.HeldAmount < 0 {
		return a.insufficient(-a.Natural(signed))
	}
	a.PostedBalance = next
	return nil
}

// PlaceHold reserves amount (natural units) of the available balance.
func (a *Account) PlaceHold(currency string, amount int64) error {
	if err := a.requireActive(); err != nil {
		return err
	}
	if currency != a.Currency {
		return NewError(KindValidation, CodeCurrencyMismatch, "account %s is in %s, hold is in %s", a.Code, a.Currency, currency)
	}
	held, err := AddChecked(a.HeldAmount, amount)
	if err != nil {
		return err
	}
	if !a.AllowOverdraft && a.AvailableBalance()-amount < 0 {
		return a.insufficient(amount)
	}
	a.HeldAmount = held
	return nil
}

// ReleaseHold gives back amount of a previously placed hold. It never fails
// on a frozen account, because releasing money is always safe.
func (a *Account) ReleaseHold(amount int64) {
	a.HeldAmount -= amount
	if a.HeldAmount < 0 {
		a.HeldAmount = 0
	}
}

// ChangeStatus moves the account to a new status, enforcing the lifecycle:
// active <-> frozen, active/frozen -> closed. Closed is final, and only an
// account with zero balance and no holds can be closed.
func (a *Account) ChangeStatus(to AccountStatus) error {
	if !to.Valid() {
		return Invalid("status", "status must be active, frozen or closed")
	}
	if a.Status == to {
		return nil
	}
	if a.Status == StatusClosed {
		return NewError(KindConflict, CodeInvalidTransition, "account %s is closed and cannot change status", a.Code)
	}
	if to == StatusClosed && (a.PostedBalance != 0 || a.HeldAmount != 0) {
		return NewError(KindRule, CodeAccountNotEmpty, "account %s must have zero balance and no holds before closing", a.Code).
			WithDetail("posted_balance", a.NaturalBalance()).WithDetail("held_amount", a.HeldAmount)
	}
	a.Status = to
	return nil
}
