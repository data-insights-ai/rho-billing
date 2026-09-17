package pg

import (
	"errors"
	"reflect"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresAccountsCatalogAndMappingHistory(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	ref := catalog.AccountReference{Account: "acct-a", Ref: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "customer-a"}, Kind: "customer"}
	if err := store.Link(ctx, ref); err != nil {
		t.Fatal(err)
	}
	ref.Account = "acct-b"
	if err := store.Link(ctx, ref); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("cross-account provider reference should conflict: %v", err)
	}
	if _, err := store.Resolve(ctx, ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account resolve should be hidden: %v", err)
	}

	p1 := testPlan("plan-v1", 1, 10, 100)
	p2 := testPlan("plan-v2", 2, 20, 100)
	if err := store.PublishPlan(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, p1); err != nil {
		t.Fatal(err)
	}
	changed := p1
	changed.Entitlements[0].Amount = 99
	if err := store.PublishPlan(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed immutable plan should conflict: %v", err)
	}
	scaleChange := testPlan("plan-v3", 3, 30, 1000)
	if err := store.PublishPlan(ctx, scaleChange); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("unit scale change should conflict: %v", err)
	}

	scopeA := catalog.MappingScope{Provider: "sim", Account: "acct-a", Environment: "sandbox", Merchant: "merchant"}
	mapping := catalog.PriceMapping{Scope: scopeA, ExternalPriceID: "price-pro", Target: catalog.MappingTarget{Kind: catalog.TargetPlanVersion, ID: p1.ID}, Revision: 1}
	if err := store.PutPriceMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	mapping.Revision = 2
	mapping.Target.ID = p2.ID
	if err := store.PutPriceMapping(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	old, err := store.PriceMappingAtRevision(ctx, scopeA, "price-pro", 1)
	if err != nil || old.Target.ID != p1.ID {
		t.Fatalf("historical mapping = %#v, err %v", old, err)
	}
	latest, err := store.PriceMapping(ctx, scopeA, "price-pro")
	if err != nil || latest.Target.ID != p2.ID || latest.Revision != 2 {
		t.Fatalf("latest mapping = %#v, err %v", latest, err)
	}
	scopeB := scopeA
	scopeB.Account = "acct-b"
	if _, err := store.PriceMapping(ctx, scopeB, "price-pro"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("mapping crossed account boundary: %v", err)
	}
	unknown := mapping
	unknown.Revision = 3
	unknown.Target.ID = "missing-plan"
	if err := store.PutPriceMapping(ctx, unknown); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("dangling mapping should fail: %v", err)
	}
}

func TestPostgresSubscriptionStaleEqualTimeReconcileAndSessionScope(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	plan := testPlan("subscription-plan", 1, 10, 100)
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub-a"}
	period := billing.Period{Start: testTime(), End: testTime().Add(24 * time.Hour)}
	snapshot := subscription.Snapshot{
		Account: "acct-a", Ref: ref, Status: "active",
		Items:       []subscription.Item{{ID: "item-a", PlanVersion: plan.ID, Quantity: 1, Period: period}},
		Assignments: []catalog.PlanAssignment{{ID: "assignment-a", PlanVersionID: plan.ID, Quantity: 1, Effective: period, Source: catalog.SourceSubscription}},
	}
	at := testTime()
	first, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: snapshot, EventID: "event-1", OccurredAt: at})
	if err != nil || first.Revision != 1 {
		t.Fatalf("initial subscription = %#v, err %v", first, err)
	}
	stale := snapshot
	stale.Status = "paused"
	if got, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: stale, EventID: "event-stale", OccurredAt: at.Add(-time.Minute)}); err != nil || got.Status != "active" || got.Revision != 1 {
		t.Fatalf("stale observation changed state: %#v, err %v", got, err)
	}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: stale, EventID: "event-equal", OccurredAt: at}); !errors.Is(err, subscription.ErrReconcile) {
		t.Fatalf("equal-time disagreement should require reconcile: %v", err)
	}
	reconciled, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: stale, EventID: "reconcile-1", OccurredAt: at.Add(time.Minute), Reconcile: true, ExpectedRevision: 1})
	if err != nil || reconciled.Status != "paused" || reconciled.Revision != 2 {
		t.Fatalf("reconcile result = %#v, err %v", reconciled, err)
	}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: snapshot, EventID: "reconcile-raced", OccurredAt: at.Add(2 * time.Minute), Reconcile: true, ExpectedRevision: 1}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale reconcile CAS should conflict: %v", err)
	}
	if err := store.Atomic(ctx, "acct-a", func(session integration.Session) error {
		_, err := subscription.New(session.Subscriptions()).Subscription(ctx, "acct-b", ref)
		return err
	}); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("session crossed account scope: %v", err)
	}
	if _, err := subscription.New(store.Subscriptions()).Subscription(ctx, "acct-b", ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("subscription leaked across account: %v", err)
	}
}

func TestPostgresSubscriptionHistoricalEventFenceAndOwnership(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	plan := testPlan("subscription-history-plan", 1, 10, 100)
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub-history"}
	period := billing.Period{Start: testTime(), End: testTime().Add(24 * time.Hour)}
	first := subscription.Observation{Snapshot: subscription.Snapshot{
		Account: "acct-a", Ref: ref, Status: "active",
		Items:       []subscription.Item{{ID: "item-history", PlanVersion: plan.ID, Quantity: 1, Period: period}},
		Assignments: []catalog.PlanAssignment{{ID: "assignment-history", PlanVersionID: plan.ID, Quantity: 1, Effective: period, Source: catalog.SourceSubscription}},
	}, EventID: "history-old", OccurredAt: testTime()}
	original, err := subscription.New(store.Subscriptions()).Observe(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	later := first
	later.EventID = "history-new"
	later.OccurredAt = testTime().Add(time.Minute)
	later.Snapshot.Status = "paused"
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, later); err != nil {
		t.Fatal(err)
	}
	replayed, err := subscription.New(store.Subscriptions()).Observe(ctx, first)
	if err != nil || !reflect.DeepEqual(replayed, original) {
		t.Fatalf("historical replay = %#v, err %v; want %#v", replayed, err, original)
	}
	changed := first
	changed.Snapshot.Status = "cancelled"
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed historical event should conflict: %v", err)
	}
	other := first
	other.EventID = "history-other-account"
	other.Snapshot.Account = "acct-b"
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, other); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("provider subscription ownership should conflict: %v", err)
	}
}

func TestPostgresSubscriptionItemPeriodsRemainIndependent(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	plan := testPlan("subscription-item-period-plan", 1, 10, 100)
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := testTime()
	firstPeriod := billing.Period{Start: start, End: start.Add(time.Hour)}
	secondPeriod := billing.Period{Start: start.Add(2 * time.Hour), End: start.Add(4 * time.Hour)}
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub-item-periods"}
	in := subscription.Observation{Snapshot: subscription.Snapshot{
		Account: "acct-a", Ref: ref, Status: "active",
		Items: []subscription.Item{
			{ID: "item-first", PlanVersion: plan.ID, Quantity: 1, Period: firstPeriod},
			{ID: "item-second", PlanVersion: plan.ID, Quantity: 2, Period: secondPeriod},
		},
	}, EventID: "item-periods", OccurredAt: start}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := subscription.New(store.Subscriptions()).Subscription(ctx, "acct-a", ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Period != firstPeriod || got.Items[1].Period != secondPeriod {
		t.Fatalf("subscription item periods changed or inherited: %#v", got.Items)
	}
}

func TestPostgresSubscriptionFreshReconcileAfterYears(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub-years-old"}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	first := subscription.Observation{Snapshot: subscription.Snapshot{Account: "acct-a", Ref: ref, Status: "active"}, EventID: "old-state", OccurredAt: old}
	if got, err := subscription.New(store.Subscriptions()).Observe(ctx, first); err != nil || got.Revision != 1 {
		t.Fatalf("old subscription = %#v, err %v", got, err)
	}
	fresh := subscription.Observation{Snapshot: subscription.Snapshot{Account: "acct-a", Ref: ref, Status: "paused"}, EventID: "fresh-reconcile", OccurredAt: testTime(), Reconcile: true, ExpectedRevision: 1}
	got, err := subscription.New(store.Subscriptions()).Observe(ctx, fresh)
	if err != nil || got.Status != "paused" || got.Revision != 2 || !got.SourceTime.Equal(billing.CanonicalTime(fresh.OccurredAt)) {
		t.Fatalf("fresh reconciliation = %#v, err %v", got, err)
	}
}

func TestPostgresRatingUsageImmutableAndAggregateOverlap(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	config := usage.RuleConfig{Version: "usage-v1", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	changed := config
	changed.Weights = map[string]string{"tokens": "2"}
	if err := store.PublishRating(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed rating version should conflict: %v", err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	start := testTime()
	firstObservation := usage.Observation{Account: "acct-a", ID: "usage-a", Source: "api", OccurredAt: start.Add(time.Hour), Interval: billing.Period{Start: start, End: start.Add(2 * time.Hour)}, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 5}}}}
	first, err := usage.Prepare(firstObservation, rule, start.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := recordUsage(ctx, store, first)
	if err != nil || stored.Fingerprint != first.Fingerprint {
		t.Fatalf("stored usage = %#v, err %v", stored, err)
	}
	retry, err := recordUsage(ctx, store, first)
	if err != nil || retry.ReceivedAt != stored.ReceivedAt {
		t.Fatalf("duplicate usage outcome changed: %#v, err %v", retry, err)
	}
	tampered := first
	tampered.Rating.RoundedAmount++
	if _, err := recordUsage(ctx, store, tampered); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("tampered rating should fail validation: %v", err)
	}
	overlapObservation := firstObservation
	overlapObservation.ID = "usage-overlap"
	overlapObservation.Input.Metrics = []usage.MetricQuantity{{Name: "tokens", Quantity: 3}}
	overlap, err := usage.Prepare(overlapObservation, rule, start.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, overlap); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlapping aggregate should conflict: %v", err)
	}
	accountB := firstObservation
	accountB.Account = "acct-b"
	accountB.ID = "usage-b"
	accountB.Interval = billing.Period{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}
	accountB.OccurredAt = start.Add(2*time.Hour + time.Minute)
	recordB, err := usage.Prepare(accountB, rule, start.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, recordB); err != nil {
		t.Fatal(err)
	}
	page, err := readUsagePage(ctx, store, "acct-a", billing.Period{Start: start, End: start.Add(24 * time.Hour)}, "", 10)
	if err != nil || len(page) != 1 || page[0].Observation.ID != firstObservation.ID {
		t.Fatalf("usage page = %#v, err %v", page, err)
	}
}

func TestPostgresSettlePrepaidAtomicRollbackAndDuplicateOutcome(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	now := testTime()
	nowFn := func() time.Time { return now }
	ruleConfig := usage.RuleConfig{Version: "prepaid-v1", Kind: usage.KindFixed, Target: usage.Target{CreditUnit: "credits"}, Rounding: usage.RoundDown, FixedRate: "2"}
	if err := store.PublishRating(ctx, ruleConfig); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, ruleConfig.Version)
	if err != nil {
		t.Fatal(err)
	}
	engine := credit.New(store, nowFn)
	unit := billing.Unit{Code: "credits", Scale: 1}
	grant := credit.GrantInput{Account: "acct-a", Operation: "grant-prepaid-a", LotID: "lot-prepaid-a", Unit: unit, Amount: 10, Source: "purchase", SourceRef: "payment-a", ValidFrom: now, ExpiresAt: now.Add(time.Hour)}
	firstGrant, err := engine.Grant(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := engine.Grant(ctx, grant); err != nil || retry != firstGrant {
		t.Fatalf("duplicate grant outcome changed: %#v, err %v", retry, err)
	}
	grantConflict := grant
	grantConflict.Amount = 9
	if _, err := engine.Grant(ctx, grantConflict); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed duplicate grant should conflict: %v", err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve-prepaid-a", ReservationID: "reservation-a", Actor: "actor-a", Unit: "credits", Scope: "AI_STANDARD", Amount: 4, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	observation := usage.Observation{Account: "acct-a", ID: "prepaid-usage-a", Source: "api", OccurredAt: now.Add(time.Minute), Funding: usage.Prepaid, ReservationID: "reservation-a", Input: usage.RateInput{ActionCount: 2}}
	for _, mismatch := range []usage.Observation{
		func() usage.Observation { v := observation; v.Actor = "foreign-actor"; return v }(),
		func() usage.Observation { v := observation; v.CreditScope = "AI_RESEARCH"; return v }(),
	} {
		if _, _, err := integration.SettlePrepaid(ctx, store, mismatch, rule, "settle-prepaid-a", nowFn); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("unauthorized attribution accepted: %v", err)
		}
		held, err := engine.Reservation(ctx, "acct-a", "reservation-a")
		if err != nil || held.State != "held" || held.Consumed != 0 {
			t.Fatalf("attribution rejection consumed hold: %+v %v", held, err)
		}
		if _, err := readUsage(ctx, store, "acct-a", observation.ID); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("attribution rejection wrote usage: %v", err)
		}
	}
	result, record, err := integration.SettlePrepaid(ctx, store, observation, rule, "settle-prepaid-a", nowFn)
	if err != nil || result.Consumed != 4 || record.Fingerprint == "" {
		t.Fatalf("prepaid settlement = %#v, %#v, err %v", result, record, err)
	}
	settled, err := engine.Reservation(ctx, "acct-a", "reservation-a")
	if record.Observation.Actor != "actor-a" || record.Observation.CreditScope != "AI_STANDARD" {
		t.Fatalf("missing reservation attribution: %+v", record.Observation)
	}
	if err != nil || settled.State != "settled" || settled.Consumed != 4 {
		t.Fatalf("settled reservation = %#v, err %v", settled, err)
	}
	if _, err := readUsage(ctx, store, "acct-a", observation.ID); err != nil {
		t.Fatal(err)
	}

	grantB := grant
	grantB.Operation = "grant-prepaid-b"
	grantB.LotID = "lot-prepaid-b"
	grantB.SourceRef = "payment-b"
	if _, err := engine.Grant(ctx, grantB); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve-prepaid-b", ReservationID: "reservation-b", Actor: "actor-a", Unit: "credits", Scope: "AI_STANDARD", Amount: 6, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	conflictingObservation := observation
	conflictingObservation.Input.ActionCount = 3
	conflictingObservation.ReservationID = "reservation-b"
	if _, _, err := integration.SettlePrepaid(ctx, store, conflictingObservation, rule, "settle-prepaid-b", nowFn); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("usage conflict should roll back credit settle: %v", err)
	}
	rolledBack, err := engine.Reservation(ctx, "acct-a", "reservation-b")
	if err != nil || rolledBack.State != "held" || rolledBack.Consumed != 0 || rolledBack.Authorized != 6 {
		t.Fatalf("credit settlement was not rolled back: %#v, err %v", rolledBack, err)
	}
	balance, err := engine.Balance(ctx, "acct-a", "credits", "")
	if err != nil || balance.Available != 10 || balance.Held != 6 || balance.Consumed != 4 {
		t.Fatalf("balance after rollback = %#v, err %v", balance, err)
	}
}

func createTestAccounts(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, "acct-b", "subject-b"); err != nil {
		t.Fatal(err)
	}
}

func testPlan(id string, version, amount, scale int64) catalog.PlanVersion {
	return catalog.PlanVersion{ID: id, PlanID: "plan", Version: version, Entitlements: []catalog.EntitlementDefinition{{Key: "credits", Kind: catalog.EntitlementLimit, Aggregation: catalog.AggregationMAX, Amount: amount, Unit: billing.Unit{Code: "credits", Scale: scale}}}}
}

func testTime() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) }
