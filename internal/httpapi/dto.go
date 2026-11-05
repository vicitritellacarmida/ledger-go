package httpapi

import (
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// All amounts on the wire are integers in minor units of the currency.
// Signed ledger amounts never appear in the API: postings carry a side
// ("debit"/"credit") and a positive amount, and balances are shown in the
// account's natural sign.

// AccountDTO is the JSON form of an account.
type AccountDTO struct {
	ID             int64      `json:"id"`
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	NormalBalance  string     `json:"normal_balance"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	OwnerRef       string     `json:"owner_ref,omitempty"`
	AllowOverdraft bool       `json:"allow_overdraft"`
	Balance        BalanceDTO `json:"balance"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// BalanceDTO shows balances in the account's natural sign.
type BalanceDTO struct {
	AccountCode     string `json:"account_code,omitempty"`
	Currency        string `json:"currency,omitempty"`
	AsOf            string `json:"as_of,omitempty"`
	Posted          int64  `json:"posted"`
	Held            *int64 `json:"held,omitempty"`
	Available       *int64 `json:"available,omitempty"`
	PostedFormatted string `json:"posted_formatted"`
}

func formatAmount(currency string, amount int64) string {
	c, err := domain.LookupCurrency(currency)
	if err != nil {
		return ""
	}
	return c.Format(amount)
}

func toAccountDTO(a *domain.Account) AccountDTO {
	held, avail := a.HeldAmount, a.AvailableBalance()
	return AccountDTO{
		ID: a.ID, Code: a.Code, Name: a.Name, Type: string(a.Type),
		NormalBalance: string(a.NormalBalance()), Currency: a.Currency, Status: string(a.Status),
		OwnerRef: a.OwnerRef, AllowOverdraft: a.AllowOverdraft,
		Balance: BalanceDTO{
			Posted: a.NaturalBalance(), Held: &held, Available: &avail,
			PostedFormatted: formatAmount(a.Currency, a.NaturalBalance()),
		},
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func toBalanceDTO(b *service.Balance) BalanceDTO {
	out := BalanceDTO{
		AccountCode: b.Account.Code, Currency: b.Account.Currency,
		Posted: b.Posted, Held: b.Held, Available: b.Available,
		PostedFormatted: formatAmount(b.Account.Currency, b.Posted),
	}
	if b.AsOf != nil {
		out.AsOf = domain.FormatDate(*b.AsOf)
	}
	return out
}

// PostingDTO is one line of an entry.
type PostingDTO struct {
	Line        int    `json:"line"`
	AccountCode string `json:"account_code"`
	Side        string `json:"side"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Role        string `json:"role,omitempty"`
}

// EntryDTO is the JSON form of a journal entry.
type EntryDTO struct {
	ID             int64        `json:"id"`
	Seq            int64        `json:"seq"`
	Type           string       `json:"type"`
	Description    string       `json:"description"`
	ExternalRef    string       `json:"external_ref,omitempty"`
	EffectiveDate  string       `json:"effective_date"`
	ReversalOf     *int64       `json:"reversal_of,omitempty"`
	ReversedBy     *int64       `json:"reversed_by,omitempty"`
	RelatedEntryID *int64       `json:"related_entry_id,omitempty"`
	HoldID         *int64       `json:"hold_id,omitempty"`
	Postings       []PostingDTO `json:"postings"`
	PrevHash       string       `json:"prev_hash"`
	Hash           string       `json:"hash"`
	CreatedAt      time.Time    `json:"created_at"`
}

func toEntryDTO(e *domain.Entry) EntryDTO {
	out := EntryDTO{
		ID: e.ID, Seq: e.Seq, Type: string(e.Type), Description: e.Description, ExternalRef: e.ExternalRef,
		EffectiveDate: domain.FormatDate(e.EffectiveDate), ReversalOf: e.ReversalOf,
		RelatedEntryID: e.RelatedEntryID, HoldID: e.HoldID,
		Postings: make([]PostingDTO, len(e.Postings)), PrevHash: e.PrevHash, Hash: e.Hash, CreatedAt: e.CreatedAt,
	}
	for i, p := range e.Postings {
		side, mag := p.Side()
		out.Postings[i] = PostingDTO{
			Line: p.LineNo, AccountCode: p.AccountCode, Side: string(side), Amount: mag, Currency: p.Currency, Role: p.Role,
		}
	}
	return out
}

// HoldDTO is the JSON form of a hold.
type HoldDTO struct {
	ID                     int64     `json:"id"`
	AccountCode            string    `json:"account_code"`
	DestinationAccountCode string    `json:"destination_account_code"`
	Currency               string    `json:"currency"`
	Amount                 int64     `json:"amount"`
	CapturedAmount         int64     `json:"captured_amount"`
	RemainingAmount        int64     `json:"remaining_amount"`
	State                  string    `json:"state"`
	Description            string    `json:"description,omitempty"`
	ExternalRef            string    `json:"external_ref,omitempty"`
	ExpiresAt              time.Time `json:"expires_at"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

func toHoldDTO(h *domain.Hold) HoldDTO {
	return HoldDTO{
		ID: h.ID, AccountCode: h.AccountCode, DestinationAccountCode: h.DestinationAccountCode,
		Currency: h.Currency, Amount: h.Amount, CapturedAmount: h.CapturedAmount, RemainingAmount: h.Remaining(),
		State: string(h.State), Description: h.Description, ExternalRef: h.ExternalRef,
		ExpiresAt: h.ExpiresAt, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
	}
}

// StatementDTO is one page of an account statement.
type StatementDTO struct {
	AccountCode    string             `json:"account_code"`
	Currency       string             `json:"currency"`
	NormalBalance  string             `json:"normal_balance"`
	OpeningBalance int64              `json:"opening_balance"`
	ClosingBalance int64              `json:"closing_balance"`
	Lines          []StatementLineDTO `json:"lines"`
	NextCursor     string             `json:"next_cursor,omitempty"`
}

// StatementLineDTO is one posting on a statement.
type StatementLineDTO struct {
	PostingID      int64     `json:"posting_id"`
	EntryID        int64     `json:"entry_id"`
	EntryType      string    `json:"entry_type"`
	Description    string    `json:"description"`
	ExternalRef    string    `json:"external_ref,omitempty"`
	EffectiveDate  string    `json:"effective_date"`
	Side           string    `json:"side"`
	Amount         int64     `json:"amount"`
	RunningBalance int64     `json:"running_balance"`
	CreatedAt      time.Time `json:"created_at"`
}

func toStatementDTO(st *service.Statement) StatementDTO {
	out := StatementDTO{
		AccountCode: st.Account.Code, Currency: st.Account.Currency,
		NormalBalance:  string(st.Account.NormalBalance()),
		OpeningBalance: st.OpeningBalance, ClosingBalance: st.ClosingBalance,
		Lines: make([]StatementLineDTO, len(st.Lines)), NextCursor: st.NextCursor,
	}
	for i, l := range st.Lines {
		out.Lines[i] = StatementLineDTO{
			PostingID: l.PostingID, EntryID: l.EntryID, EntryType: string(l.EntryType),
			Description: l.Description, ExternalRef: l.ExternalRef, EffectiveDate: domain.FormatDate(l.EffectiveDate),
			Side: string(l.Side), Amount: l.Amount, RunningBalance: l.RunningBalance, CreatedAt: l.CreatedAt,
		}
	}
	return out
}

// TrialBalanceDTO is the trial balance report.
type TrialBalanceDTO struct {
	AsOf       string                    `json:"as_of"`
	Balanced   bool                      `json:"balanced"`
	Currencies []TrialBalanceCurrencyDTO `json:"currencies"`
}

// TrialBalanceCurrencyDTO is the trial balance of one currency.
type TrialBalanceCurrencyDTO struct {
	Currency    string                `json:"currency"`
	TotalDebit  int64                 `json:"total_debit"`
	TotalCredit int64                 `json:"total_credit"`
	Balanced    bool                  `json:"balanced"`
	Lines       []TrialBalanceLineDTO `json:"lines"`
}

// TrialBalanceLineDTO is one account on the trial balance.
type TrialBalanceLineDTO struct {
	AccountCode string `json:"account_code"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Debit       int64  `json:"debit"`
	Credit      int64  `json:"credit"`
}

func toTrialBalanceDTO(tb *service.TrialBalance) TrialBalanceDTO {
	out := TrialBalanceDTO{AsOf: domain.FormatDate(tb.AsOf), Balanced: tb.Balanced,
		Currencies: make([]TrialBalanceCurrencyDTO, len(tb.Currencies))}
	for i, c := range tb.Currencies {
		cd := TrialBalanceCurrencyDTO{Currency: c.Currency, TotalDebit: c.TotalDebit, TotalCredit: c.TotalCredit,
			Balanced: c.Balanced, Lines: make([]TrialBalanceLineDTO, len(c.Lines))}
		for j, l := range c.Lines {
			cd.Lines[j] = TrialBalanceLineDTO{AccountCode: l.Account.Code, Name: l.Account.Name,
				Type: string(l.Account.Type), Debit: l.Debit, Credit: l.Credit}
		}
		out.Currencies[i] = cd
	}
	return out
}

// PeriodDTO is a closed accounting period.
type PeriodDTO struct {
	Period   string    `json:"period"`
	Status   string    `json:"status"`
	ClosedAt time.Time `json:"closed_at"`
}

func toPeriodDTO(p store.ClosedPeriod) PeriodDTO {
	return PeriodDTO{Period: p.Period, Status: "closed", ClosedAt: p.ClosedAt}
}

// ChainReportDTO is the result of the audit chain verification.
type ChainReportDTO struct {
	OK          bool           `json:"ok"`
	Checked     int64          `json:"checked_entries"`
	HeadSeq     int64          `json:"head_seq"`
	FirstBroken *ChainBreakDTO `json:"first_broken,omitempty"`
}

// ChainBreakDTO describes the first broken entry.
type ChainBreakDTO struct {
	EntryID int64  `json:"entry_id,omitempty"`
	Seq     int64  `json:"seq"`
	Reason  string `json:"reason"`
}

func toChainReportDTO(r *service.ChainReport) ChainReportDTO {
	out := ChainReportDTO{OK: r.OK, Checked: r.Checked, HeadSeq: r.HeadSeq}
	if r.FirstBroken != nil {
		out.FirstBroken = &ChainBreakDTO{EntryID: r.FirstBroken.EntryID, Seq: r.FirstBroken.Seq, Reason: r.FirstBroken.Reason}
	}
	return out
}

// ListResponse wraps a page of results.
type ListResponse[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
}
