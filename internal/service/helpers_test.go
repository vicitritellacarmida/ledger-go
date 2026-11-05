package service_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
	"github.com/vicitritellacarmida/ledger-go/internal/testutil"
)

// fakeClock is a manually advanced clock shared by the service under test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	st    *store.Store
	svc   *service.Service
	clock *fakeClock
}

// startTime is 2026-03-15 10:00 in Asia/Jakarta (UTC+7).
var startTime = time.Date(2026, 3, 15, 3, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testutil.DB(t, "service")
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{now: startTime}
	st := store.New(db)
	svc := service.New(st, service.Options{
		Clock: clk.Now, Location: loc, Currencies: []string{"IDR", "USD"},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e := &env{t: t, ctx: context.Background(), db: db, st: st, svc: svc, clock: clk}
	if _, err := svc.EnsureChart(e.ctx); err != nil {
		t.Fatalf("EnsureChart: %v", err)
	}
	return e
}

func date(s string) *time.Time {
	d, err := domain.ParseDate("date", s)
	if err != nil {
		panic(err)
	}
	return &d
}

func i64(n int64) *int64 { return &n }

func (e *env) account(code string, typ domain.AccountType, currency string, overdraft bool) *domain.Account {
	e.t.Helper()
	a, err := e.svc.CreateAccount(e.ctx, service.CreateAccountInput{
		Code: code, Name: "Test " + code, Type: typ, Currency: currency, AllowOverdraft: overdraft,
	})
	if err != nil {
		e.t.Fatalf("create %s: %v", code, err)
	}
	return a
}

func (e *env) wallet(id string) string {
	code := "liabilities:wallets:" + id + ":idr"
	e.account(code, domain.AccountLiability, "IDR", false)
	return code
}

func (e *env) merchant(id string) string {
	code := "liabilities:merchants:" + id + ":idr"
	e.account(code, domain.AccountLiability, "IDR", false)
	return code
}

func (e *env) topup(wallet string, amount int64) *domain.Entry {
	e.t.Helper()
	en, err := e.svc.Topup(e.ctx, service.TopupInput{WalletAccountCode: wallet, Amount: amount, Currency: "IDR"})
	if err != nil {
		e.t.Fatalf("topup %s: %v", wallet, err)
	}
	return en
}

func (e *env) pay(wallet, merchant string, amount, fee int64) (*domain.Entry, error) {
	return e.svc.Payment(e.ctx, service.PaymentInput{
		WalletAccountCode: wallet, MerchantAccountCode: merchant, Amount: amount, Currency: "IDR", FeeAmount: i64(fee),
	})
}

// balance returns the natural posted and available balance of an account.
func (e *env) balance(code string) (posted, available int64) {
	e.t.Helper()
	a, err := e.svc.GetAccount(e.ctx, code)
	if err != nil {
		e.t.Fatalf("get %s: %v", code, err)
	}
	return a.NaturalBalance(), a.AvailableBalance()
}

func (e *env) wantBalance(code string, posted, available int64) {
	e.t.Helper()
	p, a := e.balance(code)
	if p != posted || a != available {
		e.t.Fatalf("%s: posted=%d available=%d, want posted=%d available=%d", code, p, a, posted, available)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !domain.HasCode(err, code) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

// assertConsistent checks the global invariants of the ledger:
//   - every account's stored posted balance equals the sum of its postings;
//   - every account's held amount equals the open remainder of its holds;
//   - no account that disallows overdraft has negative available balance;
//   - the trial balance balances;
//   - the audit hash chain verifies.
func (e *env) assertConsistent() {
	e.t.Helper()
	accts, _, err := e.svc.ListAccounts(e.ctx, service.ListAccountsInput{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range accts {
		sum, err := e.st.SumPostings(e.ctx, a.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		if sum != a.PostedBalance {
			e.t.Errorf("%s: posted_balance %d != sum of postings %d (lost update?)", a.Code, a.PostedBalance, sum)
		}
		held, err := e.st.SumOpenHolds(e.ctx, a.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		if held != a.HeldAmount {
			e.t.Errorf("%s: held_amount %d != open holds %d", a.Code, a.HeldAmount, held)
		}
		if !a.AllowOverdraft && a.AvailableBalance() < 0 {
			e.t.Errorf("%s: available balance %d is negative", a.Code, a.AvailableBalance())
		}
	}
	tb, err := e.svc.TrialBalance(e.ctx, nil, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if !tb.Balanced {
		e.t.Errorf("trial balance does not balance: %+v", tb.Currencies)
	}
	rep, err := e.svc.VerifyChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if !rep.OK {
		e.t.Errorf("audit chain broken: %+v", rep.FirstBroken)
	}
}
