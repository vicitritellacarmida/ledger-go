package domain

import (
	"fmt"
	"sort"
	"time"
)

// EntryType labels why a journal entry exists.
type EntryType string

// Entry types. Types created by the system are rejected on the generic
// POST /v1/entries endpoint so that, for example, a "refund" always passes
// the refund rules.
const (
	EntryManual      EntryType = "manual"
	EntryAdjustment  EntryType = "adjustment"
	EntryTopup       EntryType = "topup"
	EntryPayment     EntryType = "payment"
	EntryPayout      EntryType = "payout"
	EntryRefund      EntryType = "refund"
	EntryHoldCapture EntryType = "hold_capture"
	EntryReversal    EntryType = "reversal"
)

// Valid reports whether t is a known entry type.
func (t EntryType) Valid() bool {
	switch t {
	case EntryManual, EntryAdjustment, EntryTopup, EntryPayment, EntryPayout,
		EntryRefund, EntryHoldCapture, EntryReversal:
		return true
	}
	return false
}

// ClientPostable reports whether a client may create this type directly
// through the generic entries endpoint.
func (t EntryType) ClientPostable() bool {
	return t == EntryManual || t == EntryAdjustment
}

// Posting roles mark the purpose of a line inside a system-built entry. They
// let later operations (refunds) find the right accounts without guessing
// from line order.
const (
	RolePayer   = "payer"
	RolePayee   = "payee"
	RoleFee     = "fee"
	RoleFunding = "funding"
)

// Posting is one line of a journal entry. Amount follows the signed
// convention: positive is a debit, negative is a credit.
type Posting struct {
	LineNo      int
	AccountID   int64
	AccountCode string
	Currency    string
	Amount      int64
	Role        string
}

// Side returns the posting side and its positive magnitude.
func (p Posting) Side() (Side, int64) { return SideOf(p.Amount) }

// Entry is an immutable journal entry. Once stored it is never updated;
// mistakes are corrected with a reversal entry that points back to it.
type Entry struct {
	ID            int64
	Seq           int64
	Type          EntryType
	Description   string
	ExternalRef   string
	EffectiveDate time.Time
	// ReversalOf is set on reversal entries and names the reversed entry.
	ReversalOf *int64
	// RelatedEntryID links follow-up entries, such as a refund to its payment.
	RelatedEntryID *int64
	// HoldID is set on entries produced by capturing a hold.
	HoldID    *int64
	Postings  []Posting
	PrevHash  string
	Hash      string
	CreatedAt time.Time
}

// ValidatePostings checks the double-entry rules for a set of postings:
//   - at least two postings;
//   - every amount is non-zero and within MaxAmount;
//   - every currency is supported;
//   - an account appears at most once;
//   - for each currency, debits equal credits (the signed amounts sum to 0).
func ValidatePostings(postings []Posting) error {
	if len(postings) < 2 {
		return Invalid("postings", "an entry needs at least two postings")
	}
	seen := make(map[string]bool, len(postings))
	sums := make(map[string]int64)
	for i, p := range postings {
		field := fmt.Sprintf("postings[%d]", i)
		if p.AccountCode == "" {
			return Invalid(field+".account_code", "account_code is required")
		}
		if seen[p.AccountCode] {
			return Invalid(field+".account_code", "account %s appears more than once", p.AccountCode)
		}
		seen[p.AccountCode] = true
		if p.Amount == 0 {
			return Invalid(field+".amount", "amount must not be zero")
		}
		if p.Amount > MaxAmount || p.Amount < -MaxAmount {
			return Invalid(field+".amount", "amount must not exceed %d", MaxAmount)
		}
		if _, err := LookupCurrency(p.Currency); err != nil {
			return err
		}
		s, err := AddChecked(sums[p.Currency], p.Amount)
		if err != nil {
			return err
		}
		sums[p.Currency] = s
	}
	unbalanced := make([]string, 0)
	for cur, s := range sums {
		if s != 0 {
			unbalanced = append(unbalanced, cur)
		}
	}
	if len(unbalanced) > 0 {
		sort.Strings(unbalanced)
		e := NewError(KindValidation, CodeUnbalancedEntry, "debits and credits are not equal for %v", unbalanced)
		diffs := make(map[string]int64, len(unbalanced))
		for _, cur := range unbalanced {
			diffs[cur] = sums[cur]
		}
		return e.WithDetail("difference_by_currency", diffs)
	}
	return nil
}

// Validate checks the header fields and postings of a new entry.
func (e *Entry) Validate() error {
	if !e.Type.Valid() {
		return Invalid("type", "unknown entry type %q", e.Type)
	}
	if len(e.Description) > 255 {
		return Invalid("description", "description must be at most 255 characters")
	}
	if len(e.ExternalRef) > 128 {
		return Invalid("external_ref", "external_ref must be at most 128 characters")
	}
	if e.EffectiveDate.IsZero() {
		return Invalid("effective_date", "effective_date is required")
	}
	return ValidatePostings(e.Postings)
}

// NumberPostings assigns line numbers 1..n in slice order.
func NumberPostings(ps []Posting) {
	for i := range ps {
		ps[i].LineNo = i + 1
	}
}

// Reverse builds the reversal of e: same accounts and currencies, every
// amount negated. Applying both entries leaves every balance unchanged.
func (e *Entry) Reverse(effectiveDate time.Time, description string) (*Entry, error) {
	if e.Type == EntryReversal {
		return nil, NewError(KindRule, CodeNotReversible, "entry %d is itself a reversal; post a new entry instead", e.ID)
	}
	if description == "" {
		description = fmt.Sprintf("Reversal of entry %d", e.ID)
	}
	id := e.ID
	rev := &Entry{
		Type:          EntryReversal,
		Description:   description,
		ExternalRef:   e.ExternalRef,
		EffectiveDate: effectiveDate,
		ReversalOf:    &id,
		Postings:      make([]Posting, len(e.Postings)),
	}
	for i, p := range e.Postings {
		rev.Postings[i] = Posting{
			AccountID:   p.AccountID,
			AccountCode: p.AccountCode,
			Currency:    p.Currency,
			Amount:      -p.Amount,
			Role:        p.Role,
		}
	}
	NumberPostings(rev.Postings)
	return rev, nil
}

// PostingByRole returns the first posting with the given role.
func (e *Entry) PostingByRole(role string) (Posting, bool) {
	for _, p := range e.Postings {
		if p.Role == role {
			return p, true
		}
	}
	return Posting{}, false
}
