package purchase

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func lifecycleFixture(t *testing.T) (*Service, time.Time, billing.Scope, Quote, Intent) {
	t.Helper()
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	s := New(NewMemoryRepository(ReferenceAccount{Account: "acct"}), func() time.Time { return now })
	offer, err := s.PublishOffer(t.Context(), Offer{Account: "acct", Revision: Revision{ID: "money", Version: 1}, Name: "Money", Effects: nil})
	if err != nil {
		t.Fatal(err)
	}
	price, err := s.PublishPrice(t.Context(), Price{Account: "acct", Revision: Revision{ID: "money-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := s.CreateQuote(t.Context(), QuoteInput{Account: "acct", ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	intent, err := s.CreateIntent(t.Context(), IntentInput{Account: "acct", ID: "intent", Operation: "create", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "purchase", ExpiresAt: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return s, now, scope, quote, intent
}

func TestLifecycleIntentPaymentReplayAndFunding(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	replay, err := s.CreateIntent(t.Context(), intent.IntentInput)
	if err != nil || replay.Fingerprint() != intent.Fingerprint() {
		t.Fatalf("intent replay=%+v err=%v", replay, err)
	}
	fact := PaymentFact{Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Rejection != "" {
		t.Fatalf("payment result=%+v", result)
	}
	paid, err := s.Intent(t.Context(), "acct", intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Payment != PaymentPaid || paid.Fulfillment != FulfillmentComplete || paid.TransactionID != fact.TransactionID {
		t.Fatalf("paid intent=%+v", paid)
	}
	repeated, err := s.ApplyPayment(t.Context(), fact)
	if err != nil || repeated != result {
		t.Fatalf("payment replay=%+v err=%v want=%+v", repeated, err, result)
	}
	funding, err := s.Funding(t.Context(), "acct", scope, fact.TransactionID)
	if err != nil || funding.IntentID != intent.ID || funding.Gross != quote.Amount {
		t.Fatalf("funding=%+v err=%v", funding, err)
	}
}

func TestLifecyclePaymentBusinessRejectionIsDurable(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	fact := PaymentFact{Account: "acct", Scope: billing.Scope{Provider: "other", Merchant: "merchant", Environment: "sandbox"}, EventID: "bad-event", TransactionID: "bad-transaction", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Rejection != RejectScope {
		t.Fatalf("rejection result=%+v", result)
	}
	if _, err := s.ApplyPayment(t.Context(), fact); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Funding(t.Context(), "acct", scope, fact.TransactionID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rejected payment created funding: %v", err)
	}
}

func TestLifecycleCompletedFactAfterExpiryKeepsOriginalFunding(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	first := PaymentFact{Account: "acct", Scope: scope, EventID: "paid-event", TransactionID: "tx-late", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)}
	if result, err := s.ApplyPayment(t.Context(), first); err != nil || !result.Applied {
		t.Fatalf("paid result=%+v err=%v", result, err)
	}
	completed := first
	completed.EventID = "completed-event"
	completed.Status = FactCompleted
	completed.OccurredAt = now.Add(time.Hour)
	result, err := s.ApplyPayment(t.Context(), completed)
	if err != nil || !result.Applied {
		t.Fatalf("late completed result=%+v err=%v", result, err)
	}
	funding, err := s.Funding(t.Context(), "acct", scope, first.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if !funding.PaidAt.Equal(first.OccurredAt) {
		t.Fatalf("funding PaidAt=%v, want original %v", funding.PaidAt, first.OccurredAt)
	}
	paid, err := s.Intent(t.Context(), "acct", intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !paid.PaidAt.Equal(first.OccurredAt) {
		t.Fatalf("intent PaidAt=%v, want original %v", paid.PaidAt, first.OccurredAt)
	}
	changed := completed
	changed.EventID = "completed-changed-collected"
	changed.CollectedAt = first.CollectedAt.Add(time.Minute)
	result, err = s.ApplyPayment(t.Context(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Rejection != RejectAllocation {
		t.Fatalf("changed collected time result=%+v", result)
	}
}

func TestLifecycleCommandStateMachine(t *testing.T) {
	s, _, _, _, intent := lifecycleFixture(t)
	dispatched, err := s.RecordCommand(t.Context(), CommandInput{Account: "acct", IntentID: intent.ID, Operation: "dispatch", ExpectedRevision: intent.Revision, State: CommandDispatched, ProviderReference: "provider-command", OccurredAt: intent.CreatedAt})
	if err != nil || dispatched.Command != CommandDispatched {
		t.Fatalf("dispatched=%+v err=%v", dispatched, err)
	}
	unknown, err := s.RecordCommand(t.Context(), CommandInput{Account: "acct", IntentID: intent.ID, Operation: "unknown", ExpectedRevision: dispatched.Revision, State: CommandUnknown, OccurredAt: intent.CreatedAt})
	if err != nil || unknown.Command != CommandUnknown {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, err := s.RecordCommand(t.Context(), CommandInput{Account: "acct", IntentID: intent.ID, Operation: "retry", ExpectedRevision: unknown.Revision, State: CommandDispatched, OccurredAt: intent.CreatedAt}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("unknown retry error=%v", err)
	}
	reconciled, err := s.RecordCommand(t.Context(), CommandInput{Account: "acct", IntentID: intent.ID, Operation: "reconcile", ExpectedRevision: unknown.Revision, State: CommandReconciled, ProviderReference: "provider-command", EvidenceReference: "lookup", OccurredAt: intent.CreatedAt})
	if err != nil || reconciled.Command != CommandReconciled {
		t.Fatalf("reconciled=%+v err=%v", reconciled, err)
	}
}

func TestLifecycleNonPaidObservationOrdering(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	first := PaymentFact{Account: "acct", Scope: scope, EventID: "pending", TransactionID: "tx-pending", IntentID: intent.ID, Status: FactActionRequired, Currency: quote.Currency, OccurredAt: now.Add(time.Minute)}
	result, err := s.ApplyPayment(t.Context(), first)
	if err != nil || !result.Applied {
		t.Fatalf("action required result=%+v err=%v", result, err)
	}
	conflict := first
	conflict.EventID = "failed-same-time"
	conflict.Status = FactFailed
	result, err = s.ApplyPayment(t.Context(), conflict)
	if err != nil || result.Applied || result.Rejection != RejectEqualTimeConflict {
		t.Fatalf("equal-time result=%+v err=%v", result, err)
	}
	stale := first
	stale.EventID = "pending-stale"
	stale.OccurredAt = now
	result, err = s.ApplyPayment(t.Context(), stale)
	if err != nil || result.Applied || result.Rejection != RejectStaleObservation {
		t.Fatalf("stale result=%+v err=%v", result, err)
	}
}
