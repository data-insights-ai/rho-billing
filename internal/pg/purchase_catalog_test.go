package pg

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresPurchaseCatalogFreezesRevisionsAndQuotes(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-a")
	other := billing.AccountID("purchase-catalog-b")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, other, string(other)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	service := purchase.New(store.Purchases(), func() time.Time { return clock })

	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-basic", Version: 1}, Name: "Basic", Effects: []purchase.Effect{{
		Key: "host", Host: &purchase.HostBenefit{Kind: "provision", Payload: []byte(`{"tier":"basic"}`)},
	}}}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-basic", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 1250, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	changedOffer := offer
	changedOffer.Name = "Changed"
	if _, err := service.PublishOffer(ctx, changedOffer); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed offer revision error=%v, want conflict", err)
	}
	changedPrice := price
	changedPrice.UnitAmount++
	if _, err := service.PublishPrice(ctx, changedPrice); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed price revision error=%v, want conflict", err)
	}

	quoteInput := purchase.QuoteInput{
		Account: account, ID: "quote-basic", ValidUntil: now.Add(time.Hour),
		Lines: []purchase.QuoteLineInput{{ID: "line-basic", Price: price.Revision, Quantity: 2}},
	}
	quote, err := service.CreateQuote(ctx, quoteInput)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Amount != 2500 || quote.Currency != "USD" || quote.TaxTreatment != purchase.TaxExclusive || len(quote.Lines) != 1 {
		t.Fatalf("quote=%+v", quote)
	}
	newPrice := price
	newPrice.Revision = purchase.Revision{ID: "price-basic", Version: 2}
	newPrice.UnitAmount = 1500
	if _, err := service.PublishPrice(ctx, newPrice); err != nil {
		t.Fatal(err)
	}
	quote.Lines[0].Offer.Name = "mutated"
	quote.Lines[0].Offer.Effects[0].Host.Payload[0] = 'X'
	read, err := service.Quote(ctx, account, quote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Lines[0].Offer.Name != offer.Name || string(read.Lines[0].Offer.Effects[0].Host.Payload) != `{"tier":"basic"}` {
		t.Fatalf("quote read was not detached: %+v", read.Lines[0].Offer)
	}
	replay, err := service.CreateQuote(ctx, quoteInput)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Fingerprint() != read.Fingerprint() {
		t.Fatalf("quote replay changed frozen result: replay=%s read=%s", replay.Fingerprint(), read.Fingerprint())
	}
	if read.Lines[0].PriceSnapshot.Revision != price.Revision || read.Lines[0].PriceSnapshot.UnitAmount != price.UnitAmount {
		t.Fatalf("old quote changed after price revision: %+v", read.Lines[0].PriceSnapshot)
	}
	changedQuote := quoteInput
	changedQuote.Lines = []purchase.QuoteLineInput{{ID: "line-basic", Price: price.Revision, Quantity: 3}}
	if _, err := service.CreateQuote(ctx, changedQuote); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed quote replay error=%v, want conflict", err)
	}
	if _, err := purchase.New(store.Purchases(), func() time.Time { return clock }).Offer(ctx, other, offer.Revision); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account offer read error=%v, want not found", err)
	}

	clock = now.Add(2 * time.Hour)
	if _, err := service.Quote(ctx, account, quote.ID); err != nil {
		t.Fatalf("expired quote read error=%v", err)
	}
	if _, err := service.CreateQuote(ctx, quoteInput); err != nil {
		t.Fatalf("expired exact quote replay error=%v", err)
	}
	newExpired := quoteInput
	newExpired.ID = "quote-expired-new"
	if _, err := service.CreateQuote(ctx, newExpired); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("new expired quote error=%v, want invalid", err)
	}
}

func TestPostgresPurchaseCatalogRejectsCurrencyTaxMismatchAndOverflow(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-validation")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-validation", Version: 1}, Name: "Validation"}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	usd := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-usd", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}
	euro := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-eur", Version: 1}, Offer: offer.Revision, Currency: "EUR", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}
	inclusive := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-inclusive", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxInclusive}
	for _, price := range []purchase.Price{usd, euro, inclusive} {
		if _, err := service.PublishPrice(ctx, price); err != nil {
			t.Fatal(err)
		}
	}
	base := purchase.QuoteInput{Account: account, ID: "quote-mismatch", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-1", Price: usd.Revision, Quantity: 1}, {ID: "line-2", Price: euro.Revision, Quantity: 1}}}
	if _, err := service.CreateQuote(ctx, base); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("currency mismatch error=%v, want conflict", err)
	}
	base.ID = "quote-tax-mismatch"
	base.Lines[1].Price = inclusive.Revision
	if _, err := service.CreateQuote(ctx, base); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("tax mismatch error=%v, want conflict", err)
	}
	overflow := purchase.QuoteInput{Account: account, ID: "quote-overflow", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-overflow", Price: purchase.Revision{ID: "price-overflow", Version: 1}, Quantity: 2}}}
	overflowPrice := purchase.Price{Account: account, Revision: overflow.Lines[0].Price, Offer: offer.Revision, Currency: "USD", UnitAmount: math.MaxInt64, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(ctx, overflowPrice); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateQuote(ctx, overflow); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("amount overflow error=%v, want overflow", err)
	}
}

func TestPostgresPurchaseCatalogCallbackRollbackAndAccountIsolation(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-rollback")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-rollback", Version: 1}, Name: "Rollback", PublishedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	want := errors.New("abort purchase catalog transaction")
	if err := store.Purchases().WithinAccount(ctx, account, func(tx purchase.Tx) error {
		if err := tx.InsertOffer(ctx, offer); err != nil {
			return err
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("callback error=%v, want %v", err, want)
	}
	service := purchase.New(store.Purchases(), func() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) })
	if _, err := service.Offer(ctx, account, offer.Revision); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rolled-back offer read error=%v, want not found", err)
	}
}

func TestPostgresPurchaseCatalogQuoteCommitFailureReturnsZeroAndRetries(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-commit")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-commit", Version: 1}, Name: "Commit"}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-commit", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 10, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_purchase_quote() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'purchase quote commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_purchase_quote AFTER INSERT ON billing_purchase_quotes DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_purchase_quote()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fail_purchase_quote ON billing_purchase_quotes`)
		_, _ = db.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS fail_purchase_quote()`)
	}()
	in := purchase.QuoteInput{Account: account, ID: "quote-commit", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-commit", Price: price.Revision, Quantity: 1}}}
	if result, err := service.CreateQuote(ctx, in); err == nil || result.ID != "" {
		t.Fatalf("failed quote result=%+v err=%v", result, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_quotes WHERE account_id=$1`, account).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed quote persisted rows=%d", count)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_purchase_quote ON billing_purchase_quotes`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION fail_purchase_quote()`); err != nil {
		t.Fatal(err)
	}
	if result, err := service.CreateQuote(ctx, in); err != nil || result.ID != in.ID {
		t.Fatalf("retry quote result=%+v err=%v", result, err)
	}
}

func TestPostgresPurchaseCatalogHostJSONFingerprintAndLargeInteger(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-json")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-json", Version: 1}, Name: "JSON", Effects: []purchase.Effect{{
		Key: "host", Host: &purchase.HostBenefit{Kind: "payload", Payload: []byte(`{"z":1,"n":9007199254740993,"a":2,"e":1e3,"d":1.2300e-2,"neg":-0}`)},
	}}}
	published, err := service.PublishOffer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	spelled := offer
	spelled.Effects = []purchase.Effect{{
		Key: "host", Host: &purchase.HostBenefit{Kind: "payload", Payload: []byte(`{"neg":0,"d":0.01230,"e":1000,"a":2,"n":9007199254740993,"z":1}`)},
	}}
	replayed, err := service.PublishOffer(ctx, spelled)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Fingerprint() != published.Fingerprint() {
		t.Fatalf("equivalent numeric spellings changed offer fingerprint: first=%s replay=%s", published.Fingerprint(), replayed.Fingerprint())
	}
	read, err := service.Offer(ctx, account, offer.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if read.Fingerprint() != published.Fingerprint() {
		t.Fatalf("Postgres changed canonical host JSON fingerprint: published=%s read=%s payload=%s", published.Fingerprint(), read.Fingerprint(), read.Effects[0].Host.Payload)
	}
	if string(read.Effects[0].Host.Payload) != `{"a":2,"d":0.0123,"e":1000,"n":9007199254740993,"neg":0,"z":1}` {
		t.Fatalf("unexpected canonical host JSON=%s", read.Effects[0].Host.Payload)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-json", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 1, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "quote-json", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-json", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if quote.Fingerprint() == "" || quote.Lines[0].Offer.Fingerprint() != published.Fingerprint() {
		t.Fatalf("quote did not retain canonical offer fingerprint: quote=%s offer=%s", quote.Fingerprint(), quote.Lines[0].Offer.Fingerprint())
	}
	readQuote, err := service.Quote(ctx, account, quote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readQuote.Fingerprint() != quote.Fingerprint() || readQuote.Lines[0].Offer.Fingerprint() != published.Fingerprint() {
		t.Fatalf("Postgres changed quote canonical fingerprint: wrote=%s read=%s", quote.Fingerprint(), readQuote.Fingerprint())
	}
}

func TestPostgresPurchaseCatalogSameQuoteAcrossPhysicalConnections(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("purchase-catalog-concurrent")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-concurrent", Version: 1}, Name: "Concurrent"}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-concurrent", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 20, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	secondDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	if _, err := secondDB.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	secondStore := New(secondDB)
	second := purchase.New(secondStore.Purchases(), func() time.Time { return now })
	in := purchase.QuoteInput{Account: account, ID: "quote-concurrent", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-concurrent", Price: price.Revision, Quantity: 1}}}
	results := make(chan error, 2)
	go func() { _, e := service.CreateQuote(ctx, in); results <- e }()
	go func() { _, e := second.CreateQuote(ctx, in); results <- e }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("same quote concurrent create error=%v", err)
		}
	}
}
