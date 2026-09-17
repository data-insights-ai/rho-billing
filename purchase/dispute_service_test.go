package purchase

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func disputeServiceFixture(t *testing.T) (*Service, Intent, Quote, billing.Scope, time.Time) {
	t.Helper()
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	account := billing.AccountID("dispute-service")
	s := New(NewMemoryRepository(ReferenceAccount{Account: account}), func() time.Time { return now })
	offer, err := s.PublishOffer(t.Context(), Offer{Account: account, Revision: Revision{ID: "dispute-offer", Version: 1}, Name: "Dispute"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := s.PublishPrice(t.Context(), Price{Account: account, Revision: Revision{ID: "dispute-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := s.CreateQuote(t.Context(), QuoteInput{Account: account, ID: "dispute-quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "dispute-line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "stripe", Merchant: "merchant", Environment: "test"}
	intent, err := s.CreateIntent(t.Context(), IntentInput{Account: account, ID: "dispute-intent", Operation: "dispute-operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "operator", Reason: "dispute", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	fact := PaymentFact{Account: account, Scope: scope, EventID: "dispute-paid", TransactionID: "dispute-tx", IntentID: intent.ID, Status: FactPaid, Currency: "USD", Gross: 100, Lines: []PaidLine{{LineID: "dispute-line", Gross: 100}}, OccurredAt: now, CollectedAt: now}
	if result, err := s.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	return s, intent, quote, scope, now
}

func TestApplyDisputeOrderingReplayAndRecovery(t *testing.T) {
	s, intent, _, scope, now := disputeServiceFixture(t)
	warning := DisputeFact{Account: intent.Account, Scope: scope, EventID: "dispute-warning", DisputeID: "case-1", IntentID: intent.ID, TransactionID: "dispute-tx", Currency: "USD", Amount: 100, Status: DisputeWarning, OccurredAt: now, EvidenceReference: "evidence-warning"}
	first, err := s.ApplyDispute(t.Context(), warning)
	if err != nil || !first.Applied || !first.StatusApplied {
		t.Fatalf("warning=%+v err=%v", first, err)
	}
	replay, err := s.ApplyDispute(t.Context(), warning)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v first=%+v", replay, err, first)
	}
	stale := warning
	stale.EventID = "dispute-stale"
	stale.Status = DisputeLost
	stale.OccurredAt = now
	staleResult, err := s.ApplyDispute(t.Context(), stale)
	if err != nil || staleResult.StatusApplied || staleResult.IgnoredStatusReason != "equal_time_conflict" {
		t.Fatalf("stale=%+v err=%v", staleResult, err)
	}
	debit := AdjustmentInput{Account: intent.Account, ID: "dispute-chargeback", IntentID: intent.ID, ProviderAdjustmentID: "provider-dispute-chargeback", TransactionID: "dispute-tx", Scope: scope, Kind: AdjustmentChargeback, Currency: "USD", Lines: []PaidLine{{LineID: "dispute-line", Gross: 100}}, PolicyVersion: "v1", CreditPolicy: CreditRefundFullOnly, Actor: "provider", Reason: "chargeback", OccurredAt: now}
	if result, err := s.ApplyAdjustment(t.Context(), debit); err != nil || !result.Applied {
		t.Fatalf("debit=%+v err=%v", result, err)
	}
	recovery := warning
	recovery.EventID = "dispute-recovery"
	recovery.Status = DisputeWon
	recovery.OccurredAt = now
	recovery.DebitAdjustmentID = debit.ID
	recovery.Recovery = &DisputeRecovery{ID: "recovery-1", AdjustmentID: debit.ID, EvidenceReference: "evidence-1", Lines: []PaidLine{{LineID: "dispute-line", Gross: 100}}}
	got, err := s.ApplyDispute(t.Context(), recovery)
	if err != nil || !got.RecoveryApplied || got.StatusApplied {
		t.Fatalf("recovery=%+v err=%v", got, err)
	}
	if _, err := s.ApplyDispute(t.Context(), recovery); err != nil {
		t.Fatal(err)
	}
	changed := recovery
	changed.EventID = "dispute-recovery-changed"
	changed.Recovery = &DisputeRecovery{ID: "recovery-1", AdjustmentID: debit.ID, EvidenceReference: "different", Lines: recovery.Recovery.Lines}
	if _, err := s.ApplyDispute(t.Context(), changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed recovery=%v", err)
	}
}
