// Package domain holds the pure business rules of the ledger: accounts,
// journal entries, holds, periods, money arithmetic and the audit hash chain.
// It has no knowledge of HTTP or the database, so every rule here can be unit
// tested without infrastructure.
package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifies a domain error so outer layers can map it to a
// transport-level status without knowing every individual error code.
type ErrorKind int

// Error kinds. The HTTP layer maps them to status codes.
const (
	// KindValidation means the input is malformed or out of range.
	KindValidation ErrorKind = iota + 1
	// KindNotFound means a referenced resource does not exist.
	KindNotFound
	// KindConflict means the request conflicts with the current state
	// (duplicate code, invalid state transition, already reversed).
	KindConflict
	// KindRule means the input is well formed but a business rule rejects it
	// (insufficient funds, closed period, frozen account).
	KindRule
)

// Stable, machine-readable error codes. Clients may branch on these values,
// so they must not change once released.
const (
	CodeValidation           = "validation_failed"
	CodeNotFound             = "not_found"
	CodeAccountNotFound      = "account_not_found"
	CodeEntryNotFound        = "entry_not_found"
	CodeHoldNotFound         = "hold_not_found"
	CodeAlreadyExists        = "already_exists"
	CodeUnbalancedEntry      = "unbalanced_entry"
	CodeCurrencyMismatch     = "currency_mismatch"
	CodeUnknownCurrency      = "unknown_currency"
	CodeInsufficientFunds    = "insufficient_funds"
	CodeAccountNotActive     = "account_not_active"
	CodeAccountNotEmpty      = "account_not_empty"
	CodePeriodClosed         = "period_closed"
	CodePeriodNotEnded       = "period_not_ended"
	CodeFutureEffectiveDate  = "effective_date_in_future"
	CodeInvalidTransition    = "invalid_state_transition"
	CodeAlreadyReversed      = "entry_already_reversed"
	CodeNotReversible        = "entry_not_reversible"
	CodeHoldExpired          = "hold_expired"
	CodeCaptureExceedsHold   = "capture_exceeds_hold"
	CodeRefundExceedsPayment = "refund_exceeds_payment"
	CodeNotAPayment          = "entry_not_a_payment"
	CodeAmountOverflow       = "amount_overflow"
)

// Error is the single error type produced by business rules.
type Error struct {
	Kind    ErrorKind
	Code    string
	Message string
	// Details carries optional structured context, for example the offending
	// field name or the shortfall of an insufficient funds error.
	Details map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Is reports whether target is a *Error with the same code. This lets callers
// write errors.Is(err, &domain.Error{Code: domain.CodeInsufficientFunds}).
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// WithDetail returns a copy of e with one more detail entry.
func (e *Error) WithDetail(key string, value any) *Error {
	cp := *e
	cp.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		cp.Details[k] = v
	}
	cp.Details[key] = value
	return &cp
}

// NewError builds an *Error with a formatted message.
func NewError(kind ErrorKind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Invalid builds a validation error that names the offending field.
func Invalid(field, format string, args ...any) *Error {
	return NewError(KindValidation, CodeValidation, format, args...).WithDetail("field", field)
}

// ErrorCode returns the domain code of err, or "" if err is not a domain error.
func ErrorCode(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// HasCode reports whether err is a domain error with the given code.
func HasCode(err error, code string) bool { return ErrorCode(err) == code }
