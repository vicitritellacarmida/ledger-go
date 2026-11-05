package domain

import (
	"time"
)

// HoldState is the lifecycle state of a hold (authorization).
type HoldState string

// Hold states.
//
//	active ──capture(partial, final=false)──> partially_captured
//	active / partially_captured ──capture(final or rest)──> captured
//	active / partially_captured ──void──> voided
//	active / partially_captured ──expire (after expires_at)──> expired
//
// captured, voided and expired are terminal.
const (
	HoldActive            HoldState = "active"
	HoldPartiallyCaptured HoldState = "partially_captured"
	HoldCaptured          HoldState = "captured"
	HoldVoided            HoldState = "voided"
	HoldExpired           HoldState = "expired"
)

// Open reports whether the hold still reserves money.
func (s HoldState) Open() bool { return s == HoldActive || s == HoldPartiallyCaptured }

// Hold reserves part of an account's available balance for a later capture.
// A hold never changes the posted balance; only a capture does, through a
// journal entry.
type Hold struct {
	ID int64
	// AccountID / AccountCode is the account whose money is reserved.
	AccountID   int64
	AccountCode string
	// DestinationAccountID / Code receives the money on capture.
	DestinationAccountID   int64
	DestinationAccountCode string
	Currency               string
	Amount                 int64
	CapturedAmount         int64
	State                  HoldState
	Description            string
	ExternalRef            string
	ExpiresAt              time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Remaining is the amount still reserved by an open hold.
func (h *Hold) Remaining() int64 {
	if !h.State.Open() {
		return 0
	}
	return h.Amount - h.CapturedAmount
}

func (h *Hold) transitionErr(action string) error {
	return NewError(KindConflict, CodeInvalidTransition, "cannot %s hold %d in state %s", action, h.ID, h.State).
		WithDetail("state", string(h.State))
}

// Capture takes amount from the hold. If final is true, or the capture uses
// the whole remainder, the hold is finished and any remainder is released.
// It returns the amount to release from the account's held total, which is
// the captured amount plus any released remainder.
func (h *Hold) Capture(amount int64, final bool, now time.Time) (released int64, err error) {
	if !h.State.Open() {
		return 0, h.transitionErr("capture")
	}
	if !now.Before(h.ExpiresAt) {
		return 0, NewError(KindRule, CodeHoldExpired, "hold %d expired at %s", h.ID, h.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if err := CheckAmount("amount", amount); err != nil {
		return 0, err
	}
	remaining := h.Remaining()
	if amount > remaining {
		return 0, NewError(KindRule, CodeCaptureExceedsHold, "capture amount %d exceeds remaining hold amount %d", amount, remaining).
			WithDetail("remaining", remaining)
	}
	h.CapturedAmount += amount
	released = amount
	if final || amount == remaining {
		released = remaining
		h.State = HoldCaptured
	} else {
		h.State = HoldPartiallyCaptured
	}
	h.UpdatedAt = now
	return released, nil
}

// Void cancels the hold and returns the amount to release.
func (h *Hold) Void(now time.Time) (released int64, err error) {
	if !h.State.Open() {
		return 0, h.transitionErr("void")
	}
	released = h.Remaining()
	h.State = HoldVoided
	h.UpdatedAt = now
	return released, nil
}

// Expire ends a hold whose expiry time has passed and returns the amount to
// release.
func (h *Hold) Expire(now time.Time) (released int64, err error) {
	if !h.State.Open() {
		return 0, h.transitionErr("expire")
	}
	if now.Before(h.ExpiresAt) {
		return 0, NewError(KindConflict, CodeInvalidTransition, "hold %d does not expire until %s", h.ID, h.ExpiresAt.UTC().Format(time.RFC3339))
	}
	released = h.Remaining()
	h.State = HoldExpired
	h.UpdatedAt = now
	return released, nil
}

// CapturePostings builds the postings of a capture: the held account side
// lowers its natural balance by amount and the destination receives the
// opposite signed amount, so the entry always balances.
func CapturePostings(held, dest *Account, amount int64) []Posting {
	heldSigned := -held.Natural(amount) // lower the natural balance of the held account
	ps := []Posting{
		{AccountID: held.ID, AccountCode: held.Code, Currency: held.Currency, Amount: heldSigned, Role: RolePayer},
		{AccountID: dest.ID, AccountCode: dest.Code, Currency: dest.Currency, Amount: -heldSigned, Role: RolePayee},
	}
	NumberPostings(ps)
	return ps
}
