package service

import (
	"context"
	"fmt"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// Hold expiry limits.
const (
	DefaultHoldTTL = 7 * 24 * time.Hour
	MaxHoldTTL     = 30 * 24 * time.Hour
)

// PlaceHoldInput reserves money on an account for a later capture.
type PlaceHoldInput struct {
	AccountCode            string
	DestinationAccountCode string
	Amount                 int64
	// Currency defaults to the account's currency when empty.
	Currency string
	// ExpiresAt defaults to now + DefaultHoldTTL and may be at most
	// MaxHoldTTL in the future.
	ExpiresAt *time.Time
	// TTL is an alternative to ExpiresAt: the hold expires TTL after now.
	TTL         time.Duration
	Description string
	ExternalRef string
}

// PlaceHold creates an active hold and raises the account's held amount.
func (s *Service) PlaceHold(ctx context.Context, in PlaceHoldInput) (*domain.Hold, error) {
	if err := domain.CheckAmount("amount", in.Amount); err != nil {
		return nil, err
	}
	if in.AccountCode == in.DestinationAccountCode {
		return nil, domain.Invalid("destination_account_code", "destination must differ from the held account")
	}
	if len(in.Description) > 255 {
		return nil, domain.Invalid("description", "description must be at most 255 characters")
	}
	if len(in.ExternalRef) > 128 {
		return nil, domain.Invalid("external_ref", "external_ref must be at most 128 characters")
	}
	now := s.now()
	expires := now.Add(DefaultHoldTTL)
	switch {
	case in.ExpiresAt != nil && in.TTL != 0:
		return nil, domain.Invalid("expires_at", "give expires_at or a TTL, not both")
	case in.ExpiresAt != nil:
		expires = in.ExpiresAt.UTC().Truncate(time.Microsecond)
	case in.TTL != 0:
		expires = now.Add(in.TTL)
	}
	if !expires.After(now) {
		return nil, domain.Invalid("expires_at", "expires_at must be in the future")
	}
	if expires.Sub(now) > MaxHoldTTL {
		return nil, domain.Invalid("expires_at", "expires_at must be at most %d days from now", int(MaxHoldTTL.Hours()/24))
	}

	var h *domain.Hold
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		// The destination is only checked, not locked: a hold does not
		// change its balance. Capture locks it later.
		dest, err := s.store.GetAccountByCode(ctx, in.DestinationAccountCode)
		if err != nil {
			return err
		}
		accts, err := s.lockByCode(ctx, in.AccountCode)
		if err != nil {
			return err
		}
		a := accts[in.AccountCode]
		cur := in.Currency
		if cur == "" {
			cur = a.Currency
		}
		if dest.Currency != cur {
			return domain.NewError(domain.KindValidation, domain.CodeCurrencyMismatch,
				"destination %s is in %s, hold is in %s", dest.Code, dest.Currency, cur)
		}
		if dest.Status == domain.StatusClosed {
			return domain.NewError(domain.KindRule, domain.CodeAccountNotActive, "destination account %s is closed", dest.Code)
		}
		if err := a.PlaceHold(cur, in.Amount); err != nil {
			return err
		}
		h = &domain.Hold{
			AccountID: a.ID, AccountCode: a.Code,
			DestinationAccountID: dest.ID, DestinationAccountCode: dest.Code,
			Currency: cur, Amount: in.Amount, State: domain.HoldActive,
			Description: in.Description, ExternalRef: in.ExternalRef,
			ExpiresAt: expires, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.store.InsertHold(ctx, h); err != nil {
			return err
		}
		return s.saveHeld(ctx, a)
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// GetHold returns one hold.
func (s *Service) GetHold(ctx context.Context, id int64) (*domain.Hold, error) {
	return s.store.GetHold(ctx, id, false)
}

// ListAccountHolds returns holds on an account, newest first.
func (s *Service) ListAccountHolds(ctx context.Context, code string, openOnly bool, limit int) ([]*domain.Hold, error) {
	a, err := s.store.GetAccountByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return s.store.ListHoldsByAccount(ctx, a.ID, openOnly, clampLimit(limit, 50, 200))
}

// CaptureInput controls a capture.
type CaptureInput struct {
	// Amount defaults to the whole remaining hold amount.
	Amount *int64
	// Final releases whatever remains after this capture.
	Final         bool
	EffectiveDate *time.Time
	Description   string
}

// CaptureResult is the updated hold and the journal entry of the capture.
type CaptureResult struct {
	Hold  *domain.Hold
	Entry *domain.Entry
}

// CaptureHold moves captured money from the held account to the hold's
// destination with a hold_capture entry, and releases the reserved amount.
func (s *Service) CaptureHold(ctx context.Context, id int64, in CaptureInput) (*CaptureResult, error) {
	var res CaptureResult
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		h, err := s.store.GetHold(ctx, id, true)
		if err != nil {
			return err
		}
		amount := h.Remaining()
		if in.Amount != nil {
			amount = *in.Amount
		}
		accts, err := s.lockByCode(ctx, h.AccountCode, h.DestinationAccountCode)
		if err != nil {
			return err
		}
		held, dest := accts[h.AccountCode], accts[h.DestinationAccountCode]
		released, err := h.Capture(amount, in.Final, s.now())
		if err != nil {
			return err
		}
		// Release first: the captured money stops being "held" and becomes
		// "posted away" in the same transaction, so available never dips.
		held.ReleaseHold(released)

		desc := in.Description
		if desc == "" {
			desc = fmt.Sprintf("Capture of hold %d", h.ID)
		}
		holdID := h.ID
		e := &domain.Entry{
			Type:        domain.EntryHoldCapture,
			Description: desc,
			ExternalRef: h.ExternalRef,
			HoldID:      &holdID,
			Postings:    domain.CapturePostings(held, dest, amount),
		}
		if in.EffectiveDate != nil {
			e.EffectiveDate = *in.EffectiveDate
		}
		if err := s.appendEntry(ctx, e, accts); err != nil {
			return err
		}
		if err := s.store.UpdateHold(ctx, h); err != nil {
			return err
		}
		res = CaptureResult{Hold: h, Entry: e}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// VoidHold cancels an open hold and releases its remaining amount.
func (s *Service) VoidHold(ctx context.Context, id int64) (*domain.Hold, error) {
	return s.endHold(ctx, id, func(h *domain.Hold, now time.Time) (int64, error) { return h.Void(now) })
}

func (s *Service) endHold(ctx context.Context, id int64, end func(*domain.Hold, time.Time) (int64, error)) (*domain.Hold, error) {
	var out *domain.Hold
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		h, err := s.store.GetHold(ctx, id, true)
		if err != nil {
			return err
		}
		locked, err := s.store.LockAccounts(ctx, []int64{h.AccountID})
		if err != nil {
			return err
		}
		a := locked[h.AccountID]
		released, err := end(h, s.now())
		if err != nil {
			return err
		}
		a.ReleaseHold(released)
		if err := s.store.UpdateHold(ctx, h); err != nil {
			return err
		}
		out = h
		return s.saveHeld(ctx, a)
	})
	return out, err
}

// ExpireDueHolds expires up to limit open holds whose expires_at has passed
// and returns how many it expired. Each hold is expired in its own
// transaction so one problem does not block the rest.
func (s *Service) ExpireDueHolds(ctx context.Context, limit int) (int, error) {
	ids, err := s.store.ListDueHoldIDs(ctx, s.now(), clampLimit(limit, 100, 1000))
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, id := range ids {
		_, err := s.endHold(ctx, id, func(h *domain.Hold, now time.Time) (int64, error) { return h.Expire(now) })
		switch {
		case err == nil:
			expired++
		case domain.HasCode(err, domain.CodeInvalidTransition):
			// Captured or voided between listing and locking: nothing to do.
		default:
			return expired, err
		}
	}
	return expired, nil
}

// RunHoldExpiry calls ExpireDueHolds every interval until ctx is done.
func (s *Service) RunHoldExpiry(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.ExpireDueHolds(ctx, 500)
			if err != nil && ctx.Err() == nil {
				s.log.Error("hold expiry failed", "error", err)
			} else if n > 0 {
				s.log.Info("holds expired", "count", n)
			}
		}
	}
}
