package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/httpapi"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
	"github.com/vicitritellacarmida/ledger-go/internal/testutil"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	n   int
	mu  sync.Mutex
}

func newClient(t *testing.T) *client {
	t.Helper()
	db := testutil.DB(t, "httpapi")
	st := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loc, _ := time.LoadLocation("Asia/Jakarta")
	svc := service.New(st, service.Options{Location: loc, Logger: logger, Currencies: []string{"IDR", "USD"}})
	if _, err := svc.EnsureChart(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(svc, st, httpapi.Options{Logger: logger}).Handler())
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var eb httpapi.ErrorBody
	r.decode(t, &eb)
	return eb.Error.Code
}

func (c *client) nextKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("test-key-%d-%d", time.Now().UnixNano(), c.n)
}

func (c *client) do(method, path, key string, body any) response {
	c.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = bytes.NewBufferString(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

// post sends a write request with a fresh idempotency key and checks the
// status.
func (c *client) post(path string, body any, wantStatus int) response {
	c.t.Helper()
	r := c.do(http.MethodPost, path, c.nextKey(), body)
	if r.status != wantStatus {
		c.t.Fatalf("POST %s: status %d, want %d: %s", path, r.status, wantStatus, r.body)
	}
	return r
}

func (c *client) get(path string, wantStatus int) response {
	c.t.Helper()
	r := c.do(http.MethodGet, path, "", nil)
	if r.status != wantStatus {
		c.t.Fatalf("GET %s: status %d, want %d: %s", path, r.status, wantStatus, r.body)
	}
	return r
}

func (c *client) createWallet(code string) {
	c.t.Helper()
	c.post("/v1/accounts", map[string]any{
		"code": code, "name": "Wallet " + code, "type": "liability", "currency": "IDR", "owner_ref": "cust-1",
	}, http.StatusCreated)
}

func TestEndToEndFlow(t *testing.T) {
	c := newClient(t)
	c.createWallet("liabilities:wallets:cust-1:idr")
	c.post("/v1/accounts", map[string]any{
		"code": "liabilities:merchants:m-1:idr", "name": "Merchant", "type": "liability", "currency": "idr",
	}, http.StatusCreated)

	c.post("/v1/transfers/topup", map[string]any{
		"wallet_account_code": "liabilities:wallets:cust-1:idr", "amount": 200000, "currency": "IDR",
		"external_ref": "bank-transfer-1",
	}, http.StatusCreated)

	var pay httpapi.EntryDTO
	c.post("/v1/transfers/payment", map[string]any{
		"wallet_account_code": "liabilities:wallets:cust-1:idr", "merchant_account_code": "liabilities:merchants:m-1:idr",
		"amount": 100000, "currency": "IDR", "fee_bps": 200,
	}, http.StatusCreated).decode(t, &pay)
	if len(pay.Postings) != 3 || pay.Postings[0].Side != "debit" || pay.Postings[2].Amount != 2000 {
		t.Fatalf("payment = %+v", pay)
	}

	var acct httpapi.AccountDTO
	c.get("/v1/accounts/liabilities:wallets:cust-1:idr", http.StatusOK).decode(t, &acct)
	if acct.NormalBalance != "credit" || acct.Balance.Posted != 100000 || *acct.Balance.Available != 100000 {
		t.Fatalf("account = %+v", acct)
	}

	var hold httpapi.HoldDTO
	c.post("/v1/holds", map[string]any{
		"account_code": "liabilities:wallets:cust-1:idr", "destination_account_code": "liabilities:merchants:m-1:idr",
		"amount": 30000, "ttl_seconds": 600,
	}, http.StatusCreated).decode(t, &hold)
	var bal httpapi.BalanceDTO
	c.get("/v1/accounts/liabilities:wallets:cust-1:idr/balance", http.StatusOK).decode(t, &bal)
	if bal.Posted != 100000 || *bal.Held != 30000 || *bal.Available != 70000 {
		t.Fatalf("balance with hold = %+v", bal)
	}
	var capRes struct {
		Hold  httpapi.HoldDTO  `json:"hold"`
		Entry httpapi.EntryDTO `json:"entry"`
	}
	c.post(fmt.Sprintf("/v1/holds/%d/capture", hold.ID), map[string]any{"amount": 10000, "final": true}, http.StatusOK).decode(t, &capRes)
	if capRes.Hold.State != "captured" || capRes.Entry.Type != "hold_capture" {
		t.Fatalf("capture = %+v", capRes)
	}

	c.post("/v1/transfers/refund", map[string]any{"payment_entry_id": pay.ID, "amount": 5000}, http.StatusCreated)

	var st httpapi.StatementDTO
	c.get("/v1/accounts/liabilities:wallets:cust-1:idr/statement?limit=10", http.StatusOK).decode(t, &st)
	if len(st.Lines) != 4 || st.ClosingBalance != 95000 {
		t.Fatalf("statement = %+v", st)
	}

	var tb httpapi.TrialBalanceDTO
	c.get("/v1/reports/trial-balance", http.StatusOK).decode(t, &tb)
	if !tb.Balanced {
		t.Fatalf("trial balance = %+v", tb)
	}

	var rep httpapi.ChainReportDTO
	c.get("/v1/audit/verify", http.StatusOK).decode(t, &rep)
	if !rep.OK || rep.Checked != 4 {
		t.Fatalf("audit = %+v", rep)
	}

	// A payment with refunds cannot be reversed; the refunds come first.
	r := c.post(fmt.Sprintf("/v1/entries/%d/reverse", pay.ID), nil, http.StatusUnprocessableEntity)
	if r.errorCode(t) != "entry_not_reversible" {
		t.Fatalf("reverse refunded payment: %s", r.body)
	}

	var list httpapi.ListResponse[httpapi.EntryDTO]
	c.get("/v1/entries?limit=2", http.StatusOK).decode(t, &list)
	if len(list.Data) != 2 || list.NextCursor == "" {
		t.Fatalf("entries page = %+v", list)
	}
}

func TestErrorResponses(t *testing.T) {
	c := newClient(t)

	r := c.do(http.MethodPost, "/v1/accounts", "", map[string]any{"code": "x"})
	if r.status != http.StatusBadRequest || r.errorCode(t) != "idempotency_key_required" {
		t.Fatalf("missing key: %d %s", r.status, r.body)
	}
	r = c.post("/v1/accounts", `{"code": "abc", "nmae": "typo"}`, http.StatusBadRequest)
	if r.errorCode(t) != "invalid_json" {
		t.Fatalf("unknown field: %s", r.body)
	}
	r = c.post("/v1/accounts", `{"code": `, http.StatusBadRequest)
	if r.errorCode(t) != "invalid_json" {
		t.Fatalf("malformed: %s", r.body)
	}
	r = c.post("/v1/accounts", map[string]any{"code": "ok-code", "name": "n", "type": "cash", "currency": "IDR"}, http.StatusBadRequest)
	var eb httpapi.ErrorBody
	r.decode(t, &eb)
	if eb.Error.Code != "validation_failed" || eb.Error.Details["field"] != "type" || eb.RequestID == "" {
		t.Fatalf("validation body: %s", r.body)
	}
	r = c.get("/v1/accounts/nope", http.StatusNotFound)
	if r.errorCode(t) != "account_not_found" {
		t.Fatalf("not found: %s", r.body)
	}
	r = c.get("/v1/nothing-here", http.StatusNotFound)
	if r.errorCode(t) != "route_not_found" {
		t.Fatalf("route: %s", r.body)
	}
	r = c.get("/v1/entries/abc", http.StatusBadRequest)
	r = c.post("/v1/entries", map[string]any{
		"postings": []map[string]any{
			{"account_code": "assets:bank:idr", "side": "debit", "amount": 10},
			{"account_code": "assets:suspense:idr", "side": "credit", "amount": 9},
		},
	}, http.StatusBadRequest)
	if r.errorCode(t) != "unbalanced_entry" {
		t.Fatalf("unbalanced: %s", r.body)
	}
	r = c.post("/v1/periods/2999-01/close", nil, http.StatusUnprocessableEntity)
	if r.errorCode(t) != "period_not_ended" {
		t.Fatalf("period: %s", r.body)
	}
	c.get("/healthz", http.StatusOK)
	c.get("/readyz", http.StatusOK)
	c.get("/v1/currencies", http.StatusOK)
}

func TestIdempotency(t *testing.T) {
	c := newClient(t)
	wallet := "liabilities:wallets:cust-idem:idr"
	c.createWallet(wallet)
	body := map[string]any{"wallet_account_code": wallet, "amount": 5000, "currency": "IDR"}

	first := c.do(http.MethodPost, "/v1/transfers/topup", "topup-1", body)
	if first.status != http.StatusCreated || first.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d %v %s", first.status, first.header, first.body)
	}
	// Same key, same body (different key order and spacing): replayed.
	second := c.do(http.MethodPost, "/v1/transfers/topup", "topup-1",
		`{ "currency":"IDR",  "amount":5000, "wallet_account_code":"`+wallet+`" }`)
	if second.status != http.StatusCreated || second.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(first.body, second.body) {
		t.Fatalf("replay: %d %v %s", second.status, second.header, second.body)
	}
	// Same key, different body: rejected.
	body["amount"] = 6000
	third := c.do(http.MethodPost, "/v1/transfers/topup", "topup-1", body)
	if third.status != http.StatusUnprocessableEntity || third.errorCode(t) != "idempotency_key_reused" {
		t.Fatalf("reuse: %d %s", third.status, third.body)
	}
	// Same key on another endpoint is also a different request.
	other := c.do(http.MethodPost, "/v1/transfers/payout", "topup-1", body)
	if other.status != http.StatusUnprocessableEntity {
		t.Fatalf("cross-endpoint reuse: %d %s", other.status, other.body)
	}

	var bal httpapi.BalanceDTO
	c.get("/v1/accounts/"+wallet+"/balance", http.StatusOK).decode(t, &bal)
	if bal.Posted != 5000 {
		t.Fatalf("balance after replays = %d, want 5000", bal.Posted)
	}

	// Business errors are stored too and replayed without re-running.
	pay := map[string]any{"merchant_account_code": wallet, "amount": 999999, "currency": "IDR"}
	e1 := c.do(http.MethodPost, "/v1/transfers/payout", "payout-1", pay)
	e2 := c.do(http.MethodPost, "/v1/transfers/payout", "payout-1", pay)
	if e1.status != http.StatusUnprocessableEntity || e2.status != e1.status || e2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("stored error: %d / %d %v", e1.status, e2.status, e2.header)
	}
}

// Many identical requests with one key arrive at once. Exactly one entry
// must be created and every caller must get the same successful response.
func TestConcurrentSameKeyPostsOnce(t *testing.T) {
	c := newClient(t)
	wallet := "liabilities:wallets:cust-race:idr"
	c.createWallet(wallet)
	body := map[string]any{"wallet_account_code": wallet, "amount": 1234, "currency": "IDR"}

	const n = 20
	results := make([]response, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = c.do(http.MethodPost, "/v1/transfers/topup", "same-key", body)
		}(i)
	}
	close(start)
	wg.Wait()

	replayed := 0
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("request %d: %d %s", i, r.status, r.body)
		}
		if !bytes.Equal(r.body, results[0].body) {
			t.Fatalf("request %d got a different body", i)
		}
		if r.header.Get("Idempotent-Replayed") == "true" {
			replayed++
		}
	}
	if replayed != n-1 {
		t.Fatalf("%d replays, want %d", replayed, n-1)
	}
	var bal httpapi.BalanceDTO
	c.get("/v1/accounts/"+wallet+"/balance", http.StatusOK).decode(t, &bal)
	if bal.Posted != 1234 {
		t.Fatalf("balance = %d, want 1234 (double posting?)", bal.Posted)
	}
	var entries httpapi.ListResponse[httpapi.EntryDTO]
	c.get("/v1/entries?type=topup", http.StatusOK).decode(t, &entries)
	if len(entries.Data) != 1 {
		t.Fatalf("%d top-up entries, want 1", len(entries.Data))
	}
}
