package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestOutboxReadRecoversEvidenceAfterRestartAndRetention(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	account := billing.AccountID("outbox-read")
	if err := store.CreateAccount(ctx, account, "outbox-read-subject"); err != nil {
		t.Fatal(err)
	}
	message := queueMessage(account, "read-command", integration.Outbound, "frozen-request")
	if err := store.Atomic(ctx, account, func(s integration.Session) error { return s.Enqueue(ctx, message) }); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := store.Claim(ctx, integration.Outbound, "read-worker", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%v error=%v", ok, err)
	}
	callback := func(integration.Session) error { return nil }
	if send, err := store.BeginOutbox(ctx, claim, callback); err != nil || !send {
		t.Fatalf("begin=%v error=%v", send, err)
	}
	if err := store.FinishOutbox(ctx, claim, integration.OutboxResult{ObservationID: "timeout", State: integration.OutboxUnknown, ProviderReference: "known-provider-id", Evidence: "response-processing-incomplete"}, callback); err != nil {
		t.Fatal(err)
	}
	restarted := New(db)
	got, err := restarted.Outbox(ctx, account, message.ProviderScope(), message.ID)
	if err != nil || got.State != "unknown" || got.Fence != claim.Fence || got.BegunAt.IsZero() || got.ProviderReference != "known-provider-id" || got.Message.Fingerprint() != message.Fingerprint() {
		t.Fatalf("recovered=%+v error=%v", got, err)
	}
	wrongScope := message.ProviderScope()
	wrongScope.Merchant = "other-merchant"
	if got, err := restarted.Outbox(ctx, account, wrongScope, message.ID); !errors.Is(err, billing.ErrConflict) || got.Message.ID != "" {
		t.Fatalf("scope mismatch result=%+v error=%v", got, err)
	}
	// Reconstruct recovery solely from retained evidence, not the original claim.
	recoveredClaim := integration.Claim{Message: got.Message, Fence: got.Fence}
	if err := restarted.ResolveOutbox(ctx, recoveredClaim, integration.OutboxResult{ObservationID: "lookup", ExpectedPrevious: "timeout", State: integration.OutboxCompleted, ProviderReference: "known-provider-id", Evidence: "authoritative-lookup"}, callback); err != nil {
		t.Fatal(err)
	}
	if n, err := restarted.PruneDeliveryPayloads(ctx, account, integration.Outbound, time.Now().Add(time.Hour), 1); err != nil || n != 1 {
		t.Fatalf("pruned=%d error=%v", n, err)
	}
	pruned, err := restarted.Outbox(ctx, account, message.ProviderScope(), message.ID)
	if err != nil || !pruned.PayloadPruned || len(pruned.Message.Payload) != 0 || pruned.State != "completed" || pruned.ProviderReference != "known-provider-id" {
		t.Fatalf("retained outcome=%+v error=%v", pruned, err)
	}
}
