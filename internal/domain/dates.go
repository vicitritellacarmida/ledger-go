package domain

import (
	"fmt"
	"time"
)

// DateLayout is the wire format of calendar dates (effective dates, as-of
// dates).
const DateLayout = "2006-01-02"

// ParseDate parses a YYYY-MM-DD string into midnight UTC of that day.
// The ledger represents calendar dates as midnight UTC so that the time zone
// never shifts a date by one day.
func ParseDate(field, s string) (time.Time, error) {
	t, err := time.ParseInLocation(DateLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, Invalid(field, "%s must be a date in YYYY-MM-DD format", field)
	}
	return t, nil
}

// DateOf returns the calendar date of instant t as seen in loc, as midnight
// UTC. For example 2026-03-01T20:00Z is 2026-03-02 in Asia/Jakarta (UTC+7).
func DateOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// FormatDate renders a calendar date as YYYY-MM-DD.
func FormatDate(t time.Time) string { return t.UTC().Format(DateLayout) }

// Period is an accounting month in YYYY-MM form.
type Period struct {
	Year  int
	Month time.Month
}

// ParsePeriod parses "YYYY-MM".
func ParsePeriod(s string) (Period, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return Period{}, Invalid("period", "period must be in YYYY-MM format")
	}
	return Period{Year: t.Year(), Month: t.Month()}, nil
}

// PeriodOf returns the period that contains date.
func PeriodOf(date time.Time) Period {
	return Period{Year: date.Year(), Month: date.Month()}
}

// String renders the period as YYYY-MM.
func (p Period) String() string { return fmt.Sprintf("%04d-%02d", p.Year, int(p.Month)) }

// Start is the first day of the period.
func (p Period) Start() time.Time { return time.Date(p.Year, p.Month, 1, 0, 0, 0, 0, time.UTC) }

// End is the last day of the period.
func (p Period) End() time.Time { return p.Start().AddDate(0, 1, -1) }

// CanClose reports whether the period may be closed on the given business
// date: a month can only be closed after its last day has passed.
func (p Period) CanClose(today time.Time) error {
	if !today.After(p.End()) {
		return NewError(KindRule, CodePeriodNotEnded, "period %s has not ended yet", p).
			WithDetail("period", p.String())
	}
	return nil
}
