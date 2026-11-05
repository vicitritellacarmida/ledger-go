package service

import (
	"context"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// TransferMeta holds the fields every transfer accepts.
type TransferMeta struct {
	Description   string
	ExternalRef   string
	EffectiveDate *time.Time
}

func (m TransferMeta) entry(t domain.EntryType, defaultDesc string) *domain.Entry {
	e := &domain.Entry{Type: t, Description: m.Description, ExternalRef: m.ExternalRef}
	if e.Description == "" {
		e.Description = defaultDesc
	}
	if m.EffectiveDate != nil {
		e.EffectiveDate = *m.EffectiveDate
	}
	return e
}

func checkCurrency(cur string) error {
	if cur == "" {
		return domain.Invalid("currency", "currency is required")
	}
	_, err := domain.LookupCurrency(cur)
	return err
}

// TopupInput credits a customer wallet with money received at the bank.
type TopupInput struct {
	WalletAccountCode string
	Amount            int64
	Currency          string
	TransferMeta
}

// Topup books Dr platform bank / Cr customer wallet.
func (s *Service) Topup(ctx context.Context, in TopupInput) (*domain.Entry, error) {
	if err := checkCurrency(in.Currency); err != nil {
		return nil, err
	}
	bank := domain.SysBank.Code(in.Currency)
	e := in.entry(domain.EntryTopup, "Wallet top-up")
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		accts, err := s.lockByCode(ctx, bank, in.WalletAccountCode)
		if err != nil {
			return err
		}
		e.Postings, err = domain.TopupPostings(accts[bank], accts[in.WalletAccountCode], in.Currency, in.Amount)
		if err != nil {
			return err
		}
		return s.appendEntry(ctx, e, accts)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// PaymentInput pays a merchant from a customer wallet. Give either
// FeeAmount or FeeBps (basis points of Amount), or neither for no fee.
type PaymentInput struct {
	WalletAccountCode   string
	MerchantAccountCode string
	Amount              int64
	Currency            string
	FeeAmount           *int64
	FeeBps              *int64
	TransferMeta
}

// Payment books Dr wallet / Cr merchant payable (net) / Cr fee revenue.
func (s *Service) Payment(ctx context.Context, in PaymentInput) (*domain.Entry, error) {
	if err := checkCurrency(in.Currency); err != nil {
		return nil, err
	}
	if in.FeeAmount != nil && in.FeeBps != nil {
		return nil, domain.Invalid("fee_amount", "give fee_amount or fee_bps, not both")
	}
	var fee int64
	switch {
	case in.FeeAmount != nil:
		fee = *in.FeeAmount
	case in.FeeBps != nil:
		var err error
		if fee, err = domain.FeeFromBasisPoints(in.Amount, *in.FeeBps); err != nil {
			return nil, err
		}
	}
	feeCode := domain.SysFeeRevenue.Code(in.Currency)
	e := in.entry(domain.EntryPayment, "Payment to merchant")
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		accts, err := s.lockByCode(ctx, in.WalletAccountCode, in.MerchantAccountCode, feeCode)
		if err != nil {
			return err
		}
		e.Postings, err = domain.PaymentPostings(accts[in.WalletAccountCode], accts[in.MerchantAccountCode],
			accts[feeCode], in.Currency, in.Amount, fee)
		if err != nil {
			return err
		}
		return s.appendEntry(ctx, e, accts)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// PayoutInput pays a merchant's balance out to their bank account.
type PayoutInput struct {
	MerchantAccountCode string
	Amount              int64
	Currency            string
	TransferMeta
}

// Payout books Dr merchant payable / Cr platform bank.
func (s *Service) Payout(ctx context.Context, in PayoutInput) (*domain.Entry, error) {
	if err := checkCurrency(in.Currency); err != nil {
		return nil, err
	}
	bank := domain.SysBank.Code(in.Currency)
	e := in.entry(domain.EntryPayout, "Merchant payout")
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		accts, err := s.lockByCode(ctx, in.MerchantAccountCode, bank)
		if err != nil {
			return err
		}
		e.Postings, err = domain.PayoutPostings(accts[in.MerchantAccountCode], accts[bank], in.Currency, in.Amount)
		if err != nil {
			return err
		}
		return s.appendEntry(ctx, e, accts)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Refund destinations.
const (
	// RefundToWallet credits the customer wallet that paid.
	RefundToWallet = "wallet"
	// RefundToOriginalMethod credits refunds payable; the money is then
	// returned through the payment processor outside the ledger.
	RefundToOriginalMethod = "original_method"
)

// RefundInput refunds all or part of a payment entry.
type RefundInput struct {
	PaymentEntryID int64
	// Amount defaults to everything not yet refunded.
	Amount *int64
	// RefundFee gives back the same share of the platform fee.
	RefundFee bool
	// Destination is RefundToWallet (default) or RefundToOriginalMethod.
	Destination string
	TransferMeta
}

// Refund books a refund linked to its payment. The total refunded across
// all non-reversed refunds can never exceed the payment amount.
func (s *Service) Refund(ctx context.Context, in RefundInput) (*domain.Entry, error) {
	if in.Destination == "" {
		in.Destination = RefundToWallet
	}
	if in.Destination != RefundToWallet && in.Destination != RefundToOriginalMethod {
		return nil, domain.Invalid("destination", "destination must be %q or %q", RefundToWallet, RefundToOriginalMethod)
	}
	e := in.entry(domain.EntryRefund, "Refund")
	err := s.store.InTx(ctx, func(ctx context.Context) error {
		pay, err := s.store.GetEntry(ctx, in.PaymentEntryID)
		if err != nil {
			return err
		}
		if pay.Type != domain.EntryPayment {
			return domain.NewError(domain.KindRule, domain.CodeNotAPayment, "entry %d is a %s, not a payment", pay.ID, pay.Type)
		}
		payer, okPayer := pay.PostingByRole(domain.RolePayer)
		payee, okPayee := pay.PostingByRole(domain.RolePayee)
		if !okPayer || !okPayee {
			return domain.NewError(domain.KindRule, domain.CodeNotAPayment, "entry %d has no payer/payee postings", pay.ID)
		}
		feeLine, hasFee := pay.PostingByRole(domain.RoleFee)
		cur := payer.Currency
		feeCode := domain.SysFeeRevenue.Code(cur)
		var paymentFee int64
		if hasFee {
			feeCode, paymentFee = feeLine.AccountCode, -feeLine.Amount
		}
		destCode := payer.AccountCode
		if in.Destination == RefundToOriginalMethod {
			destCode = domain.SysRefundsPayable.Code(cur)
		}
		accts, err := s.lockByCode(ctx, payee.AccountCode, feeCode, destCode)
		if err != nil {
			return err
		}
		// After the locks: concurrent refunds of this payment lock the same
		// merchant account, so this sum includes any refund they committed.
		if _, reversed, err := s.store.FindReversal(ctx, pay.ID); err != nil {
			return err
		} else if reversed {
			return domain.NewError(domain.KindRule, domain.CodeRefundExceedsPayment, "payment %d was reversed and cannot be refunded", pay.ID)
		}
		refunded, err := s.store.SumRefunded(ctx, pay.ID)
		if err != nil {
			return err
		}
		paymentAmount := payer.Amount // payer line is the debit of the full amount
		remaining := paymentAmount - refunded
		amount := remaining
		if in.Amount != nil {
			amount = *in.Amount
		}
		if err := domain.CheckAmount("amount", amount); err != nil {
			if remaining == 0 {
				return domain.NewError(domain.KindRule, domain.CodeRefundExceedsPayment, "payment %d is already fully refunded", pay.ID)
			}
			return err
		}
		if amount > remaining {
			return domain.NewError(domain.KindRule, domain.CodeRefundExceedsPayment,
				"refund of %d exceeds the refundable amount %d of payment %d", amount, remaining, pay.ID).
				WithDetail("refundable", remaining)
		}
		split := domain.SplitRefund(paymentAmount, paymentFee, amount, in.RefundFee)
		e.Postings, err = domain.RefundPostings(accts[payee.AccountCode], accts[feeCode], accts[destCode], cur, split)
		if err != nil {
			return err
		}
		payID := pay.ID
		e.RelatedEntryID = &payID
		return s.appendEntry(ctx, e, accts)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}
