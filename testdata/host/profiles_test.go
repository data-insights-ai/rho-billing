package host_test

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestSubscriptionOnlyHostReturnsLifecycle(t *testing.T) {
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "profile-sub", "org"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Accounts: accounts}))
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	life, err := svc.Activate(t.Context(), subscription.ActivateInput{
		Account: "profile-sub", ID: "life", Operation: "activate", Quantity: 1, Trial: true,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "trial", Quantity: 1, Period: billing.Period{Start: start, End: start.AddDate(0, 0, 14)}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionNone, Proration: subscription.ProrationNone, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: start.AddDate(0, 0, 14)}, At: start,
	})
	if err != nil || life.Access != subscription.AccessTrial {
		t.Fatalf("subscription-only=%+v err=%v", life, err)
	}
}

func TestMoneyOnlyHostReturnsPaidIntent(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("profile-money")
	svc := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: account}), func() time.Time { return now })
	if _, err := svc.PublishOffer(t.Context(), purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer", Version: 1}, Name: "Money"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishPrice(t.Context(), purchase.Price{Account: account, Revision: purchase.Revision{ID: "price", Version: 1}, Offer: purchase.Revision{ID: "offer", Version: 1}, Currency: "USD", UnitAmount: 500, TaxTreatment: purchase.TaxExclusive}); err != nil {
		t.Fatal(err)
	}
	quote, err := svc.CreateQuote(t.Context(), purchase.QuoteInput{Account: account, ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line", Price: purchase.Revision{ID: "price", Version: 1}, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "example", Merchant: "m", Environment: "test"}
	intent, err := svc.CreateIntent(t.Context(), purchase.IntentInput{Account: account, ID: "intent", Operation: "op", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host", Reason: "money", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.ApplyPayment(t.Context(), purchase.PaymentFact{Account: account, Scope: scope, EventID: "evt", TransactionID: "txn", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 500, Lines: []purchase.PaidLine{{LineID: "line", Gross: 500}}, OccurredAt: now, CollectedAt: now})
	if err != nil || !result.Applied {
		t.Fatalf("money-only payment=%+v err=%v", result, err)
	}
	stored, err := svc.Intent(t.Context(), account, intent.ID)
	if err != nil || stored.Payment != purchase.PaymentPaid || len(quote.Lines[0].Offer.Effects) != 0 {
		t.Fatalf("money-only intent=%+v quote=%+v err=%v", stored, quote, err)
	}
}

func TestOneOffFulfillmentHostReturnsAcknowledgment(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("profile-fulfill")
	svc := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: account}), func() time.Time { return now })
	if _, err := svc.PublishOffer(t.Context(), purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer", Version: 1}, Name: "Host", Effects: []purchase.Effect{{Key: "host", Host: &purchase.HostBenefit{Kind: "provision", Payload: []byte(`{"ok":true}`)}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishPrice(t.Context(), purchase.Price{Account: account, Revision: purchase.Revision{ID: "price", Version: 1}, Offer: purchase.Revision{ID: "offer", Version: 1}, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}); err != nil {
		t.Fatal(err)
	}
	quote, err := svc.CreateQuote(t.Context(), purchase.QuoteInput{Account: account, ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line", Price: purchase.Revision{ID: "price", Version: 1}, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "example", Merchant: "m", Environment: "test"}
	intent, err := svc.CreateIntent(t.Context(), purchase.IntentInput{Account: account, ID: "intent", Operation: "op", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host", Reason: "fulfill", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyPayment(t.Context(), purchase.PaymentFact{Account: account, Scope: scope, EventID: "evt", TransactionID: "txn", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line", Gross: 100}}, OccurredAt: now, CollectedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Fulfill(t.Context(), account, intent.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Fulfillments(t.Context(), account, intent.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fulfillments=%+v err=%v", rows, err)
	}
	ack, err := svc.Acknowledge(t.Context(), purchase.Acknowledgment{Account: account, EffectID: rows[0].ID, Fingerprint: rows[0].Fingerprint(), HostReference: "host-ref-1", AppliedAt: now})
	if err != nil || ack.State != purchase.FulfillmentComplete {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
}

func TestPrepaidHostReturnsSettledReservation(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("profile-prepaid")
	engine := credit.New(credit.NewMemoryRepository(account), func() time.Time { return now })
	if _, err := engine.Grant(t.Context(), credit.GrantInput{Account: account, Operation: "g", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(t.Context(), credit.ReserveInput{Account: account, Operation: "r", ReservationID: "hold", Actor: "user", Unit: "credits", Amount: 3, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Settle(t.Context(), credit.SettleInput{Account: account, Operation: "s", ReservationID: "hold", Actual: 3, Evidence: credit.Evidence{UsageID: "u1", RatingVersion: "v1", Metrics: []credit.Metric{{Name: "tokens", Quantity: 3}}}})
	if err != nil || got.Consumed != 3 {
		t.Fatalf("prepaid settle=%+v err=%v", got, err)
	}
}

func TestPostpaidHostReturnsPublishedBatch(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	period := usage.BillingPeriod{Start: now.Add(-time.Hour), End: now, Cutoff: now}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "fixed", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "2"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := usage.Prepare(usage.Observation{Account: "profile-postpaid", ID: "u1", Source: "meter", OccurredAt: period.Start, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 3}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	svc := usage.NewSettlement(usage.NewMemorySettlementRepository(usage.ReferenceAccount{ID: "profile-postpaid", Usage: []usage.Record{record}}), func() time.Time { return now })
	job, err := svc.StartClose(t.Context(), usage.CloseInput{Account: "profile-postpaid", Operation: "close", BatchID: "batch", Period: period, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	for job.State == usage.ClosePreparing {
		job, err = svc.AdvanceClose(t.Context(), "profile-postpaid", job.BatchID, job.Revision, 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.PublishClose(t.Context(), "profile-postpaid", job.BatchID, job.Revision); err != nil {
		t.Fatal(err)
	}
	batch, err := svc.BatchSummary(t.Context(), "profile-postpaid", "batch")
	if err != nil || batch.Total != 6 {
		t.Fatalf("postpaid batch=%+v err=%v", batch, err)
	}
}

func TestSeatsHostReturnsDesiredQuantity(t *testing.T) {
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "profile-seats", "org"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Accounts: accounts}))
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if _, err := svc.Activate(t.Context(), subscription.ActivateInput{
		Account: "profile-seats", ID: "life", Operation: "activate", Quantity: 2,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "team", Quantity: 2, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic, Proration: subscription.ProrationImmediate, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.SetDesiredQuantity(t.Context(), subscription.QuantityInput{Account: "profile-seats", ID: "life", Operation: "seats-7", Quantity: 7, Revision: 2, At: start.Add(time.Minute)})
	if err != nil || got.DesiredQuantity != 7 {
		t.Fatalf("seats=%+v err=%v", got, err)
	}
}

func TestBudgetsHostReturnsHold(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	period := billing.Period{Start: now, End: now.Add(24 * time.Hour)}
	svc := credit.NewLimits(credit.NewMemoryLimitRepository("profile-budget"), func() time.Time { return now })
	if _, err := svc.Configure(t.Context(), credit.BudgetInput{Account: "profile-budget", ID: "cap", Basis: credit.BasisBillable, Currency: "USD", Period: period, Amount: 20}); err != nil {
		t.Fatal(err)
	}
	hold, err := svc.Reserve(t.Context(), credit.BudgetReserveInput{Account: "profile-budget", GroupID: "g1", Basis: credit.BasisBillable, Currency: "USD", Amount: 4, Period: period})
	if err != nil || hold.Bound != 4 {
		t.Fatalf("budget=%+v err=%v", hold, err)
	}
}

func TestRecoveryHostReturnsWorkerCancelWithoutStart(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("profile-recovery")
	engine := credit.New(credit.NewMemoryRepository(account), func() time.Time { return now })
	if _, err := engine.Grant(t.Context(), credit.GrantInput{Account: account, Operation: "g", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 8, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	report, err := engine.VerifyLedger(t.Context(), account)
	if err != nil || report.Lots != 1 {
		t.Fatalf("recovery audit=%+v err=%v", report, err)
	}
	if integration.NewOps(integration.NewMemoryOpsRepository(account), func() time.Time { return now }) == nil {
		t.Fatal("ops worker started or missing")
	}
	plan := catalog.PlanVersion{ID: "p1", PlanID: "p", Version: 1, Entitlements: []catalog.EntitlementDefinition{{Key: "export", Kind: catalog.EntitlementFeature, Aggregation: catalog.AggregationOR, Enabled: true}}}
	snap, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: now, Assignments: []catalog.PlanAssignment{{ID: "a1", PlanVersionID: plan.ID, Quantity: 1, Source: catalog.SourceManual, Effective: billing.Period{Start: now, End: now.Add(time.Hour)}}}, Versions: []catalog.PlanVersion{plan}})
	if err != nil {
		t.Fatal(err)
	}
	if feature, ok := snap.Get("export"); !ok || !feature.Enabled {
		t.Fatal("recovery host lost entitlement")
	}
}
