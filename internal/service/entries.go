package service

import (
	"context"
	"fmt"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// PostingInput is one line of a client-built entry. Amount is a positive
// number of minor units; Side says whether it is a debit or a credit.
type PostingInput struct {
	AccountCode string
	Side        domain.Side
	Amount      int64
	// Currency defaults to the account's currency when empty.
	Currency string
}

// PostEntryInput is a manual or adjustment journal entry.
type PostEntryInput struct {
	Type        domain.EntryType
	Description string
	ExternalRef string
	// EffectiveDate defaults to today in the business time zone.
	EffectiveDate *time.Time
	Postings      []PostingInput
}

// PostEntry validates and books a client-built entry.
func (s *Service) PostEntry(ctx context.Context, in PostEntryInput) (*domain.Entry, error) {
	if in.Type == "" {
		in.Type = domain.EntryManual
	}
	if !in.Type.ClientPostable() {
		return nil, domain.Invalid("type", "type must be manual or adjustment; use the transfer and hold endpoints for %q", in.Type)
	}
	codes := make([]string, 0, len(in.Postings))
	for i, p := range in.Postings {
		field := fmt.Sprintf("postings[%d]", i)
		if !p.Side.Valid() {
			return nil, domain.Invalid(field+".side", "side must be debit or credit")
		}
		if err := domain.CheckAmount(field+".amount", p.Amount); err != nil {
			return nil, err
		}
		codes = append(codes, p.AccountCode)
	}
	e := &domain.Entry{
		Type:        in.Type,
		Description: in.Description,
		ExternalRef: in.ExternalRef,
	}
	if in.EffectiveDate != nil {
		e.EffectiveDate = *in.EffectiveDate
	}
	// Fail fast before touching the database. The full double-entry check
	// runs in appendEntry, once currencies are known from the accounts.
	if len(in.Postings) < 2 {
		return nil, domain.Invalid("postings", "an entry needs at least two postings")
	}

	err := s.store.InTx(ctx, func(ctx context.Context) error {
		accts, err := s.lockByCode(ctx, codes...)
		if err != nil {
			return err
		}
		e.Postings = make([]domain.Posting, len(in.Postings))
		for i, p := range in.Postings {
			cur := p.Currency
			if cur == "" {
				cur = accts[p.AccountCode].Currency
			}
			e.Postings[i] = domain.Posting{AccountCode: p.AccountCode, Currency: cur, Amount: p.Side.Signed(p.Amount)}
		}
		return s.appendEntry(ctx, e, accts)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// GetEntry returns one entry with its postings.
func (s *Service) GetEntry(ctx context.Context, id int64) (*domain.Entry, error) {
	return s.store.GetEntry(ctx, id)
}

// EntryView is an entry plus derived links that are not stored on the row.
type EntryView struct {
	*domain.Entry
	// ReversedBy is the id of the entry that reversed this one, if any.
	ReversedBy *int64
}

// GetEntryView returns an entry with its reversal link.
func (s *Service) GetEntryView(ctx context.Context, id int64) (*EntryView, error) {
	e, err := s.store.GetEntry(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &EntryView{Entry: e}
	if revID, ok, err := s.store.FindReversal(ctx, id); err != nil {
		return nil, err
	} else if ok {
		v.ReversedBy = &revID
	}
	return v, nil
}

// ListEntriesInput filters and pages the entry list (newest first).
type ListEntriesInput struct {
	Type        domain.EntryType
	ExternalRef string
	BeforeID    int64
	Limit       int
}

// ListEntries returns a page of entries and the BeforeID of the next page.
func (s *Service) ListEntries(ctx context.Context, in ListEntriesInput) ([]*domain.Entry, int64, error) {
	if in.Type != "" && !in.Type.Valid() {
		return nil, 0, domain.Invalid("type", "unknown entry type %q", in.Type)
	}
	limit := clampLimit(in.Limit, 50, 200)
	rows, err := s.store.ListEntries(ctx, store.EntryFilter{
		Type: in.Type, ExternalRef: in.ExternalRef, BeforeID: in.BeforeID, Limit: limit + 1,
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

// ReverseInput controls a reversal.
type ReverseInput struct {
	// EffectiveDate defaults to today. A reversal is usually dated today even
	// when the original is in an earlier, possibly closed, period.
	EffectiveDate *time.Time
	Description   string
}

// ReverseEntry books an entry that exactly cancels entry id. An entry can be
// reversed once, reversals cannot themselves be reversed, and a payment with
// live refunds cannot be reversed (that would refund the customer twice).
// Overdraft
// rules apply: reversing a top-up whose money was already spent fails with
// insufficient_funds.
func (s *Service) ReverseEntry(ctx context.Context, id int64, in ReverseInput) (*domain.Entry, error) {
	var rev *domain.Entry
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		orig, err := s.store.GetEntry(ctx, id)
		if err != nil {
			return err
		}
		date := s.Today()
		if in.EffectiveDate != nil {
			date = *in.EffectiveDate
		}
		rev, err = orig.Reverse(date, in.Description)
		if err != nil {
			return err
		}
		codes := make([]string, len(orig.Postings))
		for i, p := range orig.Postings {
			codes[i] = p.AccountCode
		}
		accts, err := s.lockByCode(ctx, codes...)
		if err != nil {
			return err
		}
		// Checked after locking: a concurrent reversal of the same entry
		// locks the same accounts, so by now it has committed or rolled back.
		if revID, ok, err := s.store.FindReversal(ctx, id); err != nil {
			return err
		} else if ok {
			return domain.NewError(domain.KindConflict, domain.CodeAlreadyReversed, "entry %d was already reversed by entry %d", id, revID).
				WithDetail("reversed_by", revID)
		}
		if orig.Type == domain.EntryPayment {
			refunded, err := s.store.SumRefunded(ctx, id)
			if err != nil {
				return err
			}
			if refunded > 0 {
				return domain.NewError(domain.KindRule, domain.CodeNotReversible,
					"payment %d has %d refunded; reverse its refunds first", id, refunded).
					WithDetail("refunded", refunded)
			}
		}
		return s.appendEntry(ctx, rev, accts)
	})
	if err != nil {
		return nil, err
	}
	return rev, nil
}
