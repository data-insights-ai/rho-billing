package purchase

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMemoryDisputeReferencesRollbackAndForeignOwnership(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	scope := billing.Scope{Provider: "dispute-memory", Merchant: "merchant", Environment: "sandbox"}
	accountA := billing.AccountID("dispute-memory-a")
	accountB := billing.AccountID("dispute-memory-b")
	repo := NewMemoryRepository(ReferenceAccount{Account: accountA}, ReferenceAccount{Account: accountB})
	service := New(repo, func() time.Time { return now })
	offer, err := service.PublishOffer(t.Context(), Offer{Account: accountA, Revision: Revision{ID: "dispute-memory-offer", Version: 1}, Name: "dispute"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), Price{Account: accountA, Revision: Revision{ID: "dispute-memory-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), QuoteInput{Account: accountA, ID: "dispute-memory-quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), IntentInput{Account: accountA, ID: "intent-memory", Operation: "dispute-memory-operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "operator", Reason: "dispute", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := service.ApplyPayment(t.Context(), PaymentFact{Account: accountA, Scope: scope, EventID: "payment-memory", TransactionID: "transaction-memory", IntentID: intent.ID, Status: FactPaid, Currency: "USD", Gross: 100, Lines: []PaidLine{{LineID: "line", Gross: 100}}, OccurredAt: now, CollectedAt: now}); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	caseValue := Dispute{Account: accountA, Scope: scope, ID: "case-memory", IntentID: intent.ID, TransactionID: "transaction-memory", Currency: "USD", Amount: 100, Status: DisputeWarning, StatusOccurredAt: now, StatusEventID: "event-memory", Revision: 1, CreatedAt: now, UpdatedAt: now}
	record := DisputeRecord{Fact: DisputeFact{Account: accountA, Scope: scope, EventID: "event-memory", DisputeID: caseValue.ID, IntentID: intent.ID, TransactionID: "transaction-memory", Currency: "USD", Amount: 100, Status: DisputeWarning, OccurredAt: now, EvidenceReference: "evidence-memory"}, Result: DisputeResult{Account: accountA, DisputeID: caseValue.ID, EventID: "event-memory", Applied: true, StatusApplied: true}, CreatedAt: now}
	wantErr := errors.New("rollback dispute")
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		if err := tx.SaveDispute(t.Context(), caseValue, 0); err != nil {
			return err
		}
		if err := tx.InsertDisputeEvent(t.Context(), record); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("callback error=%v, want %v", err, wantErr)
	}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		if _, err := tx.Dispute(t.Context(), scope, caseValue.ID); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("rolled-back dispute case remains")
		}
		if _, err := tx.DisputeEvent(t.Context(), scope, record.Fact.EventID); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("rolled-back dispute event remains")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		return tx.SaveDispute(t.Context(), caseValue, 0)
	}); err != nil {
		t.Fatal(err)
	}
	badRecovery := DisputeRecoveryRecord{Account: accountA, Scope: scope, DisputeID: caseValue.ID, IntentID: intent.ID, TransactionID: "wrong-transaction", Recovery: DisputeRecovery{ID: "recovery-memory", AdjustmentID: "debit-memory", EvidenceReference: "evidence-recovery", Lines: []PaidLine{{LineID: "line", Gross: 10}}}, CreatedAt: now}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		return tx.InsertDisputeRecovery(t.Context(), badRecovery)
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("bad recovery commit error=%v, want conflict", err)
	}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		if _, err := tx.DisputeRecovery(t.Context(), scope, badRecovery.Recovery.ID); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("bad recovery survived rollback")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), accountB, func(tx Tx) error {
		if _, err := tx.Dispute(t.Context(), scope, caseValue.ID); !errors.Is(err, billing.ErrConflict) {
			return errors.New("foreign dispute was not ownership-conflicted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
