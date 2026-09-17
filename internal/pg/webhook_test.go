package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

// syntheticWebhookVerifier is intentionally deterministic test crypto. It is
// not a provider signature implementation; real adapters own that boundary.
type syntheticWebhookVerifier struct {
	message integration.Message
	err     error
	calls   int
}

func (v *syntheticWebhookVerifier) Verify(_ context.Context, body []byte, headers integration.WebhookHeaders) (integration.Message, error) {
	v.calls++
	if v.err != nil {
		return integration.Message{}, v.err
	}
	if len(headers["X-Synthetic"]) != 1 || headers["X-Synthetic"][0] != "valid" || string(body) != "payload" {
		return integration.Message{}, errors.New("synthetic verification failed")
	}
	message := v.message
	message.Payload = append([]byte(nil), body...)
	return message, nil
}

func webhookScope() billing.Scope {
	return billing.Scope{Provider: "synthetic", Merchant: "merchant", Environment: "sandbox"}
}

func webhookMessage(account billing.AccountID) integration.Message {
	scope := webhookScope()
	return integration.Message{Account: account, ID: "webhook-event", Scope: scope, Kind: "payment", Direction: integration.Inbound, OccurredAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
}

func inboxCount(t *testing.T, db querier, account, messageID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, account, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReceiveWebhookVerifiesBeforeDurableReceive(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-webhook", "webhook-subject"); err != nil {
		t.Fatal(err)
	}
	base := webhookMessage("acct-webhook")
	invalid := errors.New("invalid synthetic signature")
	verifier := &syntheticWebhookVerifier{message: base, err: invalid}
	if _, err := integration.ReceiveWebhook(ctx, store, verifier, []byte("payload"), integration.WebhookHeaders{"X-Synthetic": {"valid"}}, webhookScope()); !errors.Is(err, invalid) {
		t.Fatalf("verification error=%v, want %v", err, invalid)
	}
	if verifier.calls != 1 || inboxCount(t, db, "acct-webhook", base.ID) != 0 {
		t.Fatalf("invalid verification reached inbox: calls=%d rows=%d", verifier.calls, inboxCount(t, db, "acct-webhook", base.ID))
	}

	verifier.err = nil
	verifier.message.Scope.Merchant = "other-merchant"
	if _, err := integration.ReceiveWebhook(ctx, store, verifier, []byte("payload"), integration.WebhookHeaders{"X-Synthetic": {"valid"}}, webhookScope()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("scope mismatch error=%v, want conflict", err)
	}
	if inboxCount(t, db, "acct-webhook", base.ID) != 0 {
		t.Fatal("scope mismatch inserted an inbox row")
	}

	verifier.message = base
	verifier.message.Direction = integration.Outbound
	if _, err := integration.ReceiveWebhook(ctx, store, verifier, []byte("payload"), integration.WebhookHeaders{"X-Synthetic": {"valid"}}, webhookScope()); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("outbound verifier result error=%v, want invalid", err)
	}
	if inboxCount(t, db, "acct-webhook", base.ID) != 0 {
		t.Fatal("outbound verifier result inserted an inbox row")
	}

	verifier.message = base
	callsBeforeOversized := verifier.calls
	oversized := make([]byte, integration.MaxWebhookBody+1)
	if _, err := integration.ReceiveWebhook(ctx, store, verifier, oversized, integration.WebhookHeaders{"X-Synthetic": {"valid"}}, webhookScope()); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("oversized body error=%v, want invalid", err)
	}
	if verifier.calls != callsBeforeOversized {
		t.Fatal("oversized body reached verifier")
	}
	if inboxCount(t, db, "acct-webhook", base.ID) != 0 {
		t.Fatal("oversized body inserted an inbox row")
	}
}

func TestReceiveWebhookCommitsBeforeReturningAndDeduplicates(t *testing.T) {
	store, db := queueStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-webhook-success", "webhook-success-subject"); err != nil {
		t.Fatal(err)
	}
	verifier := &syntheticWebhookVerifier{message: webhookMessage("acct-webhook-success")}
	headers := integration.WebhookHeaders{"X-Synthetic": {"valid"}}
	got, err := integration.ReceiveWebhook(ctx, store, verifier, []byte("payload"), headers, webhookScope())
	if err != nil || got.ID != "webhook-event" {
		t.Fatalf("successful receive=%+v err=%v", got, err)
	}
	if inboxCount(t, db, "acct-webhook-success", got.ID) != 1 {
		t.Fatal("webhook returned before durable inbox receive")
	}
	if _, err := integration.ReceiveWebhook(ctx, store, verifier, []byte("payload"), headers, webhookScope()); err != nil {
		t.Fatalf("identical webhook replay=%v", err)
	}
	if inboxCount(t, db, "acct-webhook-success", got.ID) != 1 {
		t.Fatal("identical webhook replay duplicated inbox row")
	}
}
