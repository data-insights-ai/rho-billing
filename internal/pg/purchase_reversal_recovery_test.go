package pg

import (
	"testing"

	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresHostReversalTombstoneSurvivesLostAcknowledgment(t *testing.T) {
	store, db, intent, svc, in := paidAdjustmentFixture(t, "host-reversal")
	ctx := t.Context()
	deliveries, err := svc.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var delivery purchase.Fulfillment
	for _, row := range deliveries {
		if row.Effect.Host != nil {
			delivery = row
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE host_grants(effect_id text PRIMARY KEY, fingerprint text NOT NULL, resource text NOT NULL, revoked boolean NOT NULL); CREATE TABLE host_reversals(reversal_id text PRIMARY KEY, fingerprint text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Host committed the resource but disappeared before acknowledging delivery.
	if _, err := db.ExecContext(ctx, `INSERT INTO host_grants VALUES($1,$2,'workspace',false)`, delivery.ID, delivery.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	in.Lines[0].Gross = 200
	out, err := svc.ApplyAdjustment(ctx, in)
	if err != nil || !out.Applied {
		t.Fatalf("refund: %+v %v", out, err)
	}
	reversals, err := svc.Reversals(ctx, intent.Account, intent.ID)
	if err != nil || len(reversals) != 1 {
		t.Fatalf("reversal: %+v %v", reversals, err)
	}
	reversal := reversals[0]
	for range 2 {
		hostTx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = hostTx.ExecContext(ctx, `INSERT INTO host_reversals VALUES($1,$2) ON CONFLICT DO NOTHING`, reversal.ID, reversal.Fingerprint())
		if err != nil {
			_ = hostTx.Rollback()
			t.Fatal(err)
		}
		var fingerprint string
		if err := hostTx.QueryRowContext(ctx, `SELECT fingerprint FROM host_reversals WHERE reversal_id=$1`, reversal.ID).Scan(&fingerprint); err != nil || fingerprint != reversal.Fingerprint() {
			_ = hostTx.Rollback()
			t.Fatalf("host reversal conflict: %v", err)
		}
		// Tombstone and undo use the same host transaction as reversal deduplication.
		_, err = hostTx.ExecContext(ctx, `INSERT INTO host_grants VALUES($1,$2,'',true) ON CONFLICT(effect_id) DO UPDATE SET resource='',revoked=true WHERE host_grants.fingerprint=EXCLUDED.fingerprint`, delivery.ID, delivery.Fingerprint())
		if err != nil {
			_ = hostTx.Rollback()
			t.Fatal(err)
		}
		if err := hostTx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// A delayed original delivery sees the durable tombstone; it cannot reprovision.
	if _, err := db.ExecContext(ctx, `INSERT INTO host_grants VALUES($1,$2,'resurrected',false) ON CONFLICT DO NOTHING`, delivery.ID, delivery.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	var resource string
	var revoked bool
	if err := db.QueryRowContext(ctx, `SELECT resource,revoked FROM host_grants WHERE effect_id=$1`, delivery.ID).Scan(&resource, &revoked); err != nil || resource != "" || !revoked {
		t.Fatalf("resurrected: %q %v %v", resource, revoked, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_reversal_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reversal ack commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_reversal_ack AFTER UPDATE ON billing_purchase_reversals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_reversal_ack()`); err != nil {
		t.Fatal(err)
	}
	ack := purchase.Acknowledgment{Account: intent.Account, EffectID: reversal.ID, Fingerprint: reversal.Fingerprint(), HostReference: "host-reversal", AppliedAt: testTime()}
	receipt, err := svc.AcknowledgeReversal(ctx, ack)
	if err == nil || receipt.ID != "" {
		t.Fatalf("failed ack leaked result: %+v %v", receipt, err)
	}
	pending, err := svc.Reversals(ctx, intent.Account, intent.ID)
	if err != nil || len(pending) != 1 || pending[0].State != purchase.FulfillmentPending {
		t.Fatalf("failed ack committed: %+v %v", pending, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_reversal_ack ON billing_purchase_reversals; DROP FUNCTION fail_reversal_ack()`); err != nil {
		t.Fatal(err)
	}
	fresh := purchase.New(New(store.db).Purchases(), testTime)
	receipt, err = fresh.AcknowledgeReversal(ctx, ack)
	if err != nil || receipt.State != purchase.FulfillmentComplete {
		t.Fatalf("recovery: %+v %v", receipt, err)
	}
	replay, err := fresh.AcknowledgeReversal(ctx, ack)
	if err != nil || replay.RecordFingerprint() != receipt.RecordFingerprint() {
		t.Fatalf("replay: %+v %v", replay, err)
	}
}
