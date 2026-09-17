package pg

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

func enqueueOutboundForTest(t *testing.T, store *Store, message integration.Message) {
	t.Helper()
	if err := store.Atomic(t.Context(), message.Account, func(scope integration.Session) error {
		return scope.Enqueue(t.Context(), message)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOutboundClaimRecoversOneStalePageWithoutRedispatch(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	account := billing.AccountID("acct-outbound-bounded")
	if err := store.CreateAccount(ctx, account, "subject-outbound-bounded"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	staleCount := outboundRecoveryLimit + 7
	for i := range staleCount {
		id := fmt.Sprintf("stale-outbound-%03d", i)
		enqueueOutboundForTest(t, store, queueMessage(account, id, integration.Outbound, id))
	}
	pending := queueMessage(account, "pending-outbound", integration.Outbound, "pending")
	enqueueOutboundForTest(t, store, pending)
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_outbox
		SET state = 'processing', worker = 'expired-worker', fence = 9, attempts = 3,
			lease_deadline = $1, available_at = $2
		WHERE account_id = $3 AND message_id LIKE 'stale-outbound-%'`,
		now.Add(-time.Minute), now.Add(-time.Hour), string(account)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_outbox SET available_at = $1
		WHERE account_id = $2 AND message_id = $3`, now.Add(-time.Hour), string(account), pending.ID); err != nil {
		t.Fatal(err)
	}

	claim, ok, err := store.Claim(ctx, integration.Outbound, "fresh-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Message.ID != pending.ID || claim.Attempt != 1 {
		t.Fatalf("claim=%+v, want independent pending attempt", claim)
	}
	var unknown, stillProcessing, attempts int
	if err := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE state = 'unknown'),
			COUNT(*) FILTER (WHERE state = 'processing'),
			COALESCE(MIN(attempts), 0)
		FROM billing_outbox
		WHERE account_id = $1 AND message_id LIKE 'stale-outbound-%'`, string(account)).Scan(&unknown, &stillProcessing, &attempts); err != nil {
		t.Fatal(err)
	}
	if unknown != outboundRecoveryLimit || stillProcessing != staleCount-outboundRecoveryLimit || attempts != 3 {
		t.Fatalf("stale recovery unknown=%d processing=%d min_attempts=%d, want unknown=%d processing=%d attempts=3", unknown, stillProcessing, attempts, outboundRecoveryLimit, staleCount-outboundRecoveryLimit)
	}
	delivery, err := store.Delivery(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "processing" || delivery.Attempt != 1 {
		t.Fatalf("pending delivery=%+v, want only pending command claimed", delivery)
	}

	// A second claim may recover the next bounded stale page, but it must not
	// dispatch an expired outbound attempt as if it were a fresh command.
	if _, ok, err := store.Claim(ctx, integration.Outbound, "another-worker", now, time.Minute); err != nil || ok {
		t.Fatalf("expired stale page was redispatched: ok=%v err=%v", ok, err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM billing_outbox
		WHERE account_id = $1 AND message_id LIKE 'stale-outbound-%' AND state = 'unknown'`, string(account)).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if unknown != staleCount {
		t.Fatalf("second bounded recovery unknown=%d, want %d", unknown, staleCount)
	}
}

func TestConcurrentInboundClaimsRetainLeaseTakeoverFence(t *testing.T) {
	store, db := queueStore(t)
	second := secondQueueDB(t, db)
	ctx := t.Context()
	account := billing.AccountID("acct-inbound-fence")
	if err := store.CreateAccount(ctx, account, "subject-inbound-fence"); err != nil {
		t.Fatal(err)
	}
	message := queueMessage(account, "inbound-fence", integration.Inbound, "payload")
	if err := store.Receive(ctx, message); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldClaim, ok, err := store.Claim(ctx, integration.Inbound, "old-worker", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("initial claim ok=%v err=%v", ok, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_inbox SET lease_deadline = $1 WHERE account_id = $2 AND message_id = $3`, now.Add(-time.Minute), string(account), message.ID); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type claimResult struct {
		claim integration.Claim
		ok    bool
		err   error
	}
	results := make(chan claimResult, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		claim, ok, err := store.Claim(ctx, integration.Inbound, "worker-a", now, time.Minute)
		results <- claimResult{claim: claim, ok: ok, err: err}
	})
	wg.Go(func() {
		<-start
		claim, ok, err := New(second).Claim(ctx, integration.Inbound, "worker-b", now, time.Minute)
		results <- claimResult{claim: claim, ok: ok, err: err}
	})
	close(start)
	wg.Wait()
	close(results)
	var takeover integration.Claim
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.ok {
			if takeover.Fence != 0 {
				t.Fatal("two workers took over the same inbound message")
			}
			takeover = result.claim
		}
	}
	if takeover.Fence <= oldClaim.Fence || takeover.Message.ID != message.ID {
		t.Fatalf("takeover=%+v old=%+v", takeover, oldClaim)
	}
	if err := store.ProcessInbox(ctx, oldClaim, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale inbound worker error=%v, want conflict", err)
	}
	if err := store.ProcessInbox(ctx, takeover, func(integration.Session) error { return nil }); err != nil {
		t.Fatalf("fenced takeover completion failed: %v", err)
	}
}
