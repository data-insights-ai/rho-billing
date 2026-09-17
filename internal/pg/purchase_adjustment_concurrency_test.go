package pg

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func secondStore(t *testing.T, db *sql.DB) *Store {
	t.Helper()
	var schema string
	if err := db.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	secondDB, err := sql.Open("pgx", os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	secondDB.SetMaxOpenConns(1)
	secondDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = secondDB.Close() })
	if _, err := secondDB.ExecContext(t.Context(), `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	return New(secondDB)
}

func secondPurchaseService(t *testing.T, db *sql.DB, now time.Time) *purchase.Service {
	return purchase.New(secondStore(t, db).Purchases(), func() time.Time { return now })
}

func TestPostgresPurchaseAdjustmentConcurrentAmountsRemainBounded(t *testing.T) {
	_, db, intent, first, base := paidAdjustmentFixture(t, "concurrent-bounded")
	ctx := t.Context()
	now := testTime()
	second := secondPurchaseService(t, db, now)
	one := base
	one.ID, one.ProviderAdjustmentID = "refund-concurrent-150", "provider-concurrent-150"
	one.Lines = []purchase.PaidLine{{LineID: base.Lines[0].LineID, Gross: 150}}
	two := base
	two.ID, two.ProviderAdjustmentID = "refund-concurrent-200", "provider-concurrent-200"
	two.Lines = []purchase.PaidLine{{LineID: base.Lines[0].LineID, Gross: 200}}
	results := make(chan struct {
		result purchase.AdjustmentResult
		err    error
	}, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		out, err := first.ApplyAdjustment(ctx, one)
		results <- struct {
			result purchase.AdjustmentResult
			err    error
		}{out, err}
	})
	wg.Go(func() {
		out, err := second.ApplyAdjustment(ctx, two)
		results <- struct {
			result purchase.AdjustmentResult
			err    error
		}{out, err}
	})
	wg.Wait()
	close(results)
	var applied int
	for out := range results {
		if out.err != nil {
			t.Fatalf("concurrent adjustment error=%v", out.err)
		}
		if out.result.Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied=%d, want one", applied)
	}
	var linesRaw []byte
	if err := db.QueryRowContext(ctx, `SELECT lines FROM billing_purchase_adjustment_states WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID).Scan(&linesRaw); err != nil {
		t.Fatal(err)
	}
	var state purchase.AdjustmentState
	if err := json.Unmarshal(linesRaw, &state.Lines); err != nil {
		t.Fatal(err)
	}
	var refunded int64
	for _, line := range state.Lines {
		refunded += line.RefundedGross
	}
	if refunded > 200 {
		t.Fatalf("refunded total=%d exceeds purchase", refunded)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_adjustments WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("durable adjustment count=%d, want two", count)
	}
}

func TestPostgresPurchaseAdjustmentConcurrentReserveAndFullRefund(t *testing.T) {
	store, db, intent, svc, in := paidAdjustmentFixture(t, "concurrent-reserve")
	ctx := t.Context()
	in.ID, in.ProviderAdjustmentID = "refund-concurrent-full", "provider-concurrent-full"
	in.Lines = []purchase.PaidLine{{LineID: in.Lines[0].LineID, Gross: 200}}
	in.CreditPolicy = purchase.CreditRefundFullOnly
	reserveStore := secondStore(t, db)
	start := make(chan struct{})
	reserveDone := make(chan struct {
		result credit.Result
		err    error
	}, 1)
	refundDone := make(chan struct {
		result purchase.AdjustmentResult
		err    error
	}, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		out, err := credit.New(reserveStore, testTime).Reserve(ctx, credit.ReserveInput{Account: intent.Account, Operation: "concurrent-reserve", ReservationID: "concurrent-reserve", Actor: "actor", Unit: "ai", Amount: 10000, Deadline: testTime().Add(time.Hour)})
		reserveDone <- struct {
			result credit.Result
			err    error
		}{out, err}
	})
	wg.Go(func() {
		<-start
		out, err := svc.ApplyAdjustment(ctx, in)
		refundDone <- struct {
			result purchase.AdjustmentResult
			err    error
		}{out, err}
	})
	close(start)
	wg.Wait()
	reserve, refund := <-reserveDone, <-refundDone
	if refund.err != nil || !refund.result.Applied {
		t.Fatalf("refund=%+v err=%v", refund.result, refund.err)
	}
	if reserve.err != nil && !errors.Is(reserve.err, billing.ErrInsufficient) {
		t.Fatalf("reserve error=%v", reserve.err)
	}
	if reserve.err == nil {
		if _, err := credit.New(store, testTime).Release(ctx, credit.ReleaseInput{Account: intent.Account, Operation: "concurrent-release", ReservationID: "concurrent-reserve", Reason: "refund cleanup"}); err != nil {
			t.Fatalf("release=%v", err)
		}
	}
	balance, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil {
		t.Fatal(err)
	}
	if balance.Available != 0 || balance.Held != 0 || balance.Consumed != 0 || balance.Revoked != 10000 {
		t.Fatalf("refund invariant=%+v", balance)
	}
}
