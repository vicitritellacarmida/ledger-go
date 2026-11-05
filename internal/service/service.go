// Package service implements the ledger use cases. Each exported method is
// one business operation; it validates input with the domain package, runs
// inside a database transaction, takes row locks in a fixed order, and
// appends journal entries to the audit hash chain.
//
// Lock order, used by every operation that takes more than one lock:
//
//  1. idempotency key row (taken by the HTTP layer, if any)
//  2. hold row (hold operations only)
//  3. account rows, in ascending id order
//  4. the chain_head row (only when an entry is appended or a period closed)
//
// Because every transaction acquires locks in this order, no two
// transactions can wait on each other in a cycle.
package service

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// Options configures a Service.
type Options struct {
	// Clock returns the current time. Defaults to time.Now.
	Clock func() time.Time
	// Location is the business time zone used to decide "today".
	// Defaults to UTC.
	Location *time.Location
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Currencies are the currencies whose system accounts EnsureChart seeds.
	Currencies []string
}

// Service is the ledger application service.
type Service struct {
	store      *store.Store
	clock      func() time.Time
	loc        *time.Location
	log        *slog.Logger
	currencies []string
}

// New builds a Service.
func New(st *store.Store, opts Options) *Service {
	s := &Service{store: st, clock: opts.Clock, loc: opts.Location, log: opts.Logger, currencies: opts.Currencies}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.loc == nil {
		s.loc = time.UTC
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// now returns the current time in UTC truncated to microseconds, the
// precision of DATETIME(6). Truncating before hashing guarantees that the
// value read back from the database hashes the same way.
func (s *Service) now() time.Time { return s.clock().UTC().Truncate(time.Microsecond) }

// Today returns the current business date as midnight UTC.
func (s *Service) Today() time.Time { return domain.DateOf(s.clock(), s.loc) }

// EnsureChart creates any missing system accounts for the configured
// currencies and returns how many it created.
func (s *Service) EnsureChart(ctx context.Context) (int, error) {
	created := 0
	now := s.now()
	for _, cur := range s.currencies {
		for _, a := range domain.ChartFor(cur) {
			ok, err := s.store.EnsureAccount(ctx, a, now)
			if err != nil {
				return created, err
			}
			if ok {
				created++
			}
		}
	}
	return created, nil
}

// lockByCode resolves account codes and locks the rows in id order. The
// result is keyed by code.
func (s *Service) lockByCode(ctx context.Context, codes ...string) (map[string]*domain.Account, error) {
	ids, err := s.store.ResolveAccountIDs(ctx, uniqueStrings(codes))
	if err != nil {
		return nil, err
	}
	list := make([]int64, 0, len(ids))
	for _, id := range ids {
		list = append(list, id)
	}
	locked, err := s.store.LockAccounts(ctx, list)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*domain.Account, len(locked))
	for _, a := range locked {
		out[a.Code] = a
	}
	return out, nil
}

// checkEffectiveDate rejects dates in the future or inside a closed period.
// It must run while the chain head is locked, because closing a period also
// takes that lock; otherwise an entry could slip into a period that is being
// closed at the same moment.
func (s *Service) checkEffectiveDate(ctx context.Context, date time.Time) error {
	if date.After(s.Today()) {
		return domain.NewError(domain.KindRule, domain.CodeFutureEffectiveDate,
			"effective_date %s is after today (%s)", domain.FormatDate(date), domain.FormatDate(s.Today())).
			WithDetail("field", "effective_date")
	}
	p := domain.PeriodOf(date)
	closed, err := s.store.IsPeriodClosed(ctx, p.String())
	if err != nil {
		return err
	}
	if closed {
		return domain.NewError(domain.KindRule, domain.CodePeriodClosed,
			"period %s is closed; post with an effective date in an open period", p).
			WithDetail("period", p.String())
	}
	return nil
}

// appendEntry is the single path through which entries reach the journal.
// The caller must be inside a transaction and must already hold the locks of
// every account in accts (keyed by code), which must cover all postings.
//
// It applies each posting to its account (enforcing status, currency and
// overdraft rules), locks the chain head, checks the effective date, seals
// the entry into the hash chain, and writes entry, postings and balances.
func (s *Service) appendEntry(ctx context.Context, e *domain.Entry, accts map[string]*domain.Account) error {
	if e.EffectiveDate.IsZero() {
		e.EffectiveDate = s.Today()
	}
	domain.NumberPostings(e.Postings)
	if err := e.Validate(); err != nil {
		return err
	}
	now := s.now()
	touched := make([]*domain.Account, 0, len(e.Postings))
	for i := range e.Postings {
		p := &e.Postings[i]
		a, ok := accts[p.AccountCode]
		if !ok {
			return domain.NewError(domain.KindNotFound, domain.CodeAccountNotFound, "account %s not found", p.AccountCode)
		}
		p.AccountID = a.ID
		if err := a.ApplyPosting(p.Currency, p.Amount); err != nil {
			return err
		}
		a.UpdatedAt = now
		touched = append(touched, a)
	}

	lastSeq, lastHash, err := s.store.LockChainHead(ctx)
	if err != nil {
		return err
	}
	if err := s.checkEffectiveDate(ctx, e.EffectiveDate); err != nil {
		return err
	}
	e.CreatedAt = now
	domain.SealEntry(e, lastSeq, lastHash)
	if err := s.store.InsertEntry(ctx, e); err != nil {
		return err
	}
	if err := s.store.UpdateChainHead(ctx, e.Seq, e.Hash, now); err != nil {
		return err
	}
	// Update in id order to keep the write pattern predictable.
	sort.Slice(touched, func(i, j int) bool { return touched[i].ID < touched[j].ID })
	for _, a := range touched {
		if err := s.store.UpdateAccountBalances(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// saveHeld writes balances of accounts whose held amount changed but which
// were not part of an appended entry.
func (s *Service) saveHeld(ctx context.Context, accts ...*domain.Account) error {
	now := s.now()
	for _, a := range accts {
		a.UpdatedAt = now
		if err := s.store.UpdateAccountBalances(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
