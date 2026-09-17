package pg

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"testing"
	"time"
)

func TestDeliveryRetentionCannotRecreateGrant(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	msg := queueMessage("acct-a", "paid-event", integration.Inbound, "verified payment evidence")
	if err := store.Receive(ctx, msg); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := store.Claim(ctx, integration.Inbound, "worker", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := store.ProcessInbox(ctx, claim, func(s integration.Session) error {
		_, err := credit.New(s.Credits(), testTime).Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "paid-grant", LotID: "paid-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "payment", SourceRef: "paid-event", ValidFrom: testTime()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	count, err := store.PruneDeliveryPayloads(ctx, "acct-a", integration.Inbound, time.Now().Add(time.Second), 10)
	if err != nil || count != 1 {
		t.Fatalf("prune %d %v", count, err)
	}
	var size int
	if err := db.QueryRowContext(ctx, `SELECT octet_length(payload) FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, "acct-a", msg.ID).Scan(&size); err != nil || size != 0 {
		t.Fatalf("payload not pruned %d %v", size, err)
	}
	if err := store.Receive(ctx, msg); err != nil {
		t.Fatalf("exact replay lost tombstone %v", err)
	}
	if _, ok, err := store.Claim(ctx, integration.Inbound, "replay-worker", time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("replayed effect claimable %v %v", ok, err)
	}
	msg.Payload = []byte("changed financial fact")
	if err := store.Receive(ctx, msg); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay accepted %v", err)
	}
	engine := credit.New(store, testTime)
	b, err := engine.Balance(ctx, "acct-a", "credits", "")
	if err != nil || b.Available != 10 {
		t.Fatalf("retention recreated grant %+v %v", b, err)
	}
	if _, err := engine.VerifyLedger(ctx, "acct-a"); err != nil {
		t.Fatal(err)
	}
}
