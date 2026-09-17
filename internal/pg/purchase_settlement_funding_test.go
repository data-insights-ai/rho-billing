package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresPurchaseSettlesExistingUsageExactlyOnce(t *testing.T) {
	store, db := testStore(t)
	batch, _ := boundSettlementFixture(t, store)
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	offer, err := service.PublishOffer(ctx, purchase.Offer{Account: batch.Account, Revision: purchase.Revision{ID: "usage-offer", Version: 1}, Name: "Usage", Effects: []purchase.Effect{{Key: "usage", Settlement: &purchase.SettlementBenefit{}}}})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: batch.Account, Revision: purchase.Revision{ID: "usage-price", Version: 1}, Offer: offer.Revision, Currency: batch.Currency, UnitAmount: batch.Total, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: batch.Account, ID: "usage-quote", ValidUntil: testTime().Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "usage-line", Price: price.Revision, Quantity: 1, SettlementBatchID: batch.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	in := purchase.IntentInput{Account: batch.Account, ID: "usage-purchase", Operation: "buy-usage", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, Actor: "owner", Reason: "settle usage", ExpiresAt: quote.ValidUntil}
	intent, err := service.CreateIntent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	fact := purchase.PaymentFact{Account: batch.Account, IntentID: intent.ID, Scope: in.Scope, EventID: "paid-usage", TransactionID: "usage-tx", Status: purchase.FactPaid, Currency: batch.Currency, Gross: batch.Total, Lines: []purchase.PaidLine{{LineID: "usage-line", Gross: batch.Total}}, CollectedAt: testTime(), OccurredAt: testTime()}
	if out, err := service.ApplyPayment(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("payment=%+v %v", out, err)
	}
	if out, err := service.Fulfill(ctx, batch.Account, intent.ID); err != nil || out.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("fulfillment=%+v %v", out, err)
	}
	var links int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_settlement_funding WHERE account_id=$1 AND batch_id=$2`, batch.Account, batch.ID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("funding links=%d %v", links, err)
	}
	original, err := usage.NewSettlement(store.Settlements(), store.now).BatchSummary(ctx, batch.Account, batch.ID)
	if err != nil || original.Total != batch.Total || original.State != batch.State || original.LineCount != int64(len(batch.Lines)) {
		t.Fatalf("purchase altered usage evidence=%+v %v", original, err)
	}
	// Another approved purchase cannot fund the same usage batch again.
	in.ID = "duplicate-usage"
	in.Operation = "duplicate-buy"
	if _, err := service.CreateIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	fact.IntentID = in.ID
	fact.EventID = "duplicate-event"
	fact.TransactionID = "duplicate-tx"
	out, err := service.ApplyPayment(ctx, fact)
	if err == nil || out.Applied {
		t.Fatalf("duplicate settlement accepted=%+v %v", out, err)
	}
	current, err := service.Intent(ctx, batch.Account, in.ID)
	if err != nil || current.Payment != purchase.PaymentPending {
		t.Fatalf("failed settlement committed=%+v %v", current, err)
	}
}
