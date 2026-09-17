package pg

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func insertEstimateRecord(t *testing.T, db *sql.DB, record usage.Record) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	o := record.Observation
	_, err = db.ExecContext(t.Context(), `INSERT INTO billing_usage(account_id,usage_id,source,occurred_at,interval_start,interval_end,funding,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, o.Account, o.ID, o.Source, o.OccurredAt, nullTime(o.Interval.Start), nullTime(o.Interval.End), string(o.Funding), o.Scope.Subscription.Scope.Provider, o.Scope.Subscription.Scope.Merchant, o.Scope.Subscription.Scope.Environment, o.Scope.Subscription.ID, o.Scope.ItemID, record.Fingerprint, raw)
	if err != nil {
		t.Fatal(err)
	}
}

func estimateTestPeriod() usage.EstimatePeriod {
	period := settlementTestPeriod()
	return usage.EstimatePeriod{Start: period.Start, End: period.End, Cutoff: period.Cutoff, Scope: period.Scope}
}

func TestPostgresUsageEstimateMatchesFrozenFinalizationAndStoredRuleEvidence(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "estimate-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	first := settlementTestRecord(t, store, "acct-a", "estimate-a", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	second := settlementTestRecord(t, store, "acct-a", "estimate-b", period.Start.Add(3*time.Hour), period.Start.Add(4*time.Hour), 3)
	if _, err := recordUsage(ctx, store, first); err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, second); err != nil {
		t.Fatal(err)
	}
	input := usage.EstimateInput{Account: "acct-a", Period: estimateTestPeriod(), Currency: "USD", Limit: 1}
	pending, err := store.EstimateUsage(ctx, input)
	if err != nil || pending.Status != usage.StatusEstimated || pending.Total != 5 || len(pending.Lines) != 1 || pending.NextAfter == "" {
		t.Fatalf("pending estimate = %+v, %v", pending, err)
	}
	page, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: input.Period, Currency: "USD", After: pending.NextAfter, Limit: 1})
	if err != nil || page.Total != pending.Total || len(page.Lines) != 1 || page.NextAfter != "" {
		t.Fatalf("second pending page = %+v, %v", page, err)
	}
	if err := store.PublishRating(ctx, usage.RuleConfig{Version: "settlement-test-v2", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "9"}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: input.Period, Currency: "USD", Limit: 10})
	if err != nil || unchanged.Total != 5 || unchanged.Lines[0].RuleVersion != first.Rating.RuleVersion {
		t.Fatalf("published rule changed stored estimate = %+v, %v", unchanged, err)
	}

	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "estimate-close", BatchID: "estimate-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: input.Period, Currency: "USD", Limit: 10})
	if err != nil || finalized.Status != usage.StatusFinalized || finalized.BatchID != batch.ID || finalized.Total != pending.Total || len(finalized.Lines) != 2 {
		t.Fatalf("finalized estimate = %+v, %v", finalized, err)
	}
	exact, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", BatchID: batch.ID, Period: input.Period, Currency: "USD", Limit: 10})
	if err != nil || exact.BatchID != batch.ID || exact.Total != finalized.Total {
		t.Fatalf("exact frozen estimate = %+v, %v", exact, err)
	}
}

func TestPostgresUsageEstimateKeepsFinalizedAmountAndPagesCorrectionKinds(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "estimate-correction-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	originalRecord := settlementTestRecord(t, store, "acct-a", "estimate-original", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	if _, err := recordUsage(ctx, store, originalRecord); err != nil {
		t.Fatal(err)
	}
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "estimate-close-correction", BatchID: "estimate-original-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	late := settlementTestRecord(t, store, "acct-a", "estimate-late", period.Start.Add(3*time.Hour), period.Cutoff.Add(time.Hour), 4)
	if _, err := recordUsage(ctx, store, late); err != nil {
		t.Fatal(err)
	}
	finalized, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: estimateTestPeriod(), Currency: "USD", Limit: 10})
	if err != nil || finalized.Status != usage.StatusFinalized || finalized.BatchID != original.ID || finalized.Total != 2 || len(finalized.Lines) != 1 {
		t.Fatalf("late usage changed original estimate = %+v, %v", finalized, err)
	}

	service := usage.NewSettlement(store.Settlements(), time.Now)
	correction, err := service.Correct(ctx, usage.CorrectionInput{Account: "acct-a", Operation: "estimate-correct-late", BatchID: "estimate-correction-batch", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, Adjustments: []usage.Adjustment{{ID: "estimate-adjustment", OriginalUsageID: originalRecord.Observation.ID, Amount: -1, Currency: "USD", Reason: "late credit"}}, CreatedAt: period.Cutoff.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", BatchID: correction.ID, Period: estimateTestPeriod(), Currency: "USD", Limit: 1})
	if err != nil || first.Total != correction.Total || len(first.Lines) != 1 || first.Lines[0].Kind != "adjustment" || first.NextAfter == "" {
		t.Fatalf("correction first page = %+v, %v", first, err)
	}
	second, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", BatchID: correction.ID, Period: estimateTestPeriod(), Currency: "USD", After: first.NextAfter, Limit: 1})
	if err != nil || second.Total != correction.Total || len(second.Lines) != 1 || second.Lines[0].Kind != "usage" || second.NextAfter != "" {
		t.Fatalf("correction second page = %+v, %v", second, err)
	}
}

func TestPostgresUsageEstimateRejectsIntegerOverflow(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "estimate-overflow-subject"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "estimate-overflow", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "9223372036854775807"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	period := estimateTestPeriod()
	for _, id := range []string{"estimate-overflow-a", "estimate-overflow-b"} {
		record, prepareErr := usage.Prepare(usage.Observation{Account: "acct-a", ID: id, Source: "meter", OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, period.Start.Add(2*time.Hour))
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if _, putErr := recordUsage(ctx, store, record); putErr != nil {
			t.Fatal(putErr)
		}
	}
	_, err = store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: period, Currency: "USD", Limit: 10})
	if !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("estimate overflow = %v", err)
	}
}

func TestPostgresUsageEstimateStreamsFullRangeAndPagesBounded(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("estimate-streaming")
	if err := store.CreateAccount(ctx, account, "estimate-streaming-subject"); err != nil {
		t.Fatal(err)
	}
	period := estimateTestPeriod()
	config := usage.RuleConfig{Version: "estimate-streaming-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1001 {
		record, err := usage.Prepare(usage.Observation{Account: account, ID: fmt.Sprintf("stream-%04d", i), Source: "meter", OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, period.Start.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		insertEstimateRecord(t, db, record)
	}
	late, err := usage.Prepare(usage.Observation{Account: account, ID: "stream-late", Source: "meter", OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, period.Cutoff.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	insertEstimateRecord(t, db, late)
	input := usage.EstimateInput{Account: account, Period: period, Currency: "USD", Limit: 2}
	first, err := store.EstimateUsage(ctx, input)
	if err != nil || first.Total != 1001 || len(first.Lines) != 2 || first.NextAfter == "" {
		t.Fatalf("first streaming page=%+v err=%v", first, err)
	}
	second, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: period, Currency: "USD", After: first.NextAfter, Limit: 1000})
	if err != nil || second.Total != 1001 || len(second.Lines) != 999 || second.NextAfter != "" {
		t.Fatalf("second streaming page=%+v err=%v", second, err)
	}
}

func TestPostgresUsageEstimateUsesBytewiseKeysetOrder(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("estimate-key-order")
	if err := store.CreateAccount(ctx, account, "estimate-key-order-subject"); err != nil {
		t.Fatal(err)
	}
	period := estimateTestPeriod()
	config := usage.RuleConfig{Version: "estimate-key-order-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"A-key", "a-key"} {
		record, err := usage.Prepare(usage.Observation{Account: account, ID: id, Source: "meter", OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, period.Start.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		insertEstimateRecord(t, db, record)
	}
	first, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: period, Currency: "USD", Limit: 1})
	if err != nil || len(first.Lines) != 1 || first.Lines[0].UsageID != "A-key" || first.NextAfter == "" {
		t.Fatalf("first mixed-case page=%+v err=%v", first, err)
	}
	second, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: period, Currency: "USD", After: first.NextAfter, Limit: 1})
	if err != nil || len(second.Lines) != 1 || second.Lines[0].UsageID != "a-key" || second.NextAfter != "" {
		t.Fatalf("second mixed-case page=%+v err=%v", second, err)
	}
}

func TestPostgresUsageEstimateHidesPreparingAndCancelingBatches(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("estimate-hidden-batch")
	if err := store.CreateAccount(ctx, account, "estimate-hidden-batch-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "hidden-batch-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 1)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: account, Operation: "hidden-batch-close", BatchID: "hidden-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"preparing", "canceling"} {
		if _, err := db.ExecContext(ctx, `UPDATE billing_settlement_batches SET state=$3 WHERE account_id=$1 AND batch_id=$2`, account, batch.ID, state); err != nil {
			t.Fatalf("set hidden batch state %q: %v", state, err)
		}
		pending, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: estimateTestPeriod(), Currency: "USD", Limit: 10})
		if err != nil || pending.Status != usage.StatusEstimated || pending.BatchID != "" {
			t.Fatalf("implicit estimate for %q = %+v, err=%v; want pending", state, pending, err)
		}
		if _, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, BatchID: batch.ID, Period: estimateTestPeriod(), Currency: "USD", Limit: 10}); !errors.Is(err, billing.ErrNotFound) {
			t.Fatalf("explicit hidden batch %q error=%v, want not found", state, err)
		}
	}
}

func TestPostgresUsageEstimateValidatesRecordsOutsidePage(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("estimate-corrupt-page")
	if err := store.CreateAccount(ctx, account, "estimate-corrupt-page-subject"); err != nil {
		t.Fatal(err)
	}
	period := estimateTestPeriod()
	config := usage.RuleConfig{Version: "estimate-corrupt-page-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"corrupt-page-a", "corrupt-page-b"} {
		record, err := usage.Prepare(usage.Observation{Account: account, ID: id, Source: "meter", OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, period.Start.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		insertEstimateRecord(t, db, record)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_usage SET record='{}'::jsonb WHERE account_id=$1 AND usage_id=$2`, account, "corrupt-page-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: period, Currency: "USD", Limit: 1}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("corrupt record outside page error=%v, want invalid", err)
	}
}

func TestPostgresUsageEstimateRejectsDuplicateNonlinearAggregates(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("estimate-duplicate-aggregate")
	if err := store.CreateAccount(ctx, account, "estimate-duplicate-aggregate-subject"); err != nil {
		t.Fatal(err)
	}
	period := estimateTestPeriod()
	period.Cutoff = period.End
	config := usage.RuleConfig{Version: "estimate-duplicate-aggregate-rule", Kind: usage.KindIncludedQuota, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Included: &usage.IncludedQuotaConfig{Metric: "tokens", Included: 0, Rate: "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aggregate-a", "aggregate-b"} {
		record, err := usage.Prepare(usage.Observation{Account: account, ID: id, Source: "meter", OccurredAt: period.Start.Add(time.Hour), Interval: billing.Period{Start: period.Start, End: period.End}, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule, period.Start.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		insertEstimateRecord(t, db, record)
	}
	if _, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: account, Period: period, Currency: "USD", Limit: 1}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate nonlinear aggregate error=%v, want conflict", err)
	}
}
