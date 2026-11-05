package service_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
)

// runParallel starts n goroutines at the same moment and waits for them.
func runParallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// Many payments race for a wallet that can afford only some of them. Row
// locks must serialize them so that exactly the affordable number succeeds
// and the wallet never goes negative.
func TestConcurrentPaymentsNeverOverdraw(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-race")
	merchant := e.merchant("m-race")
	e.topup(wallet, 10_000)

	const workers, amount = 40, 300
	var ok, insufficient, other atomic.Int64
	runParallel(workers, func(int) {
		_, err := e.pay(wallet, merchant, amount, 10)
		switch {
		case err == nil:
			ok.Add(1)
		case domain.HasCode(err, domain.CodeInsufficientFunds):
			insufficient.Add(1)
		default:
			other.Add(1)
			t.Errorf("unexpected error: %v", err)
		}
	})
	if ok.Load() != 10_000/amount || insufficient.Load() != workers-10_000/amount {
		t.Fatalf("ok=%d insufficient=%d other=%d", ok.Load(), insufficient.Load(), other.Load())
	}
	e.wantBalance(wallet, 10_000-ok.Load()*amount, 10_000-ok.Load()*amount)
	e.assertConsistent()
}

// Transfers in opposite directions between the same accounts are the
// classic deadlock: A->B locks A then B, B->A locks B then A. Locking in id
// order makes both lock A first, so they queue instead of deadlocking.
func TestConcurrentOpposingTransfersDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	a := e.wallet("cust-a")
	b := e.wallet("cust-b")
	e.topup(a, 100_000)
	e.topup(b, 100_000)

	const workers = 40
	var failures atomic.Int64
	runParallel(workers, func(i int) {
		from, to := a, b
		if i%2 == 1 {
			from, to = b, a
		}
		_, err := e.svc.PostEntry(e.ctx, service.PostEntryInput{
			Description: fmt.Sprintf("transfer %d", i),
			Postings: []service.PostingInput{
				{AccountCode: from, Side: domain.Debit, Amount: 1_000 + int64(i)},
				{AccountCode: to, Side: domain.Credit, Amount: 1_000 + int64(i)},
			},
		})
		if err != nil {
			failures.Add(1)
			t.Errorf("transfer %d: %v", i, err)
		}
	})
	if failures.Load() != 0 {
		t.Fatalf("%d transfers failed", failures.Load())
	}
	pa, _ := e.balance(a)
	pb, _ := e.balance(b)
	if pa+pb != 200_000 {
		t.Fatalf("money created or destroyed: %d + %d", pa, pb)
	}
	// Even-numbered workers moved a->b, odd ones b->a.
	var net int64
	for i := 0; i < workers; i++ {
		if i%2 == 0 {
			net += 1_000 + int64(i)
		} else {
			net -= 1_000 + int64(i)
		}
	}
	if pa != 100_000-net {
		t.Fatalf("a = %d, want %d", pa, 100_000-net)
	}
	e.assertConsistent()
}

// Holds, captures and payments race on one wallet. Whatever the
// interleaving, the wallet's available balance must stay >= 0 and the
// stored held amount must match the open holds.
func TestConcurrentHoldsCapturesAndPayments(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-mix")
	merchant := e.merchant("m-mix")
	e.topup(wallet, 20_000)

	var holdIDs sync.Map
	runParallel(30, func(i int) {
		switch i % 3 {
		case 0:
			h, err := e.svc.PlaceHold(e.ctx, service.PlaceHoldInput{AccountCode: wallet, DestinationAccountCode: merchant, Amount: 1_500})
			if err == nil {
				holdIDs.Store(i, h.ID)
			} else if !domain.HasCode(err, domain.CodeInsufficientFunds) {
				t.Errorf("hold: %v", err)
			}
		default:
			if _, err := e.pay(wallet, merchant, 1_200, 0); err != nil && !domain.HasCode(err, domain.CodeInsufficientFunds) {
				t.Errorf("payment: %v", err)
			}
		}
	})
	var ids []int64
	holdIDs.Range(func(_, v any) bool {
		ids = append(ids, v.(int64))
		return true
	})
	// Capture half of each hold concurrently, twice per hold.
	runParallel(len(ids)*2, func(i int) {
		_, err := e.svc.CaptureHold(e.ctx, ids[i/2], service.CaptureInput{Amount: i64(750)})
		if err != nil {
			t.Errorf("capture: %v", err)
		}
	})
	for _, id := range ids {
		h, err := e.svc.GetHold(e.ctx, id)
		if err != nil || h.State != domain.HoldCaptured || h.CapturedAmount != 1_500 {
			t.Fatalf("hold %d after two half captures: %+v %v", id, h, err)
		}
	}
	e.assertConsistent()
}

// Concurrent refunds of one payment must never refund more than was paid.
func TestConcurrentRefundsRespectPaymentAmount(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-ref")
	merchant := e.merchant("m-ref")
	e.topup(wallet, 5_000)
	pay, err := e.pay(wallet, merchant, 5_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int64
	runParallel(12, func(int) {
		_, err := e.svc.Refund(e.ctx, service.RefundInput{PaymentEntryID: pay.ID, Amount: i64(1_000)})
		switch {
		case err == nil:
			ok.Add(1)
		case domain.HasCode(err, domain.CodeRefundExceedsPayment):
		default:
			t.Errorf("refund: %v", err)
		}
	})
	if ok.Load() != 5 {
		t.Fatalf("%d refunds succeeded, want 5", ok.Load())
	}
	e.wantBalance(wallet, 5_000, 5_000)
	e.wantBalance(merchant, 0, 0)
	e.assertConsistent()
}

// Two reversals of the same entry racing: exactly one wins.
func TestConcurrentReversalsOnlyOneWins(t *testing.T) {
	e := newEnv(t)
	wallet := e.wallet("cust-rev")
	top := e.topup(wallet, 1_000)
	var ok atomic.Int64
	runParallel(8, func(int) {
		_, err := e.svc.ReverseEntry(e.ctx, top.ID, service.ReverseInput{})
		switch {
		case err == nil:
			ok.Add(1)
		case domain.HasCode(err, domain.CodeAlreadyReversed):
		default:
			t.Errorf("reverse: %v", err)
		}
	})
	if ok.Load() != 1 {
		t.Fatalf("%d reversals succeeded", ok.Load())
	}
	e.wantBalance(wallet, 0, 0)
	e.assertConsistent()
}
