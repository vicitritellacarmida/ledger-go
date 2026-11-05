package domain

// This file builds the postings for the convenience transfer operations of a
// marketplace. Each builder returns balanced postings; the service layer then
// applies them under row locks like any other entry.
//
// Accounts used (see DefaultChart for the seeded codes):
//
//	platform bank        asset      money the platform holds at its bank
//	customer wallet      liability  money the platform owes a customer
//	merchant payable     liability  money the platform owes a merchant
//	fee revenue          revenue    fees the platform has earned
//	refunds payable      liability  refunds owed back to an outside payment method

func requireType(field string, a *Account, types ...AccountType) error {
	for _, t := range types {
		if a.Type == t {
			return nil
		}
	}
	return Invalid(field, "account %s has type %s, expected %v", a.Code, a.Type, types)
}

func requireSameCurrency(currency string, accts ...*Account) error {
	for _, a := range accts {
		if a.Currency != currency {
			return NewError(KindValidation, CodeCurrencyMismatch, "account %s is in %s, operation is in %s", a.Code, a.Currency, currency).
				WithDetail("account_code", a.Code)
		}
	}
	return nil
}

func line(a *Account, signed int64, role string) Posting {
	return Posting{AccountID: a.ID, AccountCode: a.Code, Currency: a.Currency, Amount: signed, Role: role}
}

// TopupPostings records money arriving at the bank for a customer wallet:
// Dr bank (asset grows), Cr wallet (what we owe the customer grows).
func TopupPostings(bank, wallet *Account, currency string, amount int64) ([]Posting, error) {
	if err := CheckAmount("amount", amount); err != nil {
		return nil, err
	}
	if err := requireType("wallet_account_code", wallet, AccountLiability); err != nil {
		return nil, err
	}
	if err := requireSameCurrency(currency, bank, wallet); err != nil {
		return nil, err
	}
	ps := []Posting{line(bank, amount, RoleFunding), line(wallet, -amount, RolePayee)}
	NumberPostings(ps)
	return ps, nil
}

// PaymentPostings records a customer paying a merchant from a wallet, with a
// platform fee kept out of the merchant's share:
//
//	Dr customer wallet  amount
//	Cr merchant payable amount - fee
//	Cr fee revenue      fee
func PaymentPostings(wallet, merchant, feeRevenue *Account, currency string, amount, fee int64) ([]Posting, error) {
	if err := CheckAmount("amount", amount); err != nil {
		return nil, err
	}
	if fee < 0 || fee >= amount {
		return nil, Invalid("fee_amount", "fee must be at least 0 and less than the payment amount")
	}
	if err := requireType("wallet_account_code", wallet, AccountLiability); err != nil {
		return nil, err
	}
	if err := requireType("merchant_account_code", merchant, AccountLiability); err != nil {
		return nil, err
	}
	if err := requireSameCurrency(currency, wallet, merchant, feeRevenue); err != nil {
		return nil, err
	}
	ps := []Posting{line(wallet, amount, RolePayer), line(merchant, -(amount - fee), RolePayee)}
	if fee > 0 {
		ps = append(ps, line(feeRevenue, -fee, RoleFee))
	}
	NumberPostings(ps)
	return ps, nil
}

// PayoutPostings records paying a merchant's balance out to their bank:
// Dr merchant payable (we owe less), Cr bank (we hold less).
func PayoutPostings(merchant, bank *Account, currency string, amount int64) ([]Posting, error) {
	if err := CheckAmount("amount", amount); err != nil {
		return nil, err
	}
	if err := requireType("merchant_account_code", merchant, AccountLiability); err != nil {
		return nil, err
	}
	if err := requireSameCurrency(currency, merchant, bank); err != nil {
		return nil, err
	}
	ps := []Posting{line(merchant, amount, RolePayer), line(bank, -amount, RoleFunding)}
	NumberPostings(ps)
	return ps, nil
}

// RefundSplit is how a refund amount is shared between merchant and platform.
type RefundSplit struct {
	FromMerchant int64
	FromFee      int64
}

// SplitRefund decides who pays for a refund of refundAmount out of a payment
// of paymentAmount that carried paymentFee. If refundFee is true, the
// platform gives back the same share of its fee (rounded down); otherwise
// the merchant pays for the whole refund.
func SplitRefund(paymentAmount, paymentFee, refundAmount int64, refundFee bool) RefundSplit {
	if !refundFee {
		return RefundSplit{FromMerchant: refundAmount}
	}
	feePart := ProRata(paymentFee, refundAmount, paymentAmount)
	return RefundSplit{FromMerchant: refundAmount - feePart, FromFee: feePart}
}

// RefundPostings records a refund of a payment:
//
//	Dr merchant payable     split.FromMerchant
//	Dr fee revenue          split.FromFee (when > 0)
//	Cr customer wallet or refunds payable  total
func RefundPostings(merchant, feeRevenue, destination *Account, currency string, split RefundSplit) ([]Posting, error) {
	total := split.FromMerchant + split.FromFee
	if err := CheckAmount("amount", total); err != nil {
		return nil, err
	}
	if err := requireType("destination", destination, AccountLiability); err != nil {
		return nil, err
	}
	if err := requireSameCurrency(currency, merchant, feeRevenue, destination); err != nil {
		return nil, err
	}
	ps := make([]Posting, 0, 3)
	if split.FromMerchant > 0 {
		ps = append(ps, line(merchant, split.FromMerchant, RolePayee))
	}
	if split.FromFee > 0 {
		ps = append(ps, line(feeRevenue, split.FromFee, RoleFee))
	}
	ps = append(ps, line(destination, -total, RolePayer))
	NumberPostings(ps)
	return ps, nil
}
