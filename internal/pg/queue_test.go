package pg

import (
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

func queueStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	store, db := testStore(t)
	ctx := t.Context()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('billing_inbox') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		schema, err := queueMigrationSQL()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(schema)); err != nil {
			t.Fatal(err)
		}
	}
	return store, db
}

func queueMessage(account billing.AccountID, id string, direction integration.Direction, payload string) integration.Message {
	return integration.Message{
		Account: account, ID: id, Scope: billing.Scope{Provider: "provider", Merchant: "merchant", Environment: "test"},
		Kind: "event", Direction: direction,
		OccurredAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), Payload: []byte(payload),
	}
}

func secondQueueDB(t *testing.T, base *sql.DB) *sql.DB {
	t.Helper()
	var schema string
	if err := base.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return searchPathDB(t, schema)
}

func searchPathDB(t *testing.T, schema string) *sql.DB {
	t.Helper()
	return searchPathDBPool(t, schema, 1)
}

func searchPathDBPool(t *testing.T, schema string, pool int) *sql.DB {
	t.Helper()
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("BILLING_TEST_DATABASE_URL is not set")
	}
	return openSearchPathPool(t, dsn, schema, pool)
}

func TestReceiveDedupAndProcessInboxRollback(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-queue", "subject-queue"); err != nil {
		t.Fatal(err)
	}
	inbound := queueMessage("acct-queue", "event-1", integration.Inbound, "payload")
	if err := store.Receive(ctx, inbound); err != nil {
		t.Fatal(err)
	}
	if err := store.Receive(ctx, inbound); err != nil {
		t.Fatalf("identical receive was not idempotent: %v", err)
	}
	changed := inbound
	changed.Payload = []byte("changed")
	if err := store.Receive(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed duplicate error=%v, want conflict", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim, ok, err := store.Claim(ctx, integration.Inbound, "worker-1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	outbound := queueMessage("acct-queue", "command-1", integration.Outbound, "command")
	callbackErr := errors.New("business callback failed")
	err = store.ProcessInbox(ctx, claim, func(scope integration.Session) error {
		if err := scope.Enqueue(ctx, outbound); err != nil {
			return err
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error=%v, want callback error", err)
	}
	if _, err := store.Delivery(ctx, outbound); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rolled back outbox delivery error=%v", err)
	}
	delivery, err := store.Delivery(ctx, inbound)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "processing" || delivery.Attempt != 1 {
		t.Fatalf("claim changed after rollback: %+v", delivery)
	}
	if err := store.ProcessInbox(ctx, claim, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	delivery, err = store.Delivery(ctx, inbound)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "processed" {
		t.Fatalf("inbox state=%s, want processed", delivery.State)
	}
	_ = db
}

func TestRacingClaimsAndExpiredLeaseBehavior(t *testing.T) {
	store, db := queueStore(t)
	second := secondQueueDB(t, db)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-race", "subject-race"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"event-a", "event-b"} {
		if err := store.Receive(ctx, queueMessage("acct-race", id, integration.Inbound, id)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	type claimResult struct {
		claim integration.Claim
		ok    bool
		err   error
	}
	results := make(chan claimResult, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		claim, ok, err := store.Claim(ctx, integration.Inbound, "worker-a", now, time.Minute)
		results <- claimResult{claim, ok, err}
	})
	wg.Go(func() {
		claim, ok, err := New(second).Claim(ctx, integration.Inbound, "worker-b", now, time.Minute)
		results <- claimResult{claim, ok, err}
	})
	wg.Wait()
	close(results)
	var claims []integration.Claim
	for result := range results {
		if result.err != nil || !result.ok {
			t.Fatalf("racing claim result=%+v", result)
		}
		claims = append(claims, result.claim)
	}
	if len(claims) != 2 || claims[0].Message.ID == claims[1].Message.ID {
		t.Fatalf("racing claims were not disjoint: %+v", claims)
	}
	// Force one inbound lease into the past. The next claim may reclaim it and
	// receives a new fence and attempt count.
	if _, err := db.ExecContext(ctx, `UPDATE billing_inbox SET lease_deadline=$1 WHERE message_id=$2`, now.Add(-time.Minute), claims[0].Message.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := store.Claim(ctx, integration.Inbound, "worker-c", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim ok=%v err=%v", ok, err)
	}
	if reclaimed.Message.ID != claims[0].Message.ID || reclaimed.Fence <= claims[0].Fence || reclaimed.Attempt != claims[0].Attempt+1 {
		t.Fatalf("reclaimed claim=%+v, original=%+v", reclaimed, claims[0])
	}
	if err := store.ProcessInbox(ctx, claims[0], func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale inbox worker error=%v, want conflict", err)
	}
	if err := store.ProcessInbox(ctx, reclaimed, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestOutboundClaimLeaseSurvivesInjectedPastClock(t *testing.T) {
	_, db := queueStore(t)
	ctx := t.Context()
	past := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	store := NewWithClock(db, func() time.Time { return past })
	account := billing.AccountID("acct-lease-past")
	if err := store.CreateAccount(ctx, account, "subject-lease-past"); err != nil {
		t.Fatal(err)
	}
	outbound := queueMessage(account, "command-past-clock", integration.Outbound, "command")
	if err := store.Atomic(ctx, account, func(scope integration.Session) error {
		return scope.Enqueue(ctx, outbound)
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := store.Claim(ctx, integration.Outbound, "past-clock-worker", past.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	permit, err := store.BeginOutbox(ctx, claim, func(integration.Session) error { return nil })
	if err != nil || !permit {
		t.Fatalf("BeginOutbox permit=%v err=%v, injected past clock must not expire a live lease", permit, err)
	}
}

func TestOutboxUnknownAndExplicitResolution(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-outbox", "subject-outbox"); err != nil {
		t.Fatal(err)
	}
	outbound := queueMessage("acct-outbox", "command-unknown", integration.Outbound, "command")
	if err := store.Atomic(ctx, outbound.Account, func(scope integration.Session) error {
		return scope.Enqueue(ctx, outbound)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim, ok, err := store.Claim(ctx, integration.Outbound, "worker-out", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("outbound claim ok=%v err=%v", ok, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_outbox SET lease_deadline=$1 WHERE message_id=$2`, now.Add(-time.Minute), outbound.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim(ctx, integration.Outbound, "worker-next", now, time.Minute); err != nil || ok {
		t.Fatalf("expired outbound was claimable: ok=%v err=%v", ok, err)
	}
	delivery, err := store.Delivery(ctx, outbound)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "unknown" {
		t.Fatalf("expired outbound state=%s, want unknown", delivery.State)
	}
	if err := store.FinishOutbox(ctx, claim, integration.OutboxResult{ObservationID: "finish", State: integration.OutboxCompleted, ProviderReference: "provider-ref", Evidence: "provider response"}, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale completion error=%v, want conflict", err)
	}
	if err := store.ResolveOutbox(ctx, claim, integration.OutboxResult{ObservationID: "lookup", State: integration.OutboxCompleted, ProviderReference: "provider-ref", Evidence: "provider query accepted"}, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	delivery, err = store.Delivery(ctx, outbound)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "completed" {
		t.Fatalf("resolved outbound state=%s, want completed", delivery.State)
	}
	if err := store.ResolveOutbox(ctx, claim, integration.OutboxResult{ObservationID: "lookup-conflict", State: integration.OutboxRejected, Evidence: "duplicate resolution"}, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second resolution error=%v, want conflict", err)
	}
}

func TestInboundDeadLetterAndOperatorRetry(t *testing.T) {
	store, _ := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-dead", "subject-dead"); err != nil {
		t.Fatal(err)
	}
	message := queueMessage("acct-dead", "event-dead", integration.Inbound, "payload")
	if err := store.Receive(ctx, message); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim, ok, err := store.Claim(ctx, integration.Inbound, "worker-dead", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := store.Fail(ctx, claim, "invalid event", now.Add(time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.Delivery(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "dead" {
		t.Fatalf("dead delivery state=%s", delivery.State)
	}
	if err := store.RetryInbox(ctx, claim, now); err != nil {
		t.Fatal(err)
	}
	retry, ok, err := store.Claim(ctx, integration.Inbound, "worker-retry", now, time.Minute)
	if err != nil || !ok || retry.Attempt != 2 {
		t.Fatalf("retry claim=%+v ok=%v err=%v", retry, ok, err)
	}
	if err := store.Fail(ctx, claim, "stale", now, 2); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale dead retry error=%v, want conflict", err)
	}
	if err := store.ProcessInbox(ctx, retry, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestQueueValidation(t *testing.T) {
	store, _ := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-validation", "subject-validation"); err != nil {
		t.Fatal(err)
	}
	if err := store.Receive(ctx, queueMessage("acct-validation", "outbound-invalid", integration.Outbound, "x")); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("outbound receive error=%v", err)
	}
	if _, ok, err := store.Claim(ctx, integration.Inbound, "", time.Now(), time.Minute); !errors.Is(err, billing.ErrInvalid) || ok {
		t.Fatalf("invalid worker claim ok=%v err=%v", ok, err)
	}
	if err := store.ResolveOutbox(ctx, integration.Claim{}, integration.OutboxResult{ObservationID: "lookup", State: integration.OutboxCompleted, Evidence: "evidence"}, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid resolution error=%v", err)
	}
}
