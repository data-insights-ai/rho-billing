package pg

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestConcurrentConsumersCannotOverspend(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var first, second credit.Repository
			if backend == "memory" {
				first = credit.NewMemoryRepository("acct-a")
				second = first
			} else {
				store, db := testStore(t)
				createTestAccounts(t, store)
				first = store
				second = New(secondQueueDB(t, db))
			}
			engine := credit.New(first, testTime)
			ctx := t.Context()
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 12, Source: "test", SourceRef: "grant", ValidFrom: testTime()}); err != nil {
				t.Fatal(err)
			}
			outcomes := make(chan error, 24)
			var wg sync.WaitGroup
			for i := range 24 {
				wg.Go(func() {
					repo := first
					if i%2 != 0 {
						repo = second
					}
					_, err := credit.New(repo, testTime).Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: billing.OperationID(fmt.Sprintf("reserve-%d", i)), ReservationID: fmt.Sprintf("hold-%d", i), Actor: "actor", Unit: "credits", Amount: 1, Deadline: testTime().Add(time.Hour)})
					outcomes <- err
				})
			}
			wg.Wait()
			close(outcomes)
			var successes, rejected int
			for err := range outcomes {
				if err == nil {
					successes++
				} else if errors.Is(err, billing.ErrInsufficient) {
					rejected++
				} else {
					t.Fatal(err)
				}
			}
			if successes != 12 || rejected != 12 {
				t.Fatalf("success=%d rejected=%d", successes, rejected)
			}
			b, err := engine.Balance(ctx, "acct-a", "credits", "")
			if err != nil || b.Available != 0 || b.Held != 12 {
				t.Fatalf("overspend %+v %v", b, err)
			}
			if _, err := engine.VerifyLedger(ctx, "acct-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresConcurrentBalanceReadsDoNotChangeTotals(t *testing.T) {
	store, db := testStore(t)
	createTestAccounts(t, store)
	ctx := t.Context()
	engine := credit.New(store, testTime)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant-balance", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 20, Source: "test", SourceRef: "grant-balance", ValidFrom: testTime()}); err != nil {
		t.Fatal(err)
	}
	reader := credit.New(New(secondQueueDB(t, db)), testTime)
	const readers = 16
	outcomes := make(chan credit.Balance, readers)
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			got, err := reader.Balance(ctx, "acct-a", "credits", "")
			if err != nil {
				errs <- err
				return
			}
			outcomes <- got
		})
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for got := range outcomes {
		if got != (credit.Balance{Available: 20}) {
			t.Fatalf("concurrent balance=%+v, want available=20", got)
		}
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve-after-reads", ReservationID: "hold-after-reads", Actor: "actor", Unit: "credits", Amount: 5, Deadline: testTime().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Balance(ctx, "acct-a", "credits", "")
	if err != nil || got.Available != 15 || got.Held != 5 {
		t.Fatalf("after reserve %+v %v", got, err)
	}
}

func TestRefundVersusSettlementConservesHeldCredits(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var first, second credit.Repository
			if backend == "memory" {
				first = credit.NewMemoryRepository("acct-a")
				second = first
			} else {
				store, db := testStore(t)
				createTestAccounts(t, store)
				first = store
				second = New(secondQueueDB(t, db))
			}
			engine := credit.New(first, testTime)
			ctx := t.Context()
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "payment", ValidFrom: testTime()}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: testTime().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			errs := make(chan error, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				<-start
				_, err := engine.RevokeAmount(ctx, credit.RevokeAmountInput{Account: "acct-a", Operation: "refund", LotID: "lot", Reason: "partial refund", Amount: 5})
				errs <- err
			})
			in := credit.SettleInput{Account: "acct-a", Operation: "settle", ReservationID: "hold", Actual: 4, Evidence: credit.Evidence{UsageID: "usage", RatingVersion: "rule", Metrics: []credit.Metric{{Name: "actions", Quantity: 4}}}}
			wg.Go(func() { <-start; _, err := credit.New(second, testTime).Settle(ctx, in); errs <- err })
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			b, err := engine.Balance(ctx, "acct-a", "credits", "")
			if err != nil || b.Consumed != 4 || b.Held != 0 || b.Available+b.Revoked != 6 {
				t.Fatalf("race violated conservation %+v %v", b, err)
			}
			if _, err := engine.VerifyLedger(ctx, "acct-a"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Settle(ctx, in); err != nil {
				t.Fatalf("terminal replay failed %v", err)
			}
			in.Operation = "late-new-operation"
			if _, err := engine.Settle(ctx, in); !errors.Is(err, billing.ErrState) {
				t.Fatalf("second consumption accepted %v", err)
			}
		})
	}
}

func TestReservationWorkerRecoveryFencesStaleExecution(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	engine := credit.New(store, testTime)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "test", SourceRef: "source", ValidFrom: testTime()}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: testTime().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Simulated process loss: a new store/engine sees the durable active hold.
	restarted := New(secondQueueDB(t, db))
	r, err := credit.New(restarted, testTime).Reservation(ctx, "acct-a", "hold")
	if err != nil || r.State != "held" {
		t.Fatalf("lost crash checkpoint %+v %v", r, err)
	}
	msg := queueMessage("acct-a", "completion", integration.Inbound, "verified job result")
	if err := store.Receive(ctx, msg); err != nil {
		t.Fatal(err)
	}
	old, ok, err := store.Claim(ctx, integration.Inbound, "old-worker", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_inbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE account_id=$1`, "acct-a"); err != nil {
		t.Fatal(err)
	}
	next, ok, err := restarted.Claim(ctx, integration.Inbound, "successor", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("takeover %v %v", ok, err)
	}
	called := false
	if err := store.ProcessInbox(ctx, old, func(integration.Session) error { called = true; return nil }); !errors.Is(err, billing.ErrConflict) || called {
		t.Fatalf("stale worker reached reservation: %v called=%v", err, called)
	}
	if err := restarted.ProcessInbox(ctx, next, func(s integration.Session) error {
		_, err := credit.New(s.Credits(), testTime).Settle(ctx, credit.SettleInput{Account: "acct-a", Operation: "settle", ReservationID: "hold", Actual: 3, Evidence: credit.Evidence{UsageID: "job", RatingVersion: "rule", Metrics: []credit.Metric{{Name: "actions", Quantity: 3}}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b, err := engine.Balance(ctx, "acct-a", "credits", "")
	if err != nil || b.Consumed != 3 || b.Available != 7 {
		t.Fatalf("recovery %+v %v", b, err)
	}
	if _, err := engine.VerifyLedger(ctx, "acct-a"); err != nil {
		t.Fatal(err)
	}
}
