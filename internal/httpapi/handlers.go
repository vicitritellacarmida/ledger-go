package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
)

// ---- request helpers --------------------------------------------------------

func optionalDate(field, v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	d, err := domain.ParseDate(field, v)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func queryInt(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, domain.Invalid(name, "%s must be a non-negative integer", name)
	}
	return n, nil
}

func pathID(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, domain.Invalid(name, "%s must be a positive integer", name)
	}
	return n, nil
}

func cursorString(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// meta is embedded in every transfer request.
type meta struct {
	Description   string `json:"description"`
	ExternalRef   string `json:"external_ref"`
	EffectiveDate string `json:"effective_date"`
}

func (m meta) toService() (service.TransferMeta, error) {
	d, err := optionalDate("effective_date", m.EffectiveDate)
	if err != nil {
		return service.TransferMeta{}, err
	}
	return service.TransferMeta{Description: m.Description, ExternalRef: m.ExternalRef, EffectiveDate: d}, nil
}

// ---- currencies -------------------------------------------------------------

type currencyDTO struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Exponent int    `json:"exponent"`
}

func (a *API) listCurrencies(w http.ResponseWriter, _ *http.Request) {
	cs := domain.SupportedCurrencies()
	out := make([]currencyDTO, len(cs))
	for i, c := range cs {
		out[i] = currencyDTO{Code: c.Code, Name: c.Name, Exponent: c.Exponent}
	}
	writeJSON(w, http.StatusOK, ListResponse[currencyDTO]{Data: out})
}

// ---- accounts ---------------------------------------------------------------

type createAccountRequest struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Currency       string `json:"currency"`
	OwnerRef       string `json:"owner_ref"`
	AllowOverdraft bool   `json:"allow_overdraft"`
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	acct, err := a.svc.CreateAccount(r.Context(), service.CreateAccountInput{
		Code: req.Code, Name: req.Name, Type: domain.AccountType(req.Type),
		Currency: strings.ToUpper(req.Currency), OwnerRef: req.OwnerRef, AllowOverdraft: req.AllowOverdraft,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAccountDTO(acct))
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := queryInt(r, "cursor")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	accts, next, err := a.svc.ListAccounts(r.Context(), service.ListAccountsInput{
		Type: domain.AccountType(q.Get("type")), Currency: strings.ToUpper(q.Get("currency")),
		Status: domain.AccountStatus(q.Get("status")), OwnerRef: q.Get("owner_ref"),
		CodePrefix: q.Get("code_prefix"), AfterID: after, Limit: int(limit),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := ListResponse[AccountDTO]{Data: make([]AccountDTO, len(accts)), NextCursor: cursorString(next)}
	for i, acct := range accts {
		out.Data[i] = toAccountDTO(acct)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getAccount(w http.ResponseWriter, r *http.Request) {
	acct, err := a.svc.GetAccount(r.Context(), r.PathValue("code"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountDTO(acct))
}

type changeStatusRequest struct {
	Status string `json:"status"`
}

func (a *API) changeAccountStatus(w http.ResponseWriter, r *http.Request) {
	var req changeStatusRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	acct, err := a.svc.ChangeAccountStatus(r.Context(), r.PathValue("code"), domain.AccountStatus(req.Status))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountDTO(acct))
}

func (a *API) getBalance(w http.ResponseWriter, r *http.Request) {
	asOf, err := optionalDate("as_of", r.URL.Query().Get("as_of"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	b, err := a.svc.GetBalance(r.Context(), r.PathValue("code"), asOf)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBalanceDTO(b))
}

func (a *API) getStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := optionalDate("from", q.Get("from"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	to, err := optionalDate("to", q.Get("to"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if from != nil && to != nil && to.Before(*from) {
		a.fail(w, r, domain.Invalid("to", "to must not be before from"))
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	st, err := a.svc.Statement(r.Context(), r.PathValue("code"), service.StatementInput{
		From: from, To: to, Cursor: q.Get("cursor"), Limit: int(limit),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toStatementDTO(st))
}

func (a *API) listAccountHolds(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	openOnly := r.URL.Query().Get("state") == "open"
	holds, err := a.svc.ListAccountHolds(r.Context(), r.PathValue("code"), openOnly, int(limit))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := ListResponse[HoldDTO]{Data: make([]HoldDTO, len(holds))}
	for i, h := range holds {
		out.Data[i] = toHoldDTO(h)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- entries ----------------------------------------------------------------

type postingRequest struct {
	AccountCode string `json:"account_code"`
	Side        string `json:"side"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

type postEntryRequest struct {
	Type          string           `json:"type"`
	Description   string           `json:"description"`
	ExternalRef   string           `json:"external_ref"`
	EffectiveDate string           `json:"effective_date"`
	Postings      []postingRequest `json:"postings"`
}

func (a *API) postEntry(w http.ResponseWriter, r *http.Request) {
	var req postEntryRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	date, err := optionalDate("effective_date", req.EffectiveDate)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	in := service.PostEntryInput{
		Type: domain.EntryType(req.Type), Description: req.Description, ExternalRef: req.ExternalRef,
		EffectiveDate: date, Postings: make([]service.PostingInput, len(req.Postings)),
	}
	for i, p := range req.Postings {
		in.Postings[i] = service.PostingInput{
			AccountCode: p.AccountCode, Side: domain.Side(p.Side), Amount: p.Amount, Currency: strings.ToUpper(p.Currency),
		}
	}
	e, err := a.svc.PostEntry(r.Context(), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toEntryDTO(e))
}

func (a *API) getEntry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := a.svc.GetEntryView(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	dto := toEntryDTO(v.Entry)
	dto.ReversedBy = v.ReversedBy
	writeJSON(w, http.StatusOK, dto)
}

func (a *API) listEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, err := queryInt(r, "cursor")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	entries, next, err := a.svc.ListEntries(r.Context(), service.ListEntriesInput{
		Type: domain.EntryType(q.Get("type")), ExternalRef: q.Get("external_ref"), BeforeID: before, Limit: int(limit),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := ListResponse[EntryDTO]{Data: make([]EntryDTO, len(entries)), NextCursor: cursorString(next)}
	for i, e := range entries {
		out.Data[i] = toEntryDTO(e)
	}
	writeJSON(w, http.StatusOK, out)
}

type reverseRequest struct {
	EffectiveDate string `json:"effective_date"`
	Description   string `json:"description"`
}

func (a *API) reverseEntry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req reverseRequest
	if err := decode(r, &req, true); err != nil {
		a.fail(w, r, err)
		return
	}
	date, err := optionalDate("effective_date", req.EffectiveDate)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.svc.ReverseEntry(r.Context(), id, service.ReverseInput{EffectiveDate: date, Description: req.Description})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toEntryDTO(e))
}

// ---- holds ------------------------------------------------------------------

type placeHoldRequest struct {
	AccountCode            string     `json:"account_code"`
	DestinationAccountCode string     `json:"destination_account_code"`
	Amount                 int64      `json:"amount"`
	Currency               string     `json:"currency"`
	ExpiresAt              *time.Time `json:"expires_at"`
	TTLSeconds             int64      `json:"ttl_seconds"`
	Description            string     `json:"description"`
	ExternalRef            string     `json:"external_ref"`
}

func (a *API) placeHold(w http.ResponseWriter, r *http.Request) {
	var req placeHoldRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.ExpiresAt != nil && req.TTLSeconds != 0 {
		a.fail(w, r, domain.Invalid("ttl_seconds", "give expires_at or ttl_seconds, not both"))
		return
	}
	if req.TTLSeconds < 0 {
		a.fail(w, r, domain.Invalid("ttl_seconds", "ttl_seconds must be positive"))
		return
	}
	h, err := a.svc.PlaceHold(r.Context(), service.PlaceHoldInput{
		AccountCode: req.AccountCode, DestinationAccountCode: req.DestinationAccountCode,
		Amount: req.Amount, Currency: strings.ToUpper(req.Currency),
		ExpiresAt: req.ExpiresAt, TTL: time.Duration(req.TTLSeconds) * time.Second,
		Description: req.Description, ExternalRef: req.ExternalRef,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toHoldDTO(h))
}

func (a *API) getHold(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	h, err := a.svc.GetHold(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toHoldDTO(h))
}

type captureRequest struct {
	Amount        *int64 `json:"amount"`
	Final         bool   `json:"final"`
	EffectiveDate string `json:"effective_date"`
	Description   string `json:"description"`
}

type captureResponse struct {
	Hold  HoldDTO  `json:"hold"`
	Entry EntryDTO `json:"entry"`
}

func (a *API) captureHold(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req captureRequest
	if err := decode(r, &req, true); err != nil {
		a.fail(w, r, err)
		return
	}
	date, err := optionalDate("effective_date", req.EffectiveDate)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.svc.CaptureHold(r.Context(), id, service.CaptureInput{
		Amount: req.Amount, Final: req.Final, EffectiveDate: date, Description: req.Description,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, captureResponse{Hold: toHoldDTO(res.Hold), Entry: toEntryDTO(res.Entry)})
}

func (a *API) voidHold(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	h, err := a.svc.VoidHold(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toHoldDTO(h))
}

type expireRequest struct {
	Limit int `json:"limit"`
}

func (a *API) expireHolds(w http.ResponseWriter, r *http.Request) {
	var req expireRequest
	if err := decode(r, &req, true); err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := a.svc.ExpireDueHolds(r.Context(), req.Limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"expired": n})
}

// ---- transfers --------------------------------------------------------------

type topupRequest struct {
	WalletAccountCode string `json:"wallet_account_code"`
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	meta
}

func (a *API) topup(w http.ResponseWriter, r *http.Request) {
	var req topupRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	m, err := req.toService()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.svc.Topup(r.Context(), service.TopupInput{
		WalletAccountCode: req.WalletAccountCode, Amount: req.Amount, Currency: strings.ToUpper(req.Currency), TransferMeta: m,
	})
	a.respondEntry(w, r, e, err)
}

type paymentRequest struct {
	WalletAccountCode   string `json:"wallet_account_code"`
	MerchantAccountCode string `json:"merchant_account_code"`
	Amount              int64  `json:"amount"`
	Currency            string `json:"currency"`
	FeeAmount           *int64 `json:"fee_amount"`
	FeeBps              *int64 `json:"fee_bps"`
	meta
}

func (a *API) payment(w http.ResponseWriter, r *http.Request) {
	var req paymentRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	m, err := req.toService()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.svc.Payment(r.Context(), service.PaymentInput{
		WalletAccountCode: req.WalletAccountCode, MerchantAccountCode: req.MerchantAccountCode,
		Amount: req.Amount, Currency: strings.ToUpper(req.Currency), FeeAmount: req.FeeAmount, FeeBps: req.FeeBps,
		TransferMeta: m,
	})
	a.respondEntry(w, r, e, err)
}

type payoutRequest struct {
	MerchantAccountCode string `json:"merchant_account_code"`
	Amount              int64  `json:"amount"`
	Currency            string `json:"currency"`
	meta
}

func (a *API) payout(w http.ResponseWriter, r *http.Request) {
	var req payoutRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	m, err := req.toService()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.svc.Payout(r.Context(), service.PayoutInput{
		MerchantAccountCode: req.MerchantAccountCode, Amount: req.Amount, Currency: strings.ToUpper(req.Currency), TransferMeta: m,
	})
	a.respondEntry(w, r, e, err)
}

type refundRequest struct {
	PaymentEntryID int64  `json:"payment_entry_id"`
	Amount         *int64 `json:"amount"`
	RefundFee      bool   `json:"refund_fee"`
	Destination    string `json:"destination"`
	meta
}

func (a *API) refund(w http.ResponseWriter, r *http.Request) {
	var req refundRequest
	if err := decode(r, &req, false); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.PaymentEntryID <= 0 {
		a.fail(w, r, domain.Invalid("payment_entry_id", "payment_entry_id is required"))
		return
	}
	m, err := req.toService()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	e, err := a.svc.Refund(r.Context(), service.RefundInput{
		PaymentEntryID: req.PaymentEntryID, Amount: req.Amount, RefundFee: req.RefundFee,
		Destination: req.Destination, TransferMeta: m,
	})
	a.respondEntry(w, r, e, err)
}

func (a *API) respondEntry(w http.ResponseWriter, r *http.Request, e *domain.Entry, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toEntryDTO(e))
}

// ---- reports, periods, audit ------------------------------------------------

func (a *API) trialBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	asOf, err := optionalDate("as_of", q.Get("as_of"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	tb, err := a.svc.TrialBalance(r.Context(), asOf, strings.ToUpper(q.Get("currency")))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTrialBalanceDTO(tb))
}

func (a *API) listPeriods(w http.ResponseWriter, r *http.Request) {
	ps, err := a.svc.ListClosedPeriods(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := ListResponse[PeriodDTO]{Data: make([]PeriodDTO, len(ps))}
	for i, p := range ps {
		out.Data[i] = toPeriodDTO(p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) closePeriod(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.ClosePeriod(r.Context(), r.PathValue("period"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPeriodDTO(*p))
}

func (a *API) verifyChain(w http.ResponseWriter, r *http.Request) {
	rep, err := a.svc.VerifyChain(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toChainReportDTO(rep))
}
