package service

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// TrialBalanceLine is one account on the trial balance. Exactly one of Debit
// and Credit is non-zero (or both are zero for a settled account).
type TrialBalanceLine struct {
	Account *domain.Account
	Debit   int64
	Credit  int64
}

// TrialBalanceCurrency groups the trial balance of one currency. Amounts in
// different currencies are never added together.
type TrialBalanceCurrency struct {
	Currency    string
	Lines       []TrialBalanceLine
	TotalDebit  int64
	TotalCredit int64
	Balanced    bool
}

// TrialBalance lists every account's net debit or credit as of a date.
//
// If every entry balanced, total debits equal total credits in each
// currency. A false Balanced flag therefore means corrupted data, never a
// normal business situation.
type TrialBalance struct {
	AsOf       time.Time
	Currencies []TrialBalanceCurrency
	Balanced   bool
}

// TrialBalance builds the trial balance as of asOf (default today).
func (s *Service) TrialBalance(ctx context.Context, asOf *time.Time, currency string) (*TrialBalance, error) {
	date := s.Today()
	if asOf != nil {
		date = *asOf
	}
	if currency != "" {
		if err := checkCurrency(currency); err != nil {
			return nil, err
		}
	}
	rows, err := s.store.TrialBalance(ctx, date, currency)
	if err != nil {
		return nil, err
	}
	tb := &TrialBalance{AsOf: date, Balanced: true}
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.Account.Currency]
		if !ok {
			tb.Currencies = append(tb.Currencies, TrialBalanceCurrency{Currency: r.Account.Currency})
			i = len(tb.Currencies) - 1
			idx[r.Account.Currency] = i
		}
		g := &tb.Currencies[i]
		line := TrialBalanceLine{Account: r.Account}
		if r.Signed >= 0 {
			line.Debit = r.Signed
		} else {
			line.Credit = -r.Signed
		}
		g.TotalDebit += line.Debit
		g.TotalCredit += line.Credit
		g.Lines = append(g.Lines, line)
	}
	for i := range tb.Currencies {
		g := &tb.Currencies[i]
		g.Balanced = g.TotalDebit == g.TotalCredit
		tb.Balanced = tb.Balanced && g.Balanced
	}
	return tb, nil
}

// StatementInput selects a statement page.
type StatementInput struct {
	From, To *time.Time
	Cursor   string
	Limit    int
}

// StatementLine is a posting with the running balance after it, both in
// the account's natural sign.
type StatementLine struct {
	PostingID      int64
	EntryID        int64
	EntryType      domain.EntryType
	Description    string
	ExternalRef    string
	EffectiveDate  time.Time
	Side           domain.Side
	Amount         int64
	RunningBalance int64
	CreatedAt      time.Time
}

// Statement is one page of an account statement.
type Statement struct {
	Account        *domain.Account
	OpeningBalance int64
	ClosingBalance int64
	Lines          []StatementLine
	NextCursor     string
}

// Statement returns postings of an account ordered by effective date then
// posting id, with a running balance. The cursor is opaque to clients; it
// encodes the position of the last line so the next page continues there,
// even if new postings arrive in between.
func (s *Service) Statement(ctx context.Context, code string, in StatementInput) (*Statement, error) {
	a, err := s.store.GetAccountByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	var after *store.StatementPosition
	if in.Cursor != "" {
		pos, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, err
		}
		after = &pos
	}
	limit := clampLimit(in.Limit, 100, 500)
	rows, err := s.store.StatementRows(ctx, store.StatementQuery{
		AccountID: a.ID, From: in.From, To: in.To, After: after, Limit: limit + 1,
	})
	if err != nil {
		return nil, err
	}
	st := &Statement{Account: a}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	// Opening balance: everything before the first line of this page.
	var start store.StatementPosition
	switch {
	case len(rows) > 0:
		start = store.StatementPosition{Date: rows[0].EffectiveDate, PostingID: rows[0].PostingID}
	case after != nil:
		start = store.StatementPosition{Date: after.Date, PostingID: after.PostingID + 1}
	case in.From != nil:
		start = store.StatementPosition{Date: *in.From}
	default:
		start = store.StatementPosition{Date: farFuture}
	}
	opening, err := s.store.SumBefore(ctx, a.ID, start)
	if err != nil {
		return nil, err
	}
	st.OpeningBalance = a.Natural(opening)
	running := st.OpeningBalance
	for _, r := range rows {
		natural := a.Natural(r.Amount)
		running += natural
		side, mag := domain.SideOf(r.Amount)
		st.Lines = append(st.Lines, StatementLine{
			PostingID: r.PostingID, EntryID: r.EntryID, EntryType: r.EntryType,
			Description: r.Description, ExternalRef: r.ExternalRef, EffectiveDate: r.EffectiveDate,
			Side: side, Amount: mag, RunningBalance: running, CreatedAt: r.CreatedAt,
		})
	}
	st.ClosingBalance = running
	if hasMore {
		last := rows[len(rows)-1]
		st.NextCursor = encodeCursor(store.StatementPosition{Date: last.EffectiveDate, PostingID: last.PostingID})
	}
	return st, nil
}

var farFuture = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

func encodeCursor(p store.StatementPosition) string {
	raw := domain.FormatDate(p.Date) + "|" + strconv.FormatInt(p.PostingID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(c string) (store.StatementPosition, error) {
	bad := domain.Invalid("cursor", "cursor is invalid")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return store.StatementPosition{}, bad
	}
	datePart, idPart, ok := strings.Cut(string(raw), "|")
	if !ok {
		return store.StatementPosition{}, bad
	}
	d, err := domain.ParseDate("cursor", datePart)
	if err != nil {
		return store.StatementPosition{}, bad
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return store.StatementPosition{}, bad
	}
	return store.StatementPosition{Date: d, PostingID: id}, nil
}
