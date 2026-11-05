package service_test

import (
	"testing"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
)

func TestMarketplaceFlow(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1001")
	merchant := e.merchant("m-2001")
	bank := domain.SysBank.Code("IDR")
	fees := domain.SysFeeRevenue.Code("IDR")

	e.topup(wallet, 150_000)
	e.wantBalance(wallet, 150_000, 150_000)
	e.wantBalance(bank, 150_000, 150_000)

	pay, err := e.svc.Payment(e.ctx, service.PaymentInput{
		WalletAccountCode: wallet, MerchantAccountCode: merchant, Amount: 100_000, Currency: "IDR", FeeBps: i64(250),
	})
	if err != nil {
		t.Fatalf("payment: %v", err)
	}
	if len(pay.Postings) != 3 || pay.Seq != 2 {
		t.Fatalf("payment entry = %+v", pay)
	}
	e.wantBalance(wallet, 50_000, 50_000)
	e.wantBalance(merchant, 97_500, 97_500)
	e.wantBalance(fees, 2_500, 2_500)

	if _, err := e.svc.Payout(e.ctx, service.PayoutInput{MerchantAccountCode: merchant, Amount: 97_500, Currency: "IDR"}); err != nil {
		t.Fatalf("payout: %v", err)
	}
	e.wantBalance(merchant, 0, 0)
	// Bank received 150,000 and paid out 97,500.
	e.wantBalance(bank, 52_500, 52_500)
	e.assertConsistent()
}

func TestInsufficientFundsLeavesNoTrace(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	e.topup(wallet, 1_000)

	_, err := e.pay(wallet, merchant, 1_001, 0)
	wantCode(t, err, domain.CodeInsufficientFunds)

	seq, _, err := e.st.ChainHead(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("chain head moved to %d after a rejected payment", seq)
	}
	e.wantBalance(wallet, 1_000, 1_000)
	e.wantBalance(merchant, 0, 0)

	// The bank cannot pay out money it does not have.
	_, err = e.svc.Payout(e.ctx, service.PayoutInput{MerchantAccountCode: merchant, Amount: 1, Currency: "IDR"})
	wantCode(t, err, domain.CodeInsufficientFunds)
	e.assertConsistent()
}

func TestManualEntries(t *testing.T) {
	e := newEnv(t)
	suspense := domain.SysSuspense.Code("IDR")
	capital := domain.SysOwnerCapital.Code("IDR")
	bank := domain.SysBank.Code("IDR")

	en, err := e.svc.PostEntry(e.ctx, service.PostEntryInput{
		Description: "Owner funds the bank account",
		Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 5_000_000},
			{AccountCode: capital, Side: domain.Credit, Amount: 5_000_000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if en.Type != domain.EntryManual || en.EffectiveDate.Format("2006-01-02") != "2026-03-15" {
		t.Fatalf("defaults not applied: type=%s date=%s", en.Type, en.EffectiveDate)
	}
	e.wantBalance(capital, 5_000_000, 5_000_000)

	tests := []struct {
		name string
		in   service.PostEntryInput
		code string
	}{
		{"unbalanced", service.PostEntryInput{Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 10},
			{AccountCode: suspense, Side: domain.Credit, Amount: 9},
		}}, domain.CodeUnbalancedEntry},
		{"unknown account", service.PostEntryInput{Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 10},
			{AccountCode: "no-such-account", Side: domain.Credit, Amount: 10},
		}}, domain.CodeAccountNotFound},
		{"system type", service.PostEntryInput{Type: domain.EntryRefund, Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 10},
			{AccountCode: suspense, Side: domain.Credit, Amount: 10},
		}}, domain.CodeValidation},
		{"currency mismatch", service.PostEntryInput{Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 10, Currency: "USD"},
			{AccountCode: domain.SysSuspense.Code("USD"), Side: domain.Credit, Amount: 10},
		}}, domain.CodeCurrencyMismatch},
		{"bad side", service.PostEntryInput{Postings: []service.PostingInput{
			{AccountCode: bank, Side: "left", Amount: 10},
			{AccountCode: suspense, Side: domain.Credit, Amount: 10},
		}}, domain.CodeValidation},
		{"future date", service.PostEntryInput{EffectiveDate: date("2026-03-16"), Postings: []service.PostingInput{
			{AccountCode: bank, Side: domain.Debit, Amount: 10},
			{AccountCode: suspense, Side: domain.Credit, Amount: 10},
		}}, domain.CodeFutureEffectiveDate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.PostEntry(e.ctx, tc.in)
			wantCode(t, err, tc.code)
		})
	}

	// One entry may move several currencies as long as each balances.
	_, err = e.svc.PostEntry(e.ctx, service.PostEntryInput{
		Type: domain.EntryAdjustment,
		Postings: []service.PostingInput{
			{AccountCode: suspense, Side: domain.Debit, Amount: 160_000},
			{AccountCode: capital, Side: domain.Credit, Amount: 160_000},
			{AccountCode: domain.SysSuspense.Code("USD"), Side: domain.Debit, Amount: 1_000},
			{AccountCode: domain.SysOwnerCapital.Code("USD"), Side: domain.Credit, Amount: 1_000},
		},
	})
	if err != nil {
		t.Fatalf("multi-currency entry: %v", err)
	}
	e.assertConsistent()
}

func TestReversal(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	top := e.topup(wallet, 10_000)

	pay, err := e.pay(wallet, merchant, 4_000, 100)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := e.svc.ReverseEntry(e.ctx, pay.ID, service.ReverseInput{})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if rev.ReversalOf == nil || *rev.ReversalOf != pay.ID || rev.Type != domain.EntryReversal {
		t.Fatalf("bad reversal: %+v", rev)
	}
	e.wantBalance(wallet, 10_000, 10_000)
	e.wantBalance(merchant, 0, 0)

	_, err = e.svc.ReverseEntry(e.ctx, pay.ID, service.ReverseInput{})
	wantCode(t, err, domain.CodeAlreadyReversed)
	_, err = e.svc.ReverseEntry(e.ctx, rev.ID, service.ReverseInput{})
	wantCode(t, err, domain.CodeNotReversible)

	view, err := e.svc.GetEntryView(e.ctx, pay.ID)
	if err != nil || view.ReversedBy == nil || *view.ReversedBy != rev.ID {
		t.Fatalf("reversed_by link missing: %+v %v", view, err)
	}

	// Spend most of the top-up, then try to reverse it: the wallet would go
	// negative, so the reversal must fail.
	if _, err := e.pay(wallet, merchant, 9_000, 0); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ReverseEntry(e.ctx, top.ID, service.ReverseInput{})
	wantCode(t, err, domain.CodeInsufficientFunds)
	e.assertConsistent()
}

func TestHoldLifecycle(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	e.topup(wallet, 10_000)

	h, err := e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{
		AccountCode: wallet, DestinationAccountCode: merchant, Amount: 6_000, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}
	e.wantBalance(wallet, 10_000, 4_000)

	// A second hold or a payment cannot use the held money.
	_, err = e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 4_001})
	wantCode(t, err, domain.CodeInsufficientFunds)
	_, err = e.pay(wallet, merchant, 4_001, 0)
	wantCode(t, err, domain.CodeInsufficientFunds)

	// Partial capture keeps the rest on hold.
	res, err := e.svc.CaptureHold(e.ctx, h.ID, service.CaptureInput{Amount: i64(2_500)})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.Hold.State != domain.HoldPartiallyCaptured || res.Hold.Remaining() != 3_500 {
		t.Fatalf("hold after partial capture: %+v", res.Hold)
	}
	if res.Entry.Type != domain.EntryHoldCapture || res.Entry.HoldID == nil {
		t.Fatalf("capture entry: %+v", res.Entry)
	}
	e.wantBalance(wallet, 7_500, 4_000)
	e.wantBalance(merchant, 2_500, 2_500)

	// Final capture of less than the remainder releases the rest.
	res, err = e.svc.CaptureHold(e.ctx, h.ID, service.CaptureInput{Amount: i64(1_000), Final: true})
	if err != nil {
		t.Fatalf("final capture: %v", err)
	}
	if res.Hold.State != domain.HoldCaptured || res.Hold.CapturedAmount != 3_500 {
		t.Fatalf("hold after final capture: %+v", res.Hold)
	}
	e.wantBalance(wallet, 6_500, 6_500)
	_, err = e.svc.VoidHold(e.ctx, h.ID)
	wantCode(t, err, domain.CodeInvalidTransition)

	// Void releases everything.
	h2, err := e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VoidHold(e.ctx, h2.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	e.wantBalance(wallet, 6_500, 6_500)

	// Capture amount larger than the hold.
	h3, err := e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 500, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CaptureHold(e.ctx, h3.ID, service.CaptureInput{Amount: i64(501)})
	wantCode(t, err, domain.CodeCaptureExceedsHold)

	// Expiry: after the TTL the job releases the hold, and capture fails.
	e.clock.Advance(2 * time.Minute)
	_, err = e.svc.CaptureHold(e.ctx, h3.ID, service.CaptureInput{})
	wantCode(t, err, domain.CodeHoldExpired)
	n, err := e.svc.ExpireDueHolds(e.ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	got, _ := e.svc.GetHold(e.ctx, h3.ID)
	if got.State != domain.HoldExpired {
		t.Fatalf("state = %s", got.State)
	}
	e.wantBalance(wallet, 6_500, 6_500)
	if n, _ := e.svc.ExpireDueHolds(e.ctx, 10); n != 0 {
		t.Fatalf("expired %d holds on second run", n)
	}

	_, err = e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 1, TTL: 31 * 24 * time.Hour})
	wantCode(t, err, domain.CodeValidation)
	e.assertConsistent()
}

func TestFrozenAccount(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	e.topup(wallet, 5_000)
	h, err := e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangeAccountStatus(e.ctx, wallet, domain.StatusFrozen); err != nil {
		t.Fatal(err)
	}
	_, err = e.pay(wallet, merchant, 10, 0)
	wantCode(t, err, domain.CodeAccountNotActive)
	_, err = e.svc.CaptureHold(e.ctx, h.ID, service.CaptureInput{})
	wantCode(t, err, domain.CodeAccountNotActive)
	// Releasing money is allowed while frozen.
	if _, err := e.svc.VoidHold(e.ctx, h.ID); err != nil {
		t.Fatalf("void on frozen account: %v", err)
	}
	_, err = e.svc.ChangeAccountStatus(e.ctx, wallet, domain.StatusClosed)
	wantCode(t, err, domain.CodeAccountNotEmpty)

	if _, err := e.svc.ChangeAccountStatus(e.ctx, merchant, domain.StatusClosed); err != nil {
		t.Fatalf("close empty account: %v", err)
	}
	_, err = e.svc.ChangeAccountStatus(e.ctx, merchant, domain.StatusActive)
	wantCode(t, err, domain.CodeInvalidTransition)
	e.assertConsistent()
}

func TestRefunds(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	fees := domain.SysFeeRevenue.Code("IDR")
	refundsPayable := domain.SysRefundsPayable.Code("IDR")
	e.topup(wallet, 20_000)
	pay, err := e.pay(wallet, merchant, 10_000, 300)
	if err != nil {
		t.Fatal(err)
	}

	// Partial refund that returns the matching share of the fee.
	r1, err := e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID, Amount: i64(4_000), RefundFee: true})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if r1.RelatedEntryID == nil || *r1.RelatedEntryID != pay.ID {
		t.Fatalf("refund not linked: %+v", r1)
	}
	e.wantBalance(wallet, 14_000, 14_000)
	e.wantBalance(merchant, 9_700-3_880, 9_700-3_880)
	e.wantBalance(fees, 300-120, 300-120)

	_, err = e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID, Amount: i64(6_001)})
	wantCode(t, err, domain.CodeRefundExceedsPayment)

	// Without the fee share the merchant cannot cover the remaining 6,000.
	_, err = e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID, Destination: service.RefundToOriginalMethod})
	wantCode(t, err, domain.CodeInsufficientFunds)

	// Refund the rest to the original payment method, fee share included.
	r2, err := e.svc.Refund(e.ctx, service.RefundInput{
		PaymentEntryID: pay.ID, Destination: service.RefundToOriginalMethod, RefundFee: true,
	})
	if err != nil {
		t.Fatalf("refund rest: %v", err)
	}
	e.wantBalance(refundsPayable, 6_000, 6_000)
	e.wantBalance(merchant, 0, 0)
	e.wantBalance(fees, 0, 0)
	_, err = e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID})
	wantCode(t, err, domain.CodeRefundExceedsPayment)

	// Reversing a refund makes its amount refundable again.
	if _, err := e.svc.ReverseEntry(e.ctx, r2.ID, service.ReverseInput{}); err != nil {
		t.Fatalf("reverse refund: %v", err)
	}
	if _, err := e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID, Amount: i64(1_000)}); err != nil {
		t.Fatalf("refund after reversal: %v", err)
	}

	_, err = e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: r1.ID})
	wantCode(t, err, domain.CodeNotAPayment)
	_, err = e.svc.ReverseEntry(e.ctx, pay.ID, service.ReverseInput{})
	wantCode(t, err, domain.CodeNotReversible)
	e.assertConsistent()
}

func TestPeriodClose(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	bank := domain.SysBank.Code("IDR")
	if _, err := e.svc.Topup(e.ctx, service.TopupInput{
		WalletAccountCode: wallet, Amount: 1_000, Currency: "IDR",
		TransferMeta: service.TransferMeta{EffectiveDate: date("2026-02-10")},
	}); err != nil {
		t.Fatalf("backdated topup: %v", err)
	}
	p, err := e.svc.ClosePeriod(e.ctx, "2026-02")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if p.Period != "2026-02" {
		t.Fatalf("period = %+v", p)
	}
	_, err = e.svc.ClosePeriod(e.ctx, "2026-02")
	wantCode(t, err, domain.CodeAlreadyExists)
	_, err = e.svc.ClosePeriod(e.ctx, "2026-03")
	wantCode(t, err, domain.CodePeriodNotEnded)

	_, err = e.svc.Topup(e.ctx, service.TopupInput{
		WalletAccountCode: wallet, Amount: 1, Currency: "IDR",
		TransferMeta: service.TransferMeta{EffectiveDate: date("2026-02-28")},
	})
	wantCode(t, err, domain.CodePeriodClosed)

	// A correction of a closed-period entry is dated in the open period.
	first, _, err := e.svc.ListEntries(e.ctx, service.ListEntriesInput{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := e.svc.ReverseEntry(e.ctx, first[0].ID, service.ReverseInput{})
	if err != nil {
		t.Fatalf("reverse into open period: %v", err)
	}
	if rev.EffectiveDate.Format("2006-01-02") != "2026-03-15" {
		t.Fatalf("reversal date = %s", rev.EffectiveDate)
	}
	_, err = e.svc.ReverseEntry(e.ctx, first[0].ID, service.ReverseInput{EffectiveDate: date("2026-02-15")})
	wantCode(t, err, domain.CodeAlreadyReversed)

	// February still shows the original balance.
	b, err := e.svc.GetBalance(e.ctx, bank, date("2026-02-28"))
	if err != nil || b.Posted != 1_000 {
		t.Fatalf("balance as of Feb: %+v %v", b, err)
	}
	periods, err := e.svc.ListClosedPeriods(e.ctx)
	if err != nil || len(periods) != 1 {
		t.Fatalf("periods = %+v %v", periods, err)
	}
	e.assertConsistent()
}

func TestStatementPagination(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	amounts := []struct {
		date   string
		amount int64
	}{
		{"2026-03-01", 1_000}, {"2026-03-02", 2_000}, {"2026-03-05", 3_000}, {"2026-03-10", 4_000},
	}
	// Insert out of date order to prove the statement sorts by effective date.
	for i := len(amounts) - 1; i >= 0; i-- {
		a := amounts[i]
		if _, err := e.svc.Topup(e.ctx, service.TopupInput{
			WalletAccountCode: wallet, Amount: a.amount, Currency: "IDR",
			TransferMeta: service.TransferMeta{EffectiveDate: date(a.date)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pay(wallet, merchant, 2_500, 0); err != nil {
		t.Fatal(err)
	}

	var lines []service.StatementLine
	cursor := ""
	pages := 0
	for {
		st, err := e.svc.Statement(e.ctx, wallet, service.StatementInput{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(lines) > 0 && st.OpeningBalance != lines[len(lines)-1].RunningBalance {
			t.Fatalf("page %d opening %d != previous closing %d", pages, st.OpeningBalance, lines[len(lines)-1].RunningBalance)
		}
		lines = append(lines, st.Lines...)
		if st.NextCursor == "" {
			break
		}
		cursor = st.NextCursor
	}
	if pages != 3 || len(lines) != 5 {
		t.Fatalf("pages=%d lines=%d", pages, len(lines))
	}
	wantRunning := []int64{1_000, 3_000, 6_000, 10_000, 7_500}
	for i, l := range lines {
		if l.RunningBalance != wantRunning[i] {
			t.Errorf("line %d running = %d, want %d", i, l.RunningBalance, wantRunning[i])
		}
	}
	if lines[4].Side != domain.Debit || lines[4].Amount != 2_500 {
		t.Errorf("payment line = %+v", lines[4])
	}

	st, err := e.svc.Statement(e.ctx, wallet, service.StatementInput{From: date("2026-03-03"), To: date("2026-03-10")})
	if err != nil {
		t.Fatal(err)
	}
	if st.OpeningBalance != 3_000 || len(st.Lines) != 2 || st.ClosingBalance != 10_000 {
		t.Fatalf("ranged statement: opening=%d lines=%d closing=%d", st.OpeningBalance, len(st.Lines), st.ClosingBalance)
	}
	_, err = e.svc.Statement(e.ctx, wallet, service.StatementInput{Cursor: "not-a-cursor"})
	wantCode(t, err, domain.CodeValidation)

	b, err := e.svc.GetBalance(e.ctx, wallet, date("2026-03-04"))
	if err != nil || b.Posted != 3_000 || b.Available != nil {
		t.Fatalf("as-of balance: %+v %v", b, err)
	}
}

func TestTrialBalanceAsOf(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	if _, err := e.svc.Topup(e.ctx, service.TopupInput{
		WalletAccountCode: wallet, Amount: 5_000, Currency: "IDR",
		TransferMeta: service.TransferMeta{EffectiveDate: date("2026-03-01")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay(wallet, merchant, 2_000, 100); err != nil {
		t.Fatal(err)
	}
	tb, err := e.svc.TrialBalance(e.ctx, date("2026-03-01"), "IDR")
	if err != nil {
		t.Fatal(err)
	}
	if !tb.Balanced || len(tb.Currencies) != 1 || tb.Currencies[0].TotalDebit != 5_000 || len(tb.Currencies[0].Lines) != 2 {
		t.Fatalf("trial balance on 03-01: %+v", tb.Currencies)
	}
	tb, err = e.svc.TrialBalance(e.ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	g := tb.Currencies[0]
	if !g.Balanced || g.TotalDebit != 5_000 || g.TotalCredit != 5_000 || len(g.Lines) != 4 {
		t.Fatalf("trial balance today: %+v", g)
	}
}

func TestAuditChainDetectsTampering(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-1")
	merchant := e.merchant("m-1")
	e.topup(wallet, 9_000)
	pay, err := e.pay(wallet, merchant, 1_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.topup(wallet, 1)

	rep, err := e.svc.VerifyChain(e.ctx)
	if err != nil || !rep.OK || rep.Checked != 3 {
		t.Fatalf("clean chain: %+v %v", rep, err)
	}

	// The triggers block edits through normal SQL.
	if _, err := e.db.Exec("UPDATE postings SET amount = amount + 1 WHERE entry_id = ?", pay.ID); err == nil {
		t.Fatal("immutability trigger did not block UPDATE")
	}

	// Simulate an attacker with DDL rights: drop the trigger and edit.
	mustExec(t, e, "DROP TRIGGER trg_postings_no_update")
	defer mustExec(t, e, `CREATE TRIGGER trg_postings_no_update BEFORE UPDATE ON postings
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'postings rows are immutable'`)
	mustExec(t, e, "UPDATE postings SET amount = 500 WHERE entry_id = ? AND line_no = 1", pay.ID)
	mustExec(t, e, "UPDATE postings SET amount = -500 WHERE entry_id = ? AND line_no = 2", pay.ID)

	rep, err = e.svc.VerifyChain(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.FirstBroken == nil || rep.FirstBroken.EntryID != pay.ID || rep.Checked != 1 {
		t.Fatalf("tampering not detected at entry %d: %+v %+v", pay.ID, rep, rep.FirstBroken)
	}
}

func mustExec(t *testing.T, e *env, q string, args ...any) {
	t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestEnsureChartIsIdempotent(t *testing.T) {
	e := newEnv(t)
	n, err := e.svc.EnsureChart(e.ctx)
	if err != nil || n != 0 {
		t.Fatalf("second EnsureChart created %d accounts, err=%v", n, err)
	}
	accts, _, err := e.svc.ListAccounts(e.ctx, service.ListAccountsInput{CodePrefix: "assets:", Currency: "USD"})
	if err != nil || len(accts) != 3 {
		t.Fatalf("USD asset accounts = %d, err=%v", len(accts), err)
	}
	_, err = e.svc.CreateAccount(e.ctx, service.CreateAccountInput{
		Code: domain.SysBank.Code("IDR"), Name: "dup", Type: domain.AccountAsset, Currency: "IDR",
	})
	wantCode(t, err, domain.CodeAlreadyExists)
}

func TestListAccountsPaging(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.account("liabilities:wallets:page-"+string(rune('a'+i))+":idr", domain.AccountLiability, "IDR", false)
	}
	var all []*domain.Account
	var after int64
	for {
		page, next, err := e.svc.ListAccounts(e.ctx, service.ListAccountsInput{CodePrefix: "liabilities:wallets:", AfterID: after, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == 0 {
			break
		}
		after = next
	}
	if len(all) != 5 {
		t.Fatalf("got %d accounts", len(all))
	}
}
