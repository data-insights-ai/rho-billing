package pg

import (
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestOutboundStaleLeaseRecoveryResumesAfterStoreReconnect(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	account := billing.AccountID("acct-outbound-reconnect")
	if err := store.CreateAccount(ctx, account, "subject-outbound-reconnect"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	staleCount := outboundRecoveryLimit*2 + 7
	for i := range staleCount {
		enqueueOutboundForTest(t, store, queueMessage(account, fmt.Sprintf("stale-reconnect-%03d", i), integration.Outbound, "stale"))
	}
	pending := queueMessage(account, "pending-outbound-reconnect", integration.Outbound, "pending")
	enqueueOutboundForTest(t, store, pending)
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_outbox
		SET state = 'processing', worker = 'expired-worker', fence = 4, attempts = 2,
			lease_deadline = $1, available_at = $2
		WHERE account_id = $3 AND message_id LIKE 'stale-reconnect-%'`,
		now.Add(-time.Minute), now.Add(-time.Hour), string(account)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_outbox SET available_at = $1
		WHERE account_id = $2 AND message_id = $3`, now.Add(-time.Hour), string(account), pending.ID); err != nil {
		t.Fatal(err)
	}

	claim, ok, err := store.Claim(ctx, integration.Outbound, "first-worker", now, time.Minute)
	if err != nil || !ok || claim.Message.ID != pending.ID {
		t.Fatalf("first claim=%+v ok=%v err=%v", claim, ok, err)
	}
	var unknown int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing_outbox
		WHERE account_id=$1 AND message_id LIKE 'stale-reconnect-%' AND state='unknown'`, account).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if unknown != outboundRecoveryLimit {
		t.Fatalf("first recovery unknown=%d, want %d", unknown, outboundRecoveryLimit)
	}

	recovered := New(secondQueueDB(t, db))
	if _, ok, err := recovered.Claim(ctx, integration.Outbound, "second-worker", now, time.Minute); err != nil || ok {
		t.Fatalf("reconnect redispatched stale work: ok=%v err=%v", ok, err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing_outbox
		WHERE account_id=$1 AND message_id LIKE 'stale-reconnect-%' AND state='unknown'`, account).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if unknown != outboundRecoveryLimit*2 {
		t.Fatalf("second recovery unknown=%d, want %d", unknown, outboundRecoveryLimit*2)
	}
	if _, ok, err := recovered.Claim(ctx, integration.Outbound, "third-worker", now, time.Minute); err != nil || ok {
		t.Fatalf("third claim redispatched: ok=%v err=%v", ok, err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing_outbox
		WHERE account_id=$1 AND message_id LIKE 'stale-reconnect-%' AND state='unknown'`, account).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if unknown != staleCount {
		t.Fatalf("final recovery unknown=%d, want %d", unknown, staleCount)
	}
}
