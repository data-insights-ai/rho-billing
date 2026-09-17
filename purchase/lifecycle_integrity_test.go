package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type quoteFaultRepository struct {
	Repository
	quoteError error
	corrupt    bool
}

func (r quoteFaultRepository) WithinAccount(ctx context.Context, a billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, a, func(tx Tx) error { return fn(quoteFaultTx{Tx: tx, err: r.quoteError, corrupt: r.corrupt}) })
}

type quoteFaultTx struct {
	Tx
	err     error
	corrupt bool
}

func (t quoteFaultTx) Quote(ctx context.Context, id string) (Quote, error) {
	if t.err != nil {
		return Quote{}, t.err
	}
	q, err := t.Tx.Quote(ctx, id)
	if t.corrupt {
		q.Amount++
	}
	return q, err
}

func TestPaymentQuoteReadFailureDoesNotBecomeBusinessRejection(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repo := NewMemoryRepository(ReferenceAccount{Account: "account"})
	service := New(repo, clock)
	ctx := t.Context()
	offer, err := service.PublishOffer(ctx, Offer{Account: "account", Revision: Revision{ID: "offer", Version: 1}, Name: "purchase"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, Price{Account: "account", Revision: Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, QuoteInput{Account: "account", ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(ctx, IntentInput{Account: "account", ID: "intent", Operation: "create", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, Actor: "owner", Reason: "buy", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	fact := PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "paid", TransactionID: "tx", IntentID: intent.ID, Status: FactPaid, Currency: "USD", Gross: 120, Tax: 20, Lines: []PaidLine{{LineID: "line", Gross: 120, Tax: 20}}, OccurredAt: now, CollectedAt: now}
	injected := errors.New("injected database read failure")
	for _, r := range []quoteFaultRepository{{Repository: repo, quoteError: injected}, {Repository: repo, corrupt: true}} {
		result, err := New(r, clock).ApplyPayment(ctx, fact)
		if err == nil || result != (PaymentResult{}) {
			t.Fatalf("invalid source became durable result: %+v %v", result, err)
		}
		if r.quoteError != nil && !errors.Is(err, injected) {
			t.Fatalf("lost infrastructure error: %v", err)
		}
		if err := repo.WithinAccount(ctx, "account", func(tx Tx) error {
			_, err := tx.PaymentEvent(ctx, fact.Scope, fact.EventID)
			if !errors.Is(err, billing.ErrNotFound) {
				t.Fatalf("failure persisted event: %v", err)
			}
			_, err = tx.Funding(ctx, fact.Scope, fact.TransactionID)
			if !errors.Is(err, billing.ErrNotFound) {
				t.Fatalf("failure persisted funds: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.ApplyPayment(ctx, fact)
	if err != nil || !result.Applied {
		t.Fatalf("healthy retry=%+v %v", result, err)
	}
	now = now.Add(2 * time.Hour)
	fact.EventID = "completed"
	fact.Status = FactCompleted
	fact.OccurredAt = now
	result, err = service.ApplyPayment(ctx, fact)
	if err != nil || !result.Applied {
		t.Fatalf("delayed completion=%+v %v", result, err)
	}
	current, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || !current.PaidAt.Equal(intent.CreatedAt) {
		t.Fatalf("completion changed collection time: %+v %v", current, err)
	}
}

func TestLifecycleQuoteApprovalAndExpiryAreEnforcedBeforeIntentCreation(t *testing.T) {
	service, now, _, quote, existing := lifecycleFixture(t)
	changed := existing.IntentInput
	changed.ID = "changed"
	changed.Operation = "changed"
	changed.QuoteFingerprint = digest("different approval")
	if out, err := service.CreateIntent(t.Context(), changed); !errors.Is(err, billing.ErrConflict) || out != (Intent{}) {
		t.Fatalf("changed approval=%+v %v", out, err)
	}
	// Exact identity replay remains recoverable after expiry; new purchases do not.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	replay, err := service.CreateIntent(t.Context(), existing.IntentInput)
	if err != nil || replay != existing {
		t.Fatalf("expired replay=%+v %v", replay, err)
	}
	fresh := existing.IntentInput
	fresh.ID = "expired"
	fresh.Operation = "expired"
	fresh.QuoteFingerprint = quote.Fingerprint()
	if out, err := service.CreateIntent(t.Context(), fresh); !errors.Is(err, billing.ErrExpired) || out != (Intent{}) {
		t.Fatalf("expired creation=%+v %v", out, err)
	}
	if _, err := service.Intent(t.Context(), fresh.Account, fresh.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("expired intent persisted: %v", err)
	}
	reused := existing.IntentInput
	reused.ID = "other-id"
	if _, err := service.CreateIntent(t.Context(), reused); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("operation reuse=%v", err)
	}
}
