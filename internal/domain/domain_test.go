package domain

import (
	"errors"
	"testing"
	"time"
)

func mustAccount(t *testing.T, code string, typ AccountType, overdraft bool) *Account {
	t.Helper()
	a, err := NewAccount(code, "Test "+code, typ, "IDR", "", overdraft)
	if err != nil {
		t.Fatalf("NewAccount(%s): %v", code, err)
	}
	return a
}

func TestNormalBalance(t *testing.T) {
	cases := map[AccountType]Side{
		AccountAsset:     Debit,
		AccountExpense:   Debit,
		AccountLiability: Credit,
		AccountEquity:    Credit,
		AccountRevenue:   Credit,
	}
	for typ, want := range cases {
		if got := typ.NormalBalance(); got != want {
			t.Errorf("%s: normal balance = %s, want %s", typ, got, want)
		}
	}
}

func TestNaturalBalance(t *testing.T) {
	asset := mustAccount(t, "bank", AccountAsset, false)
	wallet := mustAccount(t, "wallet", AccountLiability, false)
	asset.PostedBalance = 500   // net debit
	wallet.PostedBalance = -500 // net credit
	if got := asset.NaturalBalance(); got != 500 {
		t.Errorf("asset natural = %d, want 500", got)
	}
	if got := wallet.NaturalBalance(); got != 500 {
		t.Errorf("liability natural = %d, want 500", got)
	}
}

func TestNewAccountValidation(t *testing.T) {
	tests := []struct {
		name, code, acctName, currency string
		typ                            AccountType
		wantCode                       string
	}{
		{"bad code", "Upper", "x", "IDR", AccountAsset, CodeValidation},
		{"empty name", "ok-code", "", "IDR", AccountAsset, CodeValidation},
		{"bad type", "ok-code", "x", "IDR", "cash", CodeValidation},
		{"bad currency", "ok-code", "x", "XXX", AccountAsset, CodeUnknownCurrency},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAccount(tc.code, tc.acctName, tc.typ, tc.currency, "", false)
			if !HasCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestApplyPostingOverdraft(t *testing.T) {
	wallet := mustAccount(t, "wallet", AccountLiability, false)
	// Credit 1000 into the wallet: natural balance grows.
	if err := wallet.ApplyPosting("IDR", -1000); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if err := wallet.PlaceHold("IDR", 300); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// Available is 700; debiting 701 must fail.
	err := wallet.ApplyPosting("IDR", 701)
	if !HasCode(err, CodeInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	if wallet.PostedBalance != -1000 {
		t.Fatalf("failed posting changed balance to %d", wallet.PostedBalance)
	}
	if err := wallet.ApplyPosting("IDR", 700); err != nil {
		t.Fatalf("debit 700: %v", err)
	}
	if wallet.AvailableBalance() != 0 {
		t.Fatalf("available = %d, want 0", wallet.AvailableBalance())
	}
}

func TestApplyPostingAllowsRefillWhenShort(t *testing.T) {
	a := mustAccount(t, "wallet", AccountLiability, false)
	a.PostedBalance = 50 // natural -50: already short (e.g. after an allowed adjustment)
	if err := a.ApplyPosting("IDR", -10); err != nil {
		t.Fatalf("refill should be accepted: %v", err)
	}
}

func TestApplyPostingOverdraftAllowed(t *testing.T) {
	a := mustAccount(t, "suspense", AccountAsset, true)
	if err := a.ApplyPosting("IDR", -5000); err != nil {
		t.Fatalf("overdraft account rejected posting: %v", err)
	}
	if a.NaturalBalance() != -5000 {
		t.Fatalf("natural = %d", a.NaturalBalance())
	}
}

func TestApplyPostingStatusAndCurrency(t *testing.T) {
	a := mustAccount(t, "wallet", AccountLiability, true)
	if err := a.ApplyPosting("USD", 1); !HasCode(err, CodeCurrencyMismatch) {
		t.Fatalf("want currency mismatch, got %v", err)
	}
	if err := a.ChangeStatus(StatusFrozen); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyPosting("IDR", -1); !HasCode(err, CodeAccountNotActive) {
		t.Fatalf("want account_not_active, got %v", err)
	}
	if err := a.PlaceHold("IDR", 1); !HasCode(err, CodeAccountNotActive) {
		t.Fatalf("want account_not_active for hold, got %v", err)
	}
}

func TestChangeStatus(t *testing.T) {
	a := mustAccount(t, "wallet", AccountLiability, false)
	a.PostedBalance = -10
	if err := a.ChangeStatus(StatusClosed); !HasCode(err, CodeAccountNotEmpty) {
		t.Fatalf("closing non-empty account: %v", err)
	}
	a.PostedBalance = 0
	if err := a.ChangeStatus(StatusFrozen); err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeStatus(StatusActive); err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeStatus(StatusClosed); err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeStatus(StatusActive); !HasCode(err, CodeInvalidTransition) {
		t.Fatalf("reopening closed account: %v", err)
	}
}

func posting(code string, amount int64) Posting {
	return Posting{AccountCode: code, Currency: "IDR", Amount: amount}
}

func TestValidatePostings(t *testing.T) {
	tests := []struct {
		name     string
		postings []Posting
		wantCode string
	}{
		{"balanced", []Posting{posting("a", 100), posting("b", -60), posting("c", -40)}, ""},
		{"one posting", []Posting{posting("a", 100)}, CodeValidation},
		{"zero amount", []Posting{posting("a", 0), posting("b", 0)}, CodeValidation},
		{"unbalanced", []Posting{posting("a", 100), posting("b", -99)}, CodeUnbalancedEntry},
		{"duplicate account", []Posting{posting("a", 100), posting("a", -100)}, CodeValidation},
		{"too large", []Posting{posting("a", MaxAmount+1), posting("b", -MaxAmount-1)}, CodeValidation},
		{"unknown currency", []Posting{{AccountCode: "a", Currency: "ZZZ", Amount: 1}, posting("b", -1)}, CodeUnknownCurrency},
		{
			"multi currency balanced",
			[]Posting{
				posting("a", 100), posting("b", -100),
				{AccountCode: "c", Currency: "USD", Amount: 5}, {AccountCode: "d", Currency: "USD", Amount: -5},
			},
			"",
		},
		{
			"multi currency unbalanced in one",
			[]Posting{
				posting("a", 100), {AccountCode: "b", Currency: "USD", Amount: -100},
			},
			CodeUnbalancedEntry,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePostings(tc.postings)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !HasCode(err, tc.wantCode) {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

func TestUnbalancedDetails(t *testing.T) {
	err := ValidatePostings([]Posting{posting("a", 100), posting("b", -90)})
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("not a domain error: %v", err)
	}
	diffs, ok := de.Details["difference_by_currency"].(map[string]int64)
	if !ok || diffs["IDR"] != 10 {
		t.Fatalf("details = %#v", de.Details)
	}
}

func TestErrorIs(t *testing.T) {
	err := NewError(KindRule, CodeInsufficientFunds, "x")
	if !errors.Is(err, &Error{Code: CodeInsufficientFunds}) {
		t.Fatal("errors.Is should match by code")
	}
	if errors.Is(err, &Error{Code: CodePeriodClosed}) {
		t.Fatal("errors.Is matched a different code")
	}
}

func TestReverse(t *testing.T) {
	e := &Entry{ID: 7, Type: EntryPayment, Postings: []Posting{posting("a", 100), posting("b", -100)}}
	rev, err := e.Reverse(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Type != EntryReversal || rev.ReversalOf == nil || *rev.ReversalOf != 7 {
		t.Fatalf("bad reversal header: %+v", rev)
	}
	if rev.Postings[0].Amount != -100 || rev.Postings[1].Amount != 100 {
		t.Fatalf("amounts not negated: %+v", rev.Postings)
	}
	if err := ValidatePostings(rev.Postings); err != nil {
		t.Fatalf("reversal does not balance: %v", err)
	}
	if _, err := rev.Reverse(time.Now(), ""); !HasCode(err, CodeNotReversible) {
		t.Fatalf("reversing a reversal: %v", err)
	}
}

func sampleEntry(seq int64) *Entry {
	e := &Entry{
		Type:          EntryManual,
		Description:   "test",
		ExternalRef:   "ref-1",
		EffectiveDate: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:     time.Date(2026, 3, 1, 10, 0, 0, 123456000, time.UTC),
		Postings:      []Posting{posting("a", 100), posting("b", -100)},
	}
	NumberPostings(e.Postings)
	SealEntry(e, seq-1, GenesisHash)
	return e
}

func TestHashChain(t *testing.T) {
	e1 := sampleEntry(1)
	if e1.Seq != 1 || e1.PrevHash != GenesisHash || len(e1.Hash) != 64 {
		t.Fatalf("bad seal: %+v", e1)
	}
	if b := VerifyLink(0, GenesisHash, e1); b != nil {
		t.Fatalf("fresh entry fails verification: %+v", b)
	}
	// Hash is deterministic.
	if again := ComputeHash(GenesisHash, e1); again != e1.Hash {
		t.Fatal("hash is not deterministic")
	}

	e2 := &Entry{Type: EntryManual, EffectiveDate: e1.EffectiveDate, CreatedAt: e1.CreatedAt, Postings: e1.Postings}
	SealEntry(e2, e1.Seq, e1.Hash)
	if b := VerifyLink(e1.Seq, e1.Hash, e2); b != nil {
		t.Fatalf("second entry fails: %+v", b)
	}

	// Tamper with an amount.
	tampered := *e1
	tampered.Postings = []Posting{posting("a", 101), posting("b", -101)}
	NumberPostings(tampered.Postings)
	if b := VerifyLink(0, GenesisHash, &tampered); b == nil {
		t.Fatal("tampered content was not detected")
	}
	// Wrong previous hash.
	if b := VerifyLink(e1.Seq, GenesisHash, e2); b == nil {
		t.Fatal("broken link was not detected")
	}
	// Sequence gap.
	if b := VerifyLink(5, e1.Hash, e2); b == nil {
		t.Fatal("sequence gap was not detected")
	}
}

func TestHoldLifecycle(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	newHold := func() *Hold {
		return &Hold{ID: 1, Amount: 1000, State: HoldActive, ExpiresAt: now.Add(time.Hour)}
	}

	t.Run("partial then final capture", func(t *testing.T) {
		h := newHold()
		rel, err := h.Capture(300, false, now)
		if err != nil || rel != 300 || h.State != HoldPartiallyCaptured || h.Remaining() != 700 {
			t.Fatalf("partial: rel=%d err=%v state=%s rem=%d", rel, err, h.State, h.Remaining())
		}
		rel, err = h.Capture(200, true, now)
		if err != nil || rel != 700 || h.State != HoldCaptured || h.Remaining() != 0 {
			t.Fatalf("final: rel=%d err=%v state=%s", rel, err, h.State)
		}
		if _, err := h.Capture(1, false, now); !HasCode(err, CodeInvalidTransition) {
			t.Fatalf("capture after final: %v", err)
		}
	})
	t.Run("capture full remainder finishes hold", func(t *testing.T) {
		h := newHold()
		if _, err := h.Capture(1000, false, now); err != nil || h.State != HoldCaptured {
			t.Fatalf("err=%v state=%s", err, h.State)
		}
	})
	t.Run("capture too much", func(t *testing.T) {
		h := newHold()
		if _, err := h.Capture(1001, false, now); !HasCode(err, CodeCaptureExceedsHold) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("capture after expiry", func(t *testing.T) {
		h := newHold()
		if _, err := h.Capture(1, false, now.Add(2*time.Hour)); !HasCode(err, CodeHoldExpired) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("void", func(t *testing.T) {
		h := newHold()
		_, _ = h.Capture(400, false, now)
		rel, err := h.Void(now)
		if err != nil || rel != 600 || h.State != HoldVoided {
			t.Fatalf("rel=%d err=%v", rel, err)
		}
		if _, err := h.Void(now); !HasCode(err, CodeInvalidTransition) {
			t.Fatalf("double void: %v", err)
		}
	})
	t.Run("expire", func(t *testing.T) {
		h := newHold()
		if _, err := h.Expire(now); !HasCode(err, CodeInvalidTransition) {
			t.Fatalf("early expire: %v", err)
		}
		rel, err := h.Expire(now.Add(time.Hour))
		if err != nil || rel != 1000 || h.State != HoldExpired {
			t.Fatalf("rel=%d err=%v", rel, err)
		}
	})
}

func TestCapturePostings(t *testing.T) {
	wallet := mustAccount(t, "wallet", AccountLiability, false)
	merchant := mustAccount(t, "merchant", AccountLiability, false)
	ps := CapturePostings(wallet, merchant, 250)
	if ps[0].Amount != 250 || ps[1].Amount != -250 {
		t.Fatalf("liability->liability capture: %+v", ps)
	}
	bank := mustAccount(t, "bank", AccountAsset, false)
	ps = CapturePostings(bank, wallet, 250)
	if ps[0].Amount != -250 {
		t.Fatalf("asset capture should credit the asset: %+v", ps)
	}
}

func TestFeeFromBasisPoints(t *testing.T) {
	tests := []struct{ amount, bps, want int64 }{
		{10000, 250, 250},
		{999, 250, 25}, // 24.975 rounds to 25
		{1, 5000, 1},   // 0.5 rounds half up
		{1, 4999, 0},   // 0.4999 rounds down
		{MaxAmount, 10000, MaxAmount},
		{12345, 0, 0},
	}
	for _, tc := range tests {
		got, err := FeeFromBasisPoints(tc.amount, tc.bps)
		if err != nil || got != tc.want {
			t.Errorf("FeeFromBasisPoints(%d, %d) = %d, %v; want %d", tc.amount, tc.bps, got, err, tc.want)
		}
	}
	if _, err := FeeFromBasisPoints(100, 10001); err == nil {
		t.Error("bps above 10000 accepted")
	}
}

func TestProRataAndSplitRefund(t *testing.T) {
	if got := ProRata(300, 1, 3); got != 100 {
		t.Errorf("ProRata = %d", got)
	}
	if got := ProRata(100, 1, 3); got != 33 {
		t.Errorf("ProRata floor = %d", got)
	}
	if got := ProRata(MaxAmount, MaxAmount-1, MaxAmount); got != MaxAmount-1 {
		t.Errorf("ProRata large = %d", got)
	}
	s := SplitRefund(10000, 250, 4000, true)
	if s.FromFee != 100 || s.FromMerchant != 3900 {
		t.Errorf("split with fee = %+v", s)
	}
	s = SplitRefund(10000, 250, 4000, false)
	if s.FromFee != 0 || s.FromMerchant != 4000 {
		t.Errorf("split without fee = %+v", s)
	}
}

func TestTransferBuilders(t *testing.T) {
	bank := mustAccount(t, "bank", AccountAsset, false)
	wallet := mustAccount(t, "wallet", AccountLiability, false)
	merchant := mustAccount(t, "merchant", AccountLiability, false)
	fee := mustAccount(t, "fee", AccountRevenue, true)

	ps, err := TopupPostings(bank, wallet, "IDR", 5000)
	if err != nil || ps[0].Amount != 5000 || ps[1].Amount != -5000 {
		t.Fatalf("topup: %+v %v", ps, err)
	}
	ps, err = PaymentPostings(wallet, merchant, fee, "IDR", 10000, 250)
	if err != nil || len(ps) != 3 || ps[1].Amount != -9750 || ps[2].Amount != -250 {
		t.Fatalf("payment: %+v %v", ps, err)
	}
	if err := ValidatePostings(ps); err != nil {
		t.Fatalf("payment unbalanced: %v", err)
	}
	ps, err = PaymentPostings(wallet, merchant, fee, "IDR", 10000, 0)
	if err != nil || len(ps) != 2 {
		t.Fatalf("payment without fee should have 2 lines: %+v %v", ps, err)
	}
	if _, err := PaymentPostings(wallet, merchant, fee, "IDR", 100, 100); err == nil {
		t.Fatal("fee equal to amount accepted")
	}
	if _, err := PaymentPostings(bank, merchant, fee, "IDR", 100, 0); err == nil {
		t.Fatal("asset as wallet accepted")
	}
	if _, err := TopupPostings(bank, wallet, "USD", 100); !HasCode(err, CodeCurrencyMismatch) {
		t.Fatalf("currency mismatch not detected: %v", err)
	}
	ps, err = PayoutPostings(merchant, bank, "IDR", 9750)
	if err != nil || ps[0].Amount != 9750 || ps[1].Amount != -9750 {
		t.Fatalf("payout: %+v %v", ps, err)
	}
	ps, err = RefundPostings(merchant, fee, wallet, "IDR", RefundSplit{FromMerchant: 3900, FromFee: 100})
	if err != nil || len(ps) != 3 || ps[2].Amount != -4000 {
		t.Fatalf("refund: %+v %v", ps, err)
	}
	if err := ValidatePostings(ps); err != nil {
		t.Fatalf("refund unbalanced: %v", err)
	}
}

func TestCurrencyFormat(t *testing.T) {
	usd, _ := LookupCurrency("USD")
	idr, _ := LookupCurrency("IDR")
	tests := []struct {
		c    Currency
		in   int64
		want string
	}{
		{usd, 12345, "123.45"},
		{usd, 5, "0.05"},
		{usd, -105, "-1.05"},
		{usd, 0, "0.00"},
		{idr, 150000, "150000"},
		{idr, -1, "-1"},
	}
	for _, tc := range tests {
		if got := tc.c.Format(tc.in); got != tc.want {
			t.Errorf("%s.Format(%d) = %q, want %q", tc.c.Code, tc.in, got, tc.want)
		}
	}
}

func TestAddChecked(t *testing.T) {
	if _, err := AddChecked(MaxAmount, 1); !HasCode(err, CodeAmountOverflow) {
		t.Fatalf("overflow not detected: %v", err)
	}
	if s, err := AddChecked(-5, 3); err != nil || s != -2 {
		t.Fatalf("AddChecked = %d, %v", s, err)
	}
}

func TestDatesAndPeriods(t *testing.T) {
	jkt, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Skip("tzdata not available")
	}
	instant := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	if got := FormatDate(DateOf(instant, jkt)); got != "2026-03-02" {
		t.Errorf("DateOf = %s", got)
	}
	p, err := ParsePeriod("2026-02")
	if err != nil {
		t.Fatal(err)
	}
	if FormatDate(p.End()) != "2026-02-28" || p.String() != "2026-02" {
		t.Errorf("period = %s end %s", p, FormatDate(p.End()))
	}
	if err := p.CanClose(time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)); !HasCode(err, CodePeriodNotEnded) {
		t.Errorf("closing on last day: %v", err)
	}
	if err := p.CanClose(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("closing after end: %v", err)
	}
	if _, err := ParseDate("d", "2026-13-01"); err == nil {
		t.Error("invalid date accepted")
	}
	if _, err := ParsePeriod("2026-1"); err == nil {
		t.Error("invalid period accepted")
	}
}

func TestChart(t *testing.T) {
	accts := ChartFor("USD")
	if len(accts) != len(DefaultChart) {
		t.Fatalf("got %d accounts", len(accts))
	}
	if accts[0].Code != "assets:bank:usd" || accts[0].Currency != "USD" {
		t.Fatalf("first account = %+v", accts[0])
	}
	for _, a := range accts {
		if _, err := NewAccount(a.Code, a.Name, a.Type, a.Currency, "", a.AllowOverdraft); err != nil {
			t.Errorf("chart account %s is invalid: %v", a.Code, err)
		}
	}
}
