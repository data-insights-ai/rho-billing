package pg

import (
	"errors"
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

func settlementTestPeriod() usage.BillingPeriod {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return usage.BillingPeriod{Start: start, End: start.Add(48 * time.Hour), Cutoff: start.Add(36 * time.Hour)}
}

func settlementTestRecord(t *testing.T, store *Store, account, id string, occurred, received time.Time, count int64) usage.Record {
	t.Helper()
	ruleConfig := usage.RuleConfig{Version: "settlement-test-v1", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(t.Context(), ruleConfig); err != nil && !errors.Is(err, billing.ErrConflict) {
		t.Fatal(err)
	}
	rule, err := store.Rating(t.Context(), ruleConfig.Version)
	if err != nil {
		t.Fatal(err)
	}
	record, err := usage.Prepare(usage.Observation{Account: billing.AccountID(account), ID: id, Source: "meter", OccurredAt: occurred, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: count}}, rule, received)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPostgresSettlementFinalizesCompleteStoredUsageAndClaimsOnce(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "settlement-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	first := settlementTestRecord(t, store, "acct-a", "usage-b", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	second := settlementTestRecord(t, store, "acct-a", "usage-a", period.Start.Add(2*time.Hour), period.Start.Add(3*time.Hour), 3)
	if _, err := recordUsage(ctx, store, first); err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, second); err != nil {
		t.Fatal(err)
	}

	// Finalization has no caller-supplied Records. The service selects the
	// complete stored set while holding the account transaction.
	input := usage.CloseInput{Account: "acct-a", Operation: "close-1", BatchID: "batch-1", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, input)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Total != 5 || len(batch.Lines) != 2 || batch.Lines[0].UsageID != "usage-a" || batch.Lines[1].UsageID != "usage-b" {
		t.Fatalf("stored usage was not fully finalized: %+v", batch)
	}
	got, err := readPagedSettlementBatch(ctx, store.Settlements(), store.now, "acct-a", "batch-1")
	if err != nil || got.Total != batch.Total || got.Currency != batch.Currency || got.State != batch.State || !slices.Equal(got.Lines, batch.Lines) {
		t.Fatalf("persisted batch = %+v, err %v", got, err)
	}

	// A different operation and batch cannot close the same original period.
	conflict := input
	conflict.Operation, conflict.BatchID = "close-2", "batch-2"
	if _, err := finishSettlementClose(ctx, store.Settlements(), store.now, conflict); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("period identity conflict = %v", err)
	}
	// The rejection outcome is durable and idempotent.
	if _, err := finishSettlementClose(ctx, store.Settlements(), store.now, conflict); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("replayed rejection = %v", err)
	}
	claim, err := readPagedSettlementBatch(ctx, store.Settlements(), store.now, "acct-a", "batch-1")
	if err != nil || claim.Total != 5 {
		t.Fatalf("claim changed after rejection: %+v, %v", claim, err)
	}
}

func TestPostgresSettlementReplayIgnoresLateBackdatedUsageAndFixesCutoffIdentity(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "settlement-replay-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	first := settlementTestRecord(t, store, "acct-a", "usage-replay-first", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	if _, err := recordUsage(ctx, store, first); err != nil {
		t.Fatal(err)
	}
	input := usage.CloseInput{Account: "acct-a", Operation: "close-replay", BatchID: "batch-replay", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, input)
	if err != nil {
		t.Fatal(err)
	}
	lateBackdated := settlementTestRecord(t, store, "acct-a", "usage-replay-late", period.Start.Add(3*time.Hour), period.Cutoff.Add(-time.Minute), 4)
	if _, err := recordUsage(ctx, store, lateBackdated); err != nil {
		t.Fatal(err)
	}
	replayed, err := finishSettlementClose(ctx, store.Settlements(), store.now, input)
	if err != nil || replayed.Fingerprint != original.Fingerprint || replayed.Total != original.Total {
		t.Fatalf("replay changed durable close: %+v, %v", replayed, err)
	}
	changedCutoff := input
	changedCutoff.Period.Cutoff = period.Cutoff.Add(time.Hour)
	changedCutoff.CreatedAt = changedCutoff.Period.Cutoff
	if _, err := finishSettlementClose(ctx, store.Settlements(), store.now, changedCutoff); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed cutoff should conflict with fixed period identity: %v", err)
	}
}

func TestPostgresSettlementAttributesSubscriptionItemsAndRejectsCrossScopeCorrection(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "settlement-scope-subject"); err != nil {
		t.Fatal(err)
	}
	plan := testPlan("settlement-scope-plan", 1, 10, 100)
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub-scope"}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "acct-a", Ref: ref, Status: "active", Items: []subscription.Item{{ID: "item-a", PlanVersion: plan.ID, Quantity: 1, Period: billing.Period{Start: period.Start, End: period.End}}, {ID: "item-b", PlanVersion: plan.ID, Quantity: 1, Period: billing.Period{Start: period.Start, End: period.End}}}}, EventID: "scope-event", OccurredAt: period.Start}); err != nil {
		t.Fatal(err)
	}
	scopeA := usage.BillingScope{Subscription: ref, ItemID: "item-a"}
	scopeB := usage.BillingScope{Subscription: ref, ItemID: "item-b"}
	if err := store.CreateAccount(ctx, "acct-b", "settlement-scope-other-account"); err != nil {
		t.Fatal(err)
	}
	foreign := settlementTestRecord(t, store, "acct-b", "usage-foreign-scope", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 1)
	foreign.Observation.Scope = scopeA
	foreign.Fingerprint = usage.Identity(foreign)
	if _, err := recordUsage(ctx, store, foreign); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account subscription scope = %v", err)
	}
	recordA := settlementTestRecord(t, store, "acct-a", "usage-item-a", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	recordA.Observation.Scope = scopeA
	recordA.Fingerprint = usage.Identity(recordA)
	recordB := settlementTestRecord(t, store, "acct-a", "usage-item-b", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 3)
	recordB.Observation.Scope = scopeB
	recordB.Fingerprint = usage.Identity(recordB)
	if _, err := recordUsage(ctx, store, recordA); err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, recordB); err != nil {
		t.Fatal(err)
	}
	periodA := period
	periodA.Scope = scopeA
	periodB := period
	periodB.Scope = scopeB
	batchA, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "close-item-a", BatchID: "batch-item-a", Period: periodA, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil || batchA.Total != 2 || batchA.Period.Scope != scopeA {
		t.Fatalf("item A batch = %+v, %v", batchA, err)
	}
	batchB, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "close-item-b", BatchID: "batch-item-b", Period: periodB, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil || batchB.Total != 3 || batchB.Period.Scope != scopeB {
		t.Fatalf("item B batch = %+v, %v", batchB, err)
	}
	repo := usage.NewSettlement(store.Settlements(), time.Now)
	if _, err := repo.Correct(ctx, usage.CorrectionInput{Account: "acct-a", Operation: "correct-cross-scope", BatchID: "batch-cross-scope", OriginalBatchID: batchA.ID, Period: periodB, Currency: "USD", Adjustments: []usage.Adjustment{{ID: "adjust-cross-scope", OriginalUsageID: "usage-item-a", Amount: -1, Currency: "USD", Reason: "wrong item"}}, CreatedAt: period.Cutoff}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("cross-scope correction = %v", err)
	}
}

func TestPostgresSettlementSubmissionCASAndUnknownRecovery(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "settlement-submit-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, "acct-a", "usage-submit", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 7)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "close-submit", BatchID: "batch-submit", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	repo := usage.NewSettlement(store.Settlements(), time.Now)
	attempt, err := repo.BeginSubmission(ctx, usage.BeginSubmissionInput{Account: "acct-a", Operation: "begin-submit", BatchID: batch.ID, AttemptID: "attempt-1", Provider: "sim", IdempotencyKey: "idem-1", Capability: usage.SettlementCapability{SupportsIdempotency: true, SupportsLookup: true}, CreatedAt: period.Cutoff})
	if err != nil || attempt.Revision != 1 {
		t.Fatalf("begin = %+v, %v", attempt, err)
	}
	unknown, err := repo.CompleteSubmission(ctx, usage.CompleteSubmissionInput{Account: "acct-a", Operation: "complete-submit", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: usage.SubmissionUnknown, Reason: "timeout", CompletedAt: period.Cutoff})
	if err != nil || unknown.State != usage.BatchUnknown {
		t.Fatalf("unknown completion = %+v, %v", unknown, err)
	}
	if _, err := repo.CompleteSubmission(ctx, usage.CompleteSubmissionInput{Account: "acct-a", Operation: "stale-submit", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: 1, Status: usage.SubmissionConfirmed, ProviderReference: "charge", CompletedAt: period.Cutoff}); !errors.Is(err, usage.ErrStaleRevision) {
		t.Fatalf("stale completion = %v", err)
	}
	confirmed, err := repo.ReconcileUnknown(ctx, usage.ReconcileUnknownInput{Account: "acct-a", Operation: "reconcile-submit", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: unknown.Revision, Status: usage.SubmissionConfirmed, ProviderReference: "charge-authoritative", Evidence: "provider lookup", ReconciledAt: period.Cutoff})
	if err != nil || confirmed.State != usage.BatchConfirmed || confirmed.Revision != 3 {
		t.Fatalf("reconciled = %+v, %v", confirmed, err)
	}
}

func TestPostgresSettlementPersistsLinkedLateCorrection(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "settlement-correction-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	originalRecord := settlementTestRecord(t, store, "acct-a", "usage-original", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 5)
	if _, err := recordUsage(ctx, store, originalRecord); err != nil {
		t.Fatal(err)
	}
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "close-correction", BatchID: "batch-original", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	late := settlementTestRecord(t, store, "acct-a", "usage-late", period.Start.Add(2*time.Hour), period.Cutoff.Add(time.Hour), 3)
	if _, err := recordUsage(ctx, store, late); err != nil {
		t.Fatal(err)
	}
	repo := usage.NewSettlement(store.Settlements(), time.Now)
	correction, err := repo.Correct(ctx, usage.CorrectionInput{Account: "acct-a", Operation: "correct-1", BatchID: "batch-correction", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, Adjustments: []usage.Adjustment{{ID: "adjust-1", OriginalUsageID: "usage-original", Amount: -1, Currency: "USD", Reason: "credit"}}, CreatedAt: period.Cutoff.Add(time.Hour)})
	correctionBatch, pageErr := readPagedSettlementBatch(ctx, store.Settlements(), store.now, "acct-a", correction.ID)
	if err != nil || pageErr != nil || correctionBatch.OriginalBatchID != original.ID || correction.Total != 2 || len(correctionBatch.Lines) != 2 {
		t.Fatalf("correction = %+v, batch=%+v, %v", correction, correctionBatch, errors.Join(err, pageErr))
	}
	unchanged, err := readPagedSettlementBatch(ctx, store.Settlements(), store.now, "acct-a", original.ID)
	if err != nil || unchanged.Total != 5 || unchanged.OriginalBatchID != "" {
		t.Fatalf("original changed = %+v, %v", unchanged, err)
	}
	if _, err := repo.Correct(ctx, usage.CorrectionInput{Account: "acct-a", Operation: "correct-2", BatchID: "batch-correction-2", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, CreatedAt: period.Cutoff.Add(2 * time.Hour)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate late claim = %v", err)
	}
}
