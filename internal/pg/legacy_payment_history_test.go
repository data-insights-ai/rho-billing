package pg

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/jackc/pgx/v5/pgconn"
)

var legacyTables = []string{"billing_payment_intents", "billing_payment_transactions", "billing_payment_events", "billing_payment_adjustments"}

func setLegacyPaymentTriggers(t *testing.T, db *sql.DB, enable bool) {
	t.Helper()
	action := "DISABLE"
	if enable {
		action = "ENABLE"
	}
	for _, table := range legacyTables {
		if _, err := db.ExecContext(t.Context(), `ALTER TABLE `+table+` `+action+` TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
	}
}

func legacySnapshot(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, table := range legacyTables {
		// All historical fields are compared, including payload bytes, outcome JSON,
		// times, identities and fingerprints. The additive 005 counter is separate.
		var value string
		query := `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t)-'refunded_amount' AS row FROM ` + table + ` t) s`
		if err := db.QueryRowContext(t.Context(), query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		out[table] = value
	}
	return out
}
func seedLegacyPaymentRows(t *testing.T, store *Store, db *sql.DB, account billing.AccountID) {
	t.Helper()
	ctx := t.Context()
	now := testTime()
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units VALUES('credits',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	setLegacyPaymentTriggers(t, db, false)
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_payment_intents(account_id,intent_id,provider,provider_account,environment,checkout_id,amount,currency,unit_code,unit_scale,credits,lot_id,valid_from,expires_at,expiry_policy,status,created_at,confirmed_at,transaction_id,refunded_amount) VALUES($1,'legacy-intent','stripe','merchant','sandbox','checkout',1000,'USD','credits',1,100,'legacy-lot',$2::timestamptz,$2::timestamptz+interval '1 hour','fixed','confirmed',$2,$2,'legacy-transaction',250)`, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_payment_transactions(account_id,provider,provider_account,environment,transaction_id,intent_id,amount,currency,status,first_event_id,outcome,occurred_at,updated_at) VALUES($1,'stripe','merchant','sandbox','legacy-transaction','legacy-intent',1000,'USD','paid','legacy-event','{"Granted":true,"Credits":100}',$2,$2)`, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_payment_events(account_id,event_id,provider,provider_account,environment,transaction_id,intent_id,status,amount,currency,occurred_at,payload,fingerprint,outcome) VALUES($1,'legacy-event','stripe','merchant','sandbox','legacy-transaction','legacy-intent','paid',1000,'USD',$2,'legacy-bytes','legacy-fingerprint','{"Granted":true,"Credits":100}')`, account, now); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		delta := ((i+1)*25)/10 - (i*25)/10
		if _, err := db.ExecContext(ctx, `INSERT INTO billing_payment_adjustments(account_id,adjustment_id,event_id,provider,provider_account,environment,transaction_id,intent_id,kind,amount,currency,occurred_at,reason,policy,payload,fingerprint,revoked_credits,applied,result) VALUES($1,$2,$3,'stripe','merchant','sandbox',$4,'legacy-intent','refund',25,'USD',$5,'refund','proportional','legacy-refund-bytes',$6,$7,true,$8::jsonb)`, account, fmt.Sprintf("historical-%d", i), fmt.Sprintf("refund-event-%d", i), fmt.Sprintf("refund-transaction-%d", i), now, fmt.Sprintf("retained-fingerprint-%d", i), delta, fmt.Sprintf(`{"Applied":true,"TargetCredits":%d,"RevokedCredits":%d}`, delta, delta)); err != nil {
			t.Fatal(err)
		}
	}
	setLegacyPaymentTriggers(t, db, true)
}
func TestLegacyUpgradePreservesRefundProjectionAndCreditHistory(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("legacy-upgrade")
	seedLegacyPaymentRows(t, store, db, account)
	beforeRows := legacySnapshot(t, db)
	var refunded int64
	if err := db.QueryRowContext(ctx, `SELECT refunded_amount FROM billing_payment_intents WHERE account_id=$1`, account).Scan(&refunded); err != nil || refunded != 250 {
		t.Fatalf("005 refund projection=%d %v", refunded, err)
	}
	// Assemble the historical ledger through the credit domain, independently of
	// the retired payment service. These quantities match the retained 100-credit
	// purchase and its ten cumulative refunds. Migration must not execute them.
	engine := credit.New(store, testTime)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: "legacy-grant", LotID: "legacy-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 100, Source: "payment", SourceRef: "legacy-intent", ValidFrom: testTime(), ExpiresAt: testTime().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		delta := int64(((i+1)*25)/10 - (i*25)/10)
		if _, err := engine.RevokeAmount(ctx, credit.RevokeAmountInput{Account: account, Operation: billing.OperationID(fmt.Sprintf("legacy-revoke-%d", i)), LotID: "legacy-lot", Reason: fmt.Sprintf("historical-%d", i), Amount: delta}); err != nil {
			t.Fatal(err)
		}
	}
	beforeLedger, err := engine.VerifyLedger(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	beforeBalance, err := engine.Balance(ctx, account, "credits", "")
	if err != nil || beforeBalance.Available != 75 || beforeBalance.Revoked != 25 {
		t.Fatalf("historical balance: %+v %v", beforeBalance, err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if after := legacySnapshot(t, db); !reflect.DeepEqual(beforeRows, after) {
		t.Fatal("migration rewrote historical financial evidence")
	}
	afterLedger, err := engine.VerifyLedger(ctx, account)
	if err != nil || afterLedger != beforeLedger {
		t.Fatalf("migration changed ledger: %+v %+v %v", beforeLedger, afterLedger, err)
	}
	afterBalance, err := engine.Balance(ctx, account, "credits", "")
	if err != nil || afterBalance != beforeBalance {
		t.Fatalf("migration changed credit balance: %+v %v", afterBalance, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT refunded_amount FROM billing_payment_intents WHERE account_id=$1`, account).Scan(&refunded); err != nil || refunded != 250 {
		t.Fatalf("retirement projection=%d %v", refunded, err)
	}
	var purchases int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_intents`).Scan(&purchases); err != nil || purchases != 0 {
		t.Fatalf("historical purchase fabricated: %d %v", purchases, err)
	}
	for _, table := range legacyTables {
		for _, query := range []string{`INSERT INTO ` + table + ` SELECT * FROM ` + table + ` LIMIT 1`, `UPDATE ` + table + ` SET account_id=account_id`, `DELETE FROM ` + table} {
			_, err := db.ExecContext(ctx, query)
			pg, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pg.Code != "55000" || !strings.Contains(pg.Message, "historical read-only") {
				t.Fatalf("freeze was not the reason %s failed: %v", query, err)
			}
		}
	}
	// Include every referencing legacy table, so an unrelated FK restriction
	// cannot masquerade as the truncate fence.
	_, err = db.ExecContext(ctx, `TRUNCATE billing_payment_events,billing_payment_adjustments,billing_payment_transactions,billing_payment_intents`)
	pg, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pg.Code != "55000" {
		t.Fatalf("truncate freeze: %v", err)
	}
	if !reflect.DeepEqual(beforeRows, legacySnapshot(t, db)) {
		t.Fatal("blocked writes changed history")
	}
	assertLegacyPurchaseFences(t, store, db, account)
}
func assertLegacyPurchaseFences(t *testing.T, store *Store, db *sql.DB, account billing.AccountID) {
	t.Helper()
	ctx := t.Context()
	svc := purchase.New(store.Purchases(), testTime)
	scope := billing.Scope{Provider: "stripe", Merchant: "merchant", Environment: "sandbox"}
	own := seedLifecycleIntent(t, svc, account, "new-own", scope, testTime())
	reused := own.IntentInput
	reused.ID = "legacy-intent"
	reused.Operation = "reuse-old-intent"
	if out, err := svc.CreateIntent(ctx, reused); !errors.Is(err, billing.ErrConflict) || out.ID != "" {
		t.Fatalf("legacy intent fence: %+v %v", out, err)
	}
	other := billing.AccountID("other-owner")
	if err := store.CreateAccount(ctx, other, string(other)); err != nil {
		t.Fatal(err)
	}
	intent := seedLifecycleIntent(t, svc, other, "new-other", scope, testTime())
	fact := purchase.PaymentFact{Account: other, Scope: scope, EventID: "new-paid", TransactionID: "legacy-transaction", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-new-other", Gross: 100}}, OccurredAt: testTime(), CollectedAt: testTime()}
	if out, err := svc.ApplyPayment(ctx, fact); !errors.Is(err, billing.ErrConflict) || out.Account != "" {
		t.Fatalf("legacy funding fence: %+v %v", out, err)
	}
	fact.TransactionID = "new-transaction"
	fact.EventID = "legacy-event"
	if out, err := svc.ApplyPayment(ctx, fact); !errors.Is(err, billing.ErrConflict) || out.Account != "" {
		t.Fatalf("legacy event fence: %+v %v", out, err)
	}
	stored, err := svc.Intent(ctx, other, intent.ID)
	if err != nil || stored.Payment != purchase.PaymentPending {
		t.Fatalf("failed fence committed state: %+v %v", stored, err)
	}
	fact.EventID = "new-paid"
	if out, err := svc.ApplyPayment(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("fresh identities blocked: %+v %v", out, err)
	}
	adjustment := purchase.AdjustmentInput{Account: other, Scope: scope, ID: "new-adjustment", IntentID: intent.ID, TransactionID: fact.TransactionID, ProviderAdjustmentID: "refund-transaction-0", Kind: purchase.AdjustmentRefund, Currency: "USD", Lines: []purchase.PaidLine{{LineID: "line-new-other", Gross: 50}}, PolicyVersion: "v1", CreditPolicy: purchase.CreditRefundProportional, Actor: "operator", Reason: "refund", OccurredAt: testTime()}
	if out, err := svc.ApplyAdjustment(ctx, adjustment); !errors.Is(err, billing.ErrConflict) || out.Account != "" {
		t.Fatalf("legacy provider adjustment fence: %+v %v", out, err)
	}
	if _, err := svc.Adjustment(ctx, other, adjustment.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("failed fence retained adjustment: %v", err)
	}
	ownFact := fact
	ownFact.Account, ownFact.IntentID, ownFact.TransactionID, ownFact.EventID = account, own.ID, "own-fresh-tx", "own-fresh-event"
	ownFact.Lines = []purchase.PaidLine{{LineID: "line-new-own", Gross: 100}}
	if out, err := svc.ApplyPayment(ctx, ownFact); err != nil || !out.Applied {
		t.Fatalf("own funding: %+v %v", out, err)
	}
	localCollision := adjustment
	localCollision.Account, localCollision.IntentID, localCollision.TransactionID = account, own.ID, ownFact.TransactionID
	localCollision.ID, localCollision.ProviderAdjustmentID = "historical-0", "fresh-provider-adjustment"
	localCollision.Lines = []purchase.PaidLine{{LineID: "line-new-own", Gross: 50}}
	if out, err := svc.ApplyAdjustment(ctx, localCollision); !errors.Is(err, billing.ErrConflict) || out.Account != "" {
		t.Fatalf("legacy local adjustment fence: %+v %v", out, err)
	}
	var stateCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_adjustment_states WHERE account_id=$1`, other).Scan(&stateCount); err != nil || stateCount != 0 {
		t.Fatalf("failed fence retained adjustment totals: %d %v", stateCount, err)
	}
}

func TestLegacyRetirementRejectsExistingIdentityConflictWithoutRewritingHistory(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("legacy-conflict")
	seedLegacyPaymentRows(t, store, db, account)
	svc := purchase.New(store.Purchases(), testTime)
	intent := seedLifecycleIntent(t, svc, account, "pre-retirement", billing.Scope{Provider: "stripe", Merchant: "merchant", Environment: "sandbox"}, testTime())
	conflicting := intent.IntentInput
	conflicting.ID = "legacy-intent"
	conflicting.Operation = "pre-existing-conflict"
	before := legacySnapshot(t, db)
	if _, err := svc.CreateIntent(ctx, conflicting); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("legacy identity fence err=%v, want conflict", err)
	}
	if !reflect.DeepEqual(before, legacySnapshot(t, db)) {
		t.Fatal("rejected intent rewrote historical evidence")
	}
	var triggers int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname='billing_payment_intents_frozen_trg' AND tgrelid='billing_payment_intents'::regclass`).Scan(&triggers); err != nil || triggers == 0 {
		t.Fatalf("missing freeze trigger: %d %v", triggers, err)
	}
}
