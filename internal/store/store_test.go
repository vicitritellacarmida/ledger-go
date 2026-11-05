package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/migrate"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
	"github.com/vicitritellacarmida/ledger-go/internal/testutil"
	"github.com/vicitritellacarmida/ledger-go/migrations"
)

func TestDatabaseNameGuard(t *testing.T) {
	if _, err := testutil.DatabaseName("ledger", "store"); err == nil {
		t.Fatal("a database name without _test suffix was accepted")
	}
	name, err := testutil.DatabaseName("ledger_test", "store")
	if err != nil || name != "ledger_store_test" {
		t.Fatalf("name = %q, err = %v", name, err)
	}
	if _, err := testutil.DatabaseName("led`ger_test", "store"); err == nil {
		t.Fatal("unsafe characters were accepted")
	}
}

func TestMigrationsAreIdempotentAndGuarded(t *testing.T) {
	db := testutil.DB(t, "store")
	ctx := context.Background()

	applied, err := (&migrate.Runner{DB: db, FS: migrations.FS}).Up(ctx)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second run applied %v, err=%v", applied, err)
	}

	all, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	edited := fstest.MapFS{}
	for _, m := range all {
		edited[m.Version+".sql"] = &fstest.MapFile{Data: []byte(m.SQL)}
	}
	edited[all[0].Version+".sql"] = &fstest.MapFile{Data: []byte(all[0].SQL + "\n-- edited\n")}
	_, err = (&migrate.Runner{DB: db, FS: edited}).Up(ctx)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("edited migration not detected: %v", err)
	}

	partial := fstest.MapFS{all[0].Version + ".sql": &fstest.MapFile{Data: []byte(all[0].SQL)}}
	_, err = (&migrate.Runner{DB: db, FS: partial}).Up(ctx)
	if err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Fatalf("unknown applied migration not detected: %v", err)
	}
}

func newAccount(t *testing.T, st *store.Store, code string) *domain.Account {
	t.Helper()
	a, err := domain.NewAccount(code, "Test", domain.AccountLiability, "IDR", "owner-1", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	a.CreatedAt, a.UpdatedAt = now, now
	if err := st.InsertAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAccountsRoundTrip(t *testing.T) {
	st := store.New(testutil.DB(t, "store"))
	ctx := context.Background()
	a := newAccount(t, st, "liabilities:wallets:c1:idr")
	got, err := st.GetAccountByCode(ctx, a.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.OwnerRef != "owner-1" || got.Status != domain.StatusActive || !got.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, a)
	}
	if err := st.InsertAccount(ctx, a); !domain.HasCode(err, domain.CodeAlreadyExists) {
		t.Fatalf("duplicate insert: %v", err)
	}
	if _, err := st.GetAccountByCode(ctx, "missing"); !domain.HasCode(err, domain.CodeAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := st.ResolveAccountIDs(ctx, []string{a.Code, "missing"}); !domain.HasCode(err, domain.CodeAccountNotFound) {
		t.Fatalf("resolve with missing code: %v", err)
	}
	// Underscore must be matched literally by the prefix filter.
	newAccount(t, st, "liabilities:wallets_x:c2:idr")
	rows, err := st.ListAccounts(ctx, store.AccountFilter{CodePrefix: "liabilities:wallets_", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("prefix filter: %d rows, err=%v", len(rows), err)
	}
}

func TestLockingRequiresTransaction(t *testing.T) {
	st := store.New(testutil.DB(t, "store"))
	ctx := context.Background()
	if _, err := st.LockAccounts(ctx, []int64{1}); err == nil {
		t.Fatal("LockAccounts outside a transaction should fail")
	}
	if _, _, err := st.LockChainHead(ctx); err == nil {
		t.Fatal("LockChainHead outside a transaction should fail")
	}
}

func TestInTxRollsBackAndNests(t *testing.T) {
	st := store.New(testutil.DB(t, "store"))
	ctx := context.Background()
	a := newAccount(t, st, "liabilities:wallets:tx:idr")

	boom := errors.New("boom")
	err := st.InTx(ctx, func(ctx context.Context) error {
		locked, err := st.LockAccounts(ctx, []int64{a.ID})
		if err != nil {
			return err
		}
		acct := locked[a.ID]
		acct.PostedBalance = -100
		if err := st.UpdateAccountBalances(ctx, acct); err != nil {
			return err
		}
		// A nested InTx joins the outer transaction.
		return st.InTx(ctx, func(inner context.Context) error {
			if !store.InTransaction(inner) {
				t.Error("nested call lost the transaction")
			}
			return boom
		})
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	got, _ := st.GetAccountByCode(ctx, a.Code)
	if got.PostedBalance != 0 {
		t.Fatalf("rollback did not undo the update: %d", got.PostedBalance)
	}
}

func TestIdempotencyKeyReservation(t *testing.T) {
	st := store.New(testutil.DB(t, "store"))
	ctx := context.Background()
	rec := &store.IdempotencyRecord{Key: "k-1", Method: "POST", Path: "/v1/x", RequestHash: strings.Repeat("a", 64), CreatedAt: time.Now().UTC()}

	err := st.InTx(ctx, func(ctx context.Context) error {
		if err := st.ReserveIdempotencyKey(ctx, rec); err != nil {
			return err
		}
		return st.CompleteIdempotencyKey(ctx, rec.Key, 201, []byte(`{"ok":true}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = st.InTx(ctx, func(ctx context.Context) error { return st.ReserveIdempotencyKey(ctx, rec) })
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second reservation: %v", err)
	}
	got, err := st.GetIdempotencyKey(ctx, "k-1")
	if err != nil || got == nil || got.ResponseStatus != 201 || string(got.ResponseBody) != `{"ok":true}` {
		t.Fatalf("stored record: %+v %v", got, err)
	}
	// Keys are case-sensitive.
	if got, _ := st.GetIdempotencyKey(ctx, "K-1"); got != nil {
		t.Fatal("key lookup is case-insensitive")
	}
	n, err := st.DeleteIdempotencyKeysBefore(ctx, time.Now().UTC().Add(time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("cleanup deleted %d, err=%v", n, err)
	}
}
