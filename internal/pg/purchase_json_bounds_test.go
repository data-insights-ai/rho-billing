package pg

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPurchaseHostPayloadFormattingDoesNotExceedSemanticLimit(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "json-bounds", "json-bounds"); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]int, 6000)
	for i := range 6000 {
		values[fmt.Sprintf("k%04d", i)] = 0
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 65536 {
		t.Fatal("fixture must fit canonical payload limit")
	}
	now := testTime()
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	offer := purchase.Offer{Account: "json-bounds", Revision: purchase.Revision{ID: "offer", Version: 1}, Name: "Payload", Effects: []purchase.Effect{{Key: "host", Host: &purchase.HostBenefit{Kind: "fixture", Payload: raw}}}}
	stored, err := service.PublishOffer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := service.Offer(ctx, offer.Account, offer.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Fingerprint() != stored.Fingerprint() || string(fetched.Effects[0].Host.Payload) != string(raw) {
		t.Fatal("canonical payload changed across JSONB formatting")
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: offer.Account, Revision: purchase.Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "EUR", UnitAmount: 1, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: offer.Account, ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Quote(ctx, billing.AccountID("json-bounds"), quote.ID)
	if err != nil || got.Fingerprint() != quote.Fingerprint() {
		t.Fatalf("quote roundtrip: %v", err)
	}
}
