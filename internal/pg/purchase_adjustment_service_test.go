package pg

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func paidAdjustmentFixture(t *testing.T, suffix string) (*Store, *sql.DB, purchase.Intent, *purchase.Service, purchase.AdjustmentInput) {
	t.Helper()
	store, db, intent, svc := mixedPurchaseFixture(t, suffix, true, "adjustment-plan-"+suffix)
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "paid-" + suffix, TransactionID: "tx-" + suffix, IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "fulfillment-line-" + suffix, Gross: 200}}, OccurredAt: testTime(), CollectedAt: testTime()}
	if out, err := svc.ApplyPayment(t.Context(), fact); err != nil || !out.Applied {
		t.Fatalf("pay: %+v %v", out, err)
	}
	in := purchase.AdjustmentInput{Account: intent.Account, ID: "refund-" + suffix, IntentID: intent.ID, ProviderAdjustmentID: "provider-refund-" + suffix, TransactionID: fact.TransactionID, Scope: intent.Scope, Kind: purchase.AdjustmentRefund, Currency: "USD", Lines: []purchase.PaidLine{{LineID: fact.Lines[0].LineID, Gross: 100}}, PolicyVersion: "refund-v1", CreditPolicy: purchase.CreditRefundProportional, Actor: "operator", Reason: "customer refund", OccurredAt: testTime()}
	return store, db, intent, svc, in
}
func TestPostgresPurchaseAdjustmentHeldConsumedAccessAndHost(t *testing.T) {
	store, _, intent, svc, in := paidAdjustmentFixture(t, "exposure")
	ctx := t.Context()
	engine := credit.New(store, testTime)
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: intent.Account, Operation: "consume-reserve", ReservationID: "consume", Actor: "actor", Unit: "ai", Amount: 2000, Deadline: testTime().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: intent.Account, Operation: "consume-settle", ReservationID: "consume", Actual: 2000, Evidence: credit.Evidence{UsageID: "used", RatingVersion: "v1", Metrics: []credit.Metric{{Name: "actions", Quantity: 2000}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: intent.Account, Operation: "held-reserve", ReservationID: "held", Actor: "actor", Unit: "ai", Amount: 6000, Deadline: testTime().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.ApplyAdjustment(ctx, in)
	if err != nil || !first.Applied || len(first.Effects) != 1 {
		t.Fatalf("partial: %+v %v", first, err)
	}
	exposure := first.Effects[0]
	if exposure.TargetDelta != 5000 || exposure.RevokedCredits != 2000 || exposure.PendingCredits != 3000 || exposure.ConsumedExposure != 2000 {
		t.Fatalf("exposure: %+v", exposure)
	}
	replay, err := svc.ApplyAdjustment(ctx, in)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	rows, err := svc.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var host, plan purchase.Fulfillment
	for _, row := range rows {
		if row.Effect.Host != nil {
			host = row
		}
		if row.Effect.Plan != nil {
			plan = row
		}
	}
	if _, err := catalog.NewEntitlement(store.Entitlements(), testTime).Revocation(ctx, intent.Account, plan.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("partial revoked plan: %v", err)
	}
	second := in
	second.ID = "refund-rest"
	second.ProviderAdjustmentID = "provider-rest"
	out, err := svc.ApplyAdjustment(ctx, second)
	if err != nil || !out.Applied || len(out.Effects) != 3 {
		t.Fatalf("full: %+v %v", out, err)
	}
	for _, effect := range out.Effects {
		if effect.EffectID == exposure.EffectID && (effect.TargetDelta != 5000 || effect.RevokedCredits != 0 || effect.PendingCredits != 3000 || effect.ConsumedExposure != 2000) {
			t.Fatalf("full exposure: %+v", effect)
		}
	}
	if _, err := catalog.NewEntitlement(store.Entitlements(), testTime).Revocation(ctx, intent.Account, plan.ID); err != nil {
		t.Fatal(err)
	}
	revs, err := svc.Reversals(ctx, intent.Account, intent.ID)
	if err != nil || len(revs) != 1 || revs[0].Original.ID != host.ID {
		t.Fatalf("reversals: %+v %v", revs, err)
	}
	if _, err := svc.Acknowledge(ctx, purchase.Acknowledgment{Account: intent.Account, EffectID: host.ID, Fingerprint: host.Fingerprint(), HostReference: "late-grant", AppliedAt: testTime()}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("canceled delivery acknowledged: %v", err)
	}
	if _, err := svc.Fulfill(ctx, intent.Account, intent.ID); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	rows, err = svc.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == host.ID && row.State != purchase.FulfillmentCanceled {
			t.Fatalf("resurrected host: %+v", row)
		}
	}
	chargeback := in
	chargeback.ID = "chargeback"
	chargeback.ProviderAdjustmentID = "provider-chargeback"
	chargeback.Kind = purchase.AdjustmentChargeback
	chargeback.Lines = []purchase.PaidLine{{LineID: in.Lines[0].LineID, Gross: 200}}
	overlap, err := svc.ApplyAdjustment(ctx, chargeback)
	if err != nil || !overlap.Applied || len(overlap.Effects) != 0 {
		t.Fatalf("overlap: %+v %v", overlap, err)
	}
	if _, err := engine.Release(ctx, credit.ReleaseInput{Account: intent.Account, Operation: "release", ReservationID: "held", Reason: "job canceled"}); err != nil {
		t.Fatal(err)
	}
	balance, err := engine.Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 0 || balance.Held != 0 || balance.Consumed != 2000 || balance.Revoked != 8000 || balance.Expired != 0 {
		t.Fatalf("released refunded credits: %+v %v", balance, err)
	}
	ack := purchase.Acknowledgment{Account: intent.Account, EffectID: revs[0].ID, Fingerprint: revs[0].Fingerprint(), HostReference: "undo-host", AppliedAt: testTime()}
	receipt, err := svc.AcknowledgeReversal(ctx, ack)
	if err != nil || receipt.State != purchase.FulfillmentComplete {
		t.Fatalf("ack: %+v %v", receipt, err)
	}
	again, err := svc.AcknowledgeReversal(ctx, ack)
	if err != nil || again.RecordFingerprint() != receipt.RecordFingerprint() {
		t.Fatalf("ack replay: %+v %v", again, err)
	}
	ack.HostReference = "different"
	if _, err := svc.AcknowledgeReversal(ctx, ack); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("ack conflict: %v", err)
	}
}
func TestPostgresPurchaseAdjustmentFailureRollsBackEveryEffect(t *testing.T) {
	store, db, intent, svc, in := paidAdjustmentFixture(t, "atomic-adjust")
	ctx := t.Context()
	in.Lines[0].Gross = 200
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_adjustment_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'adjustment commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_adjustment_commit AFTER INSERT ON billing_purchase_adjustments DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_adjustment_commit()`); err != nil {
		t.Fatal(err)
	}
	out, err := svc.ApplyAdjustment(ctx, in)
	if err == nil || out.Account != "" || len(out.Effects) != 0 {
		t.Fatalf("commit failure leaked result: %+v %v", out, err)
	}
	if _, err := svc.Adjustment(ctx, intent.Account, in.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("failed adjustment retained: %v", err)
	}
	rows, err := svc.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Effect.Host != nil && row.State != purchase.FulfillmentPending {
			t.Fatal("host cancellation committed")
		}
		if row.Effect.Plan != nil {
			if _, err := catalog.NewEntitlement(store.Entitlements(), testTime).Revocation(ctx, intent.Account, row.ID); !errors.Is(err, billing.ErrNotFound) {
				t.Fatalf("access revocation committed: %v", err)
			}
		}
	}
	bal, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || bal.Available != 10000 {
		t.Fatalf("credit revocation committed: %+v %v", bal, err)
	}
	revs, err := svc.Reversals(ctx, intent.Account, intent.ID)
	if err != nil || len(revs) != 0 {
		t.Fatalf("reversal committed: %+v %v", revs, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_adjustment_commit ON billing_purchase_adjustments; DROP FUNCTION fail_adjustment_commit()`); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("host rollback")
	err = store.Atomic(ctx, intent.Account, func(session integration.Session) error {
		result, err := purchase.New(session.Purchases(), testTime).ApplyAdjustment(ctx, in)
		if err != nil {
			return err
		}
		if !result.Applied {
			t.Fatalf("bound result: %+v", result)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	bal, err = credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || bal.Available != 10000 {
		t.Fatalf("bound rollback: %+v %v", bal, err)
	}
	out, err = purchase.New(New(store.db).Purchases(), testTime).ApplyAdjustment(ctx, in)
	if err != nil || !out.Applied {
		t.Fatalf("recovery: %+v %v", out, err)
	}
}

func TestPostgresPurchaseAdjustmentBeforePaymentCanReplayAfterFunding(t *testing.T) {
	_, _, intent, svc := mixedPurchaseFixture(t, "out-of-order-adjust", true, "out-of-order-plan")
	ctx := t.Context()
	in := purchase.AdjustmentInput{Account: intent.Account, ID: "early-refund", IntentID: intent.ID, ProviderAdjustmentID: "early-provider-refund", TransactionID: "delayed-tx", Scope: intent.Scope, Kind: purchase.AdjustmentRefund, Currency: "USD", Lines: []purchase.PaidLine{{LineID: "fulfillment-line-out-of-order-adjust", Gross: 200}}, PolicyVersion: "v1", CreditPolicy: purchase.CreditRefundFullOnly, Actor: "operator", Reason: "out of order refund", OccurredAt: testTime()}
	out, err := svc.ApplyAdjustment(ctx, in)
	if !errors.Is(err, billing.ErrNotFound) || out.Account != "" {
		t.Fatalf("before funding: %+v %v", out, err)
	}
	if _, err := svc.Adjustment(ctx, in.Account, in.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("premature rejection persisted: %v", err)
	}
	paid, err := svc.ApplyPayment(ctx, purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "delayed-paid", TransactionID: in.TransactionID, IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 200, Lines: in.Lines, OccurredAt: testTime(), CollectedAt: testTime()})
	if err != nil || !paid.Applied {
		t.Fatalf("delayed payment: %+v %v", paid, err)
	}
	out, err = svc.ApplyAdjustment(ctx, in)
	if err != nil || !out.Applied {
		t.Fatalf("replayed refund: %+v %v", out, err)
	}
}
