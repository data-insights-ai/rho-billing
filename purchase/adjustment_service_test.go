package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestApplyAdjustmentReplayAndOverlappingChargebackDoNotDoubleRevoke(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	effects := []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5}}}
	service, repo, scope, intent, quote := adjustmentPurchaseFixture(t, effects, now)
	in := AdjustmentInput{Account: intent.Account, ID: "refund-1", IntentID: intent.ID, ProviderAdjustmentID: "provider-refund-1", TransactionID: "adjustment-tx", Scope: scope, Kind: AdjustmentRefund, Currency: quote.Currency, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, PolicyVersion: "policy-1", CreditPolicy: CreditRefundFullOnly, Actor: "actor", Reason: "refund", OccurredAt: now.Add(2 * time.Minute)}
	first, err := service.ApplyAdjustment(t.Context(), in)
	if err != nil || !first.Applied || len(first.Effects) != 1 {
		t.Fatalf("first adjustment=%+v err=%v", first, err)
	}
	replay, err := service.ApplyAdjustment(t.Context(), in)
	if err != nil || replay.ID != first.ID || len(replay.Effects) != len(first.Effects) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	chargeback := in
	chargeback.ID = "chargeback-1"
	chargeback.ProviderAdjustmentID = "provider-chargeback-1"
	chargeback.Kind = AdjustmentChargeback
	chargeback.CreditPolicy = CreditRefundFullOnly
	second, err := service.ApplyAdjustment(t.Context(), chargeback)
	if err != nil || !second.Applied {
		t.Fatalf("overlapping chargeback=%+v err=%v", second, err)
	}
	if len(second.Effects) != 0 {
		t.Fatalf("overlapping chargeback repeated effects=%+v", second.Effects)
	}
	if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(3 * time.Minute) }).Balance(t.Context(), intent.Account, "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 0 || balance.Revoked != 5 {
			return errors.New("overlapping adjustment changed revoked credit twice")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAdjustmentLaterEffectFailureRollsBackEarlierCreditRevocation(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	effects := []Effect{
		{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5}},
		{Key: "host", Host: &HostBenefit{Kind: "host", Payload: []byte(`{"ok":true}`)}},
	}
	_, repo, scope, intent, quote := adjustmentPurchaseFixture(t, effects, now)
	in := AdjustmentInput{Account: intent.Account, ID: "refund-mixed", IntentID: intent.ID, ProviderAdjustmentID: "provider-refund-mixed", TransactionID: "adjustment-tx", Scope: scope, Kind: AdjustmentRefund, Currency: quote.Currency, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, PolicyVersion: "policy-1", CreditPolicy: CreditRefundFullOnly, Actor: "actor", Reason: "refund", OccurredAt: now.Add(2 * time.Minute)}
	failing := New(failingAdjustmentRepository{Repository: repo, err: billing.ErrState}, func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := failing.ApplyAdjustment(t.Context(), in); !errors.Is(err, billing.ErrState) {
		t.Fatalf("mixed adjustment error=%v, want state failure", err)
	}
	if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(4 * time.Minute) }).Balance(t.Context(), intent.Account, "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 5 || balance.Revoked != 0 {
			return errors.New("earlier credit revocation escaped rollback")
		}
		if _, err := tx.Adjustment(t.Context(), in.ID); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("failed adjustment evidence escaped rollback")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAdjustmentPoliciesRemainLineScoped(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	effects := []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5}}}
	service, repo, scope, intent, quote := adjustmentPurchaseFixture(t, effects, now, "line-a", "line-b")
	firstInput := AdjustmentInput{Account: intent.Account, ID: "line-a-refund", IntentID: intent.ID, ProviderAdjustmentID: "provider-line-a", TransactionID: "adjustment-tx", Scope: scope, Kind: AdjustmentRefund, Currency: quote.Currency, Lines: []PaidLine{{LineID: "line-a", Gross: 50}}, PolicyVersion: "policy-1", CreditPolicy: CreditRefundFullOnly, Actor: "actor", Reason: "partial line refund", OccurredAt: now.Add(2 * time.Minute)}
	first, err := service.ApplyAdjustment(t.Context(), firstInput)
	if err != nil || !first.Applied || len(first.Effects) != 0 {
		t.Fatalf("full-only partial line result=%+v err=%v", first, err)
	}
	secondInput := firstInput
	secondInput.ID = "line-b-refund"
	secondInput.ProviderAdjustmentID = "provider-line-b"
	secondInput.Lines = []PaidLine{{LineID: "line-b", Gross: 50}}
	secondInput.CreditPolicy = CreditRefundProportional
	second, err := service.ApplyAdjustment(t.Context(), secondInput)
	if err != nil || !second.Applied || len(second.Effects) != 1 {
		t.Fatalf("proportional second line result=%+v err=%v", second, err)
	}
	if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(3 * time.Minute) }).Balance(t.Context(), intent.Account, "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 8 || balance.Revoked != 2 {
			return errors.New("line-scoped policy revoked the full-only line")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type failingAdjustmentRepository struct {
	Repository
	err error
}

func (r failingAdjustmentRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, account, func(tx Tx) error {
		return fn(failingAdjustmentTx{Tx: tx, err: r.err})
	})
}

type failingAdjustmentTx struct {
	Tx
	err error
}

func (tx failingAdjustmentTx) InsertReversal(context.Context, Reversal) error { return tx.err }

func adjustmentPurchaseFixture(t *testing.T, effects []Effect, now time.Time, lineIDs ...string) (*Service, Repository, billing.Scope, Intent, Quote) {
	t.Helper()
	repo := NewMemoryRepository(ReferenceAccount{Account: "adjustment-acct"})
	clock := now
	service := New(repo, func() time.Time { return clock })
	offer, err := service.PublishOffer(t.Context(), Offer{Account: "adjustment-acct", Revision: Revision{ID: "adjustment-offer", Version: 1}, Name: "Adjustment", Effects: effects})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), Price{Account: "adjustment-acct", Revision: Revision{ID: "adjustment-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	if len(lineIDs) == 0 {
		lineIDs = []string{"line"}
	}
	quoteLines := make([]QuoteLineInput, 0, len(lineIDs))
	for _, id := range lineIDs {
		quoteLines = append(quoteLines, QuoteLineInput{ID: id, Price: price.Revision, Quantity: 1})
	}
	quote, err := service.CreateQuote(t.Context(), QuoteInput{Account: "adjustment-acct", ID: "adjustment-quote", ValidUntil: now.Add(time.Hour), Lines: quoteLines})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "adjustment-provider", Merchant: "merchant", Environment: "sandbox"}
	intent, err := service.CreateIntent(t.Context(), IntentInput{Account: "adjustment-acct", ID: "adjustment-intent", Operation: "adjustment-operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "purchase", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Minute)
	paidLines := make([]PaidLine, 0, len(lineIDs))
	for _, id := range lineIDs {
		paidLines = append(paidLines, PaidLine{LineID: id, Gross: 100})
	}
	fact := PaymentFact{Account: intent.Account, Scope: scope, EventID: "adjustment-event", TransactionID: "adjustment-tx", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: paidLines, OccurredAt: clock, CollectedAt: clock}
	if result, err := service.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := service.Fulfill(t.Context(), intent.Account, intent.ID); err != nil {
		t.Fatal(err)
	}
	return service, repo, scope, intent, quote
}

func TestReferenceReversalMissingAdjustmentRollsBackCreditChanges(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	_, repo, _, intent, _ := adjustmentPurchaseFixture(t, []Effect{{Key: "host", Host: &HostBenefit{Kind: "workspace", Payload: []byte(`{}`)}}}, now)
	ctx := t.Context()
	err := repo.WithinAccount(ctx, intent.Account, func(tx Tx) error {
		rows, err := tx.Fulfillments(ctx, intent.ID)
		if err != nil {
			return err
		}
		host := rows[0]
		if _, err := credit.New(tx.Credits(), func() time.Time { return now }).Grant(ctx, credit.GrantInput{Account: intent.Account, Operation: "orphan-grant", LotID: "orphan-lot", Unit: billing.Unit{Code: "test", Scale: 1}, Amount: 100, Source: "test", SourceRef: "orphan", ValidFrom: now}); err != nil {
			return err
		}
		return tx.InsertReversal(ctx, Reversal{Account: intent.Account, ID: reversalID(intent.Account, host.ID), AdjustmentID: "absent", IntentID: intent.ID, Original: host, EffectiveAt: host.EffectiveAt, CreatedAt: host.CreatedAt, State: FulfillmentPending})
	})
	if err == nil {
		t.Fatal("orphan reversal committed")
	}
	if err := repo.WithinAccount(ctx, intent.Account, func(tx Tx) error {
		bal, err := credit.New(tx.Credits(), func() time.Time { return now }).Balance(ctx, intent.Account, "test", "")
		if err != nil {
			return err
		}
		if bal.Available != 0 {
			t.Fatalf("credit escaped deferred reference failure: %+v", bal)
		}
		rows, err := tx.Reversals(ctx, intent.ID)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Fatal("orphan reversal persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A bundled free benefit is paid at zero gross, so it can never be named in an
// adjustment line (those require a positive gross) and neither revocation
// branch could reach it. Its credits survived a full refund of the whole
// payment. They are contingent on the purchase, so a complete reversal must
// take them back; a partial refund must not.
func TestFullReversalRevokesBenefitsFromAZeroGrossLine(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	repo := NewMemoryRepository(ReferenceAccount{Account: "bonus-acct"})
	clock := now
	service := New(repo, func() time.Time { return clock })
	unit := billing.Unit{Code: "credits", Scale: 1}
	paidOffer, err := service.PublishOffer(t.Context(), Offer{Account: "bonus-acct", Revision: Revision{ID: "paid-offer", Version: 1}, Name: "Paid", Effects: []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: unit, Amount: 10}}}})
	if err != nil {
		t.Fatal(err)
	}
	bonusOffer, err := service.PublishOffer(t.Context(), Offer{Account: "bonus-acct", Revision: Revision{ID: "bonus-offer", Version: 1}, Name: "Bonus", Effects: []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: unit, Amount: 500}}}})
	if err != nil {
		t.Fatal(err)
	}
	paidPrice, err := service.PublishPrice(t.Context(), Price{Account: "bonus-acct", Revision: Revision{ID: "paid-price", Version: 1}, Offer: paidOffer.Revision, Currency: "USD", UnitAmount: 1000, TaxTreatment: TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	bonusPrice, err := service.PublishPrice(t.Context(), Price{Account: "bonus-acct", Revision: Revision{ID: "bonus-price", Version: 1}, Offer: bonusOffer.Revision, Currency: "USD", UnitAmount: 0, TaxTreatment: TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), QuoteInput{Account: "bonus-acct", ID: "bonus-quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{
		{ID: "line-paid", Price: paidPrice.Revision, Quantity: 1},
		{ID: "line-bonus", Price: bonusPrice.Revision, Quantity: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "bonus-provider", Merchant: "merchant", Environment: "sandbox"}
	intent, err := service.CreateIntent(t.Context(), IntentInput{Account: "bonus-acct", ID: "bonus-intent", Operation: "bonus-operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "purchase", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Minute)
	fact := PaymentFact{Account: intent.Account, Scope: scope, EventID: "bonus-event", TransactionID: "bonus-tx", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: []PaidLine{{LineID: "line-paid", Gross: 1000}, {LineID: "line-bonus", Gross: 0}}, OccurredAt: clock, CollectedAt: clock}
	if result, err := service.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := service.Fulfill(t.Context(), intent.Account, intent.ID); err != nil {
		t.Fatal(err)
	}
	balance := func() credit.Balance {
		t.Helper()
		var out credit.Balance
		if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
			var err error
			out, err = credit.New(tx.Credits(), func() time.Time { return clock }).Balance(t.Context(), intent.Account, "credits", "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := balance(); got.Available != 510 {
		t.Fatalf("after fulfilment available=%d, want 510", got.Available)
	}
	// A partial refund leaves the bonus alone.
	clock = now.Add(5 * time.Minute)
	partial := AdjustmentInput{Account: intent.Account, ID: "partial", IntentID: intent.ID, ProviderAdjustmentID: "provider-partial", TransactionID: "bonus-tx", Scope: scope, Kind: AdjustmentRefund, Currency: quote.Currency, Lines: []PaidLine{{LineID: "line-paid", Gross: 400}}, PolicyVersion: "p1", CreditPolicy: CreditRefundFullOnly, Actor: "actor", Reason: "partial", OccurredAt: now.Add(3 * time.Minute)}
	if result, err := service.ApplyAdjustment(t.Context(), partial); err != nil || !result.Applied {
		t.Fatalf("partial adjustment=%+v err=%v", result, err)
	}
	if got := balance(); got.Available != 510 {
		t.Fatalf("partial refund revoked the bonus: available=%d, want 510", got.Available)
	}
	// Reversing the remainder reverses the purchase, so the bonus goes too.
	full := partial
	full.ID = "remainder"
	full.ProviderAdjustmentID = "provider-remainder"
	full.Lines = []PaidLine{{LineID: "line-paid", Gross: 600}}
	full.OccurredAt = now.Add(4 * time.Minute)
	if result, err := service.ApplyAdjustment(t.Context(), full); err != nil || !result.Applied {
		t.Fatalf("remainder adjustment=%+v err=%v", result, err)
	}
	if got := balance(); got.Available != 0 || got.Revoked != 510 {
		t.Fatalf("full reversal left benefits: %+v, want available=0 revoked=510", got)
	}
}
