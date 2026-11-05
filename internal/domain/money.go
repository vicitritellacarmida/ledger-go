package domain

import (
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// MaxAmount is the largest absolute amount, in minor units, that a single
// posting, hold or balance may hold. It equals 2^53-1 so that every amount is
// exactly representable as a JSON number in JavaScript clients.
const MaxAmount int64 = 1<<53 - 1

// Currency describes an ISO 4217 currency and how many digits follow the
// decimal point in its minor unit. IDR has exponent 0 (1 rupiah is the
// smallest unit we book), USD has exponent 2 (1 dollar = 100 cents).
type Currency struct {
	Code     string
	Name     string
	Exponent int
}

var currencies = map[string]Currency{
	"IDR": {Code: "IDR", Name: "Indonesian Rupiah", Exponent: 0},
	"USD": {Code: "USD", Name: "US Dollar", Exponent: 2},
	"SGD": {Code: "SGD", Name: "Singapore Dollar", Exponent: 2},
	"EUR": {Code: "EUR", Name: "Euro", Exponent: 2},
	"JPY": {Code: "JPY", Name: "Japanese Yen", Exponent: 0},
}

// LookupCurrency returns the currency for an upper-case ISO code.
func LookupCurrency(code string) (Currency, error) {
	c, ok := currencies[code]
	if !ok {
		return Currency{}, NewError(KindValidation, CodeUnknownCurrency, "currency %q is not supported", code).
			WithDetail("currency", code)
	}
	return c, nil
}

// SupportedCurrencies returns all known currencies sorted by code.
func SupportedCurrencies() []Currency {
	out := make([]Currency, 0, len(currencies))
	for _, c := range currencies {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Format renders an amount in minor units as a plain decimal string, for
// example 12345 USD becomes "123.45" and 12345 IDR stays "12345". It is meant
// for human display only; the API always transports integers.
func (c Currency) Format(amount int64) string {
	neg := amount < 0
	// Work on the unsigned magnitude so math.MinInt64 does not overflow.
	mag := uint64(amount)
	if neg {
		mag = uint64(-(amount + 1)) + 1
	}
	digits := strconv.FormatUint(mag, 10)
	if c.Exponent > 0 {
		if len(digits) <= c.Exponent {
			digits = strings.Repeat("0", c.Exponent-len(digits)+1) + digits
		}
		cut := len(digits) - c.Exponent
		digits = digits[:cut] + "." + digits[cut:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// CheckAmount validates a positive amount used in a request (hold amount,
// transfer amount, capture amount).
func CheckAmount(field string, amount int64) error {
	if amount <= 0 {
		return Invalid(field, "%s must be greater than zero", field)
	}
	if amount > MaxAmount {
		return Invalid(field, "%s must not exceed %d", field, MaxAmount)
	}
	return nil
}

// AddChecked adds two balances and reports an error when the result leaves
// the [-MaxAmount, MaxAmount] range.
func AddChecked(a, b int64) (int64, error) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) || s > MaxAmount || s < -MaxAmount {
		return 0, NewError(KindRule, CodeAmountOverflow, "balance would exceed the supported range")
	}
	return s, nil
}

// FeeFromBasisPoints computes amount * bps / 10000, rounded half up.
// One basis point is 0.01%, so 250 bps is a 2.5% fee.
func FeeFromBasisPoints(amount, bps int64) (int64, error) {
	if amount < 0 {
		return 0, Invalid("amount", "amount must not be negative")
	}
	if bps < 0 || bps > 10000 {
		return 0, Invalid("fee_bps", "fee_bps must be between 0 and 10000")
	}
	hi, lo := bits.Mul64(uint64(amount), uint64(bps))
	// Adding half of the divisor before dividing rounds half up.
	lo, carry := bits.Add64(lo, 5000, 0)
	hi += carry
	q, _ := bits.Div64(hi, lo, 10000)
	return int64(q), nil
}

// ProRata returns floor(total * part / whole) without intermediate overflow.
// It is used to split a fee proportionally on a partial refund. All inputs
// must be non-negative and part must not exceed whole.
func ProRata(total, part, whole int64) int64 {
	if whole <= 0 || total <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return total
	}
	hi, lo := bits.Mul64(uint64(total), uint64(part))
	q, _ := bits.Div64(hi, lo, uint64(whole))
	return int64(q)
}
