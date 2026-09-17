package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMemoryChargebackRecoveryTotalsReplayRollbackOverflowAndIsolation(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	scope := billing.Scope{Provider: "memory-chargeback", Merchant: "merchant", Environment: "sandbox"}
	accountA := billing.AccountID("chargeback-recovery-a")
	accountB := billing.AccountID("chargeback-recovery-b")
	repo := NewMemoryRepository(ReferenceAccount{Account: accountA}, ReferenceAccount{Account: accountB})
	service := New(repo, func() time.Time { return now })
	intent := memoryChargebackIntent(t, service, accountA, scope, now)
	if result, err := service.ApplyPayment(t.Context(), PaymentFact{Account: accountA, Scope: scope, EventID: "payment-chargeback", TransactionID: "transaction-chargeback", IntentID: intent.ID, Status: FactPaid, Currency: "USD", Gross: 100, Lines: []PaidLine{{LineID: "line", Gross: 100}}, OccurredAt: now, CollectedAt: now}); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	caseValue := Dispute{Account: accountA, Scope: scope, ID: "case-chargeback", IntentID: intent.ID, TransactionID: "transaction-chargeback", Currency: "USD", Amount: 100, Status: DisputeWarning, StatusOccurredAt: now, StatusEventID: "event-chargeback", DebitAdjustmentID: "debit-chargeback", Revision: 1, CreatedAt: now, UpdatedAt: now}
	adjustment := AdjustmentRecord{Input: AdjustmentInput{Account: accountA, ID: "debit-chargeback", IntentID: intent.ID, ProviderAdjustmentID: "provider-debit-chargeback", TransactionID: "transaction-chargeback", Scope: scope, Kind: AdjustmentChargeback, Currency: "USD", Lines: []PaidLine{{LineID: "line", Gross: 100}}, PolicyVersion: "v1", CreditPolicy: CreditRefundFullOnly, Actor: "provider", Reason: "chargeback", OccurredAt: now}, Result: AdjustmentResult{Account: accountA, ID: "debit-chargeback", IntentID: intent.ID, Applied: true}, CreatedAt: now}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		if err := tx.InsertAdjustment(t.Context(), adjustment); err != nil {
			return err
		}
		return tx.SaveDispute(t.Context(), caseValue, 0)
	}); err != nil {
		t.Fatal(err)
	}
	recovery := DisputeRecoveryRecord{Account: accountA, Scope: scope, DisputeID: caseValue.ID, IntentID: intent.ID, TransactionID: caseValue.TransactionID, Recovery: DisputeRecovery{ID: "recovery-chargeback", AdjustmentID: adjustment.Input.ID, EvidenceReference: "recovery-evidence", Lines: []PaidLine{{LineID: "line", Gross: 10, Tax: 1}}}, CreatedAt: now}
	insert := func(tx Tx, value DisputeRecoveryRecord) error {
		return tx.InsertDisputeRecovery(t.Context(), value)
	}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error { return insert(tx, recovery) }); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error { return insert(tx, recovery) }); err != nil {
		t.Fatalf("exact recovery replay error=%v", err)
	}
	assertRecoveryTotals := func(account billing.AccountID, want []PaidLine) error {
		return repo.WithinAccount(t.Context(), account, func(tx Tx) error {
			reader, ok := tx.(interface {
				ChargebackRecoveries(context.Context, string) ([]PaidLine, error)
			})
			if !ok {
				return errors.New("memory transaction lacks ChargebackRecoveries")
			}
			got, err := reader.ChargebackRecoveries(t.Context(), intent.ID)
			if err != nil {
				return err
			}
			if len(got) != len(want) {
				return errors.New("unexpected recovery line count")
			}
			for i := range want {
				if got[i] != want[i] {
					return errors.New("unexpected recovery totals")
				}
			}
			return nil
		})
	}
	if err := assertRecoveryTotals(accountA, []PaidLine{{LineID: "line", Gross: 10, Tax: 1}}); err != nil {
		t.Fatal(err)
	}
	rollback := recovery
	rollback.Recovery.ID = "recovery-rollback"
	rollback.Recovery.Lines = []PaidLine{{LineID: "line", Gross: 5, Tax: 1}}
	rollbackErr := errors.New("rollback recovery")
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		if err := insert(tx, rollback); err != nil {
			return err
		}
		return rollbackErr
	}); !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback error=%v", err)
	}
	if err := assertRecoveryTotals(accountA, []PaidLine{{LineID: "line", Gross: 10, Tax: 1}}); err != nil {
		t.Fatal(err)
	}
	overflow := recovery
	overflow.Recovery.ID = "recovery-overflow"
	overflow.Recovery.Lines = []PaidLine{{LineID: "line-overflow", Gross: mathMaxInt64, Tax: 0}}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error { return insert(tx, overflow) }); err != nil {
		t.Fatal(err)
	}
	overflowNext := recovery
	overflowNext.Recovery.ID = "recovery-overflow-next"
	overflowNext.Recovery.Lines = []PaidLine{{LineID: "line-overflow", Gross: 1, Tax: 0}}
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error { return insert(tx, overflowNext) }); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow error=%v, want ErrOverflow", err)
	}
	if err := repo.WithinAccount(t.Context(), accountB, func(tx Tx) error {
		reader := tx.(interface {
			ChargebackRecoveries(context.Context, string) ([]PaidLine, error)
		})
		if _, err := reader.ChargebackRecoveries(t.Context(), intent.ID); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("foreign account observed recovery totals")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

func memoryChargebackIntent(t *testing.T, service *Service, account billing.AccountID, scope billing.Scope, now time.Time) Intent {
	t.Helper()
	offer, err := service.PublishOffer(t.Context(), Offer{Account: account, Revision: Revision{ID: "chargeback-offer", Version: 1}, Name: "chargeback"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), Price{Account: account, Revision: Revision{ID: "chargeback-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), QuoteInput{Account: account, ID: "chargeback-quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), IntentInput{Account: account, ID: "intent-chargeback", Operation: "operation-chargeback", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "operator", Reason: "chargeback", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
