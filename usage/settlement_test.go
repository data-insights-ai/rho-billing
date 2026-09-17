package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

var settlementAccount = billing.AccountID("acct")

type settlementClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newSettlementClock() *settlementClock {
	return &settlementClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *settlementClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *settlementClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

func newSettlementService() (*SettlementService, SettlementRepository, *settlementClock) {
	clock := newSettlementClock()
	repo := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount})
	return NewSettlement(repo, clock.Now), repo, clock
}

func settlementPeriod(clock *settlementClock) BillingPeriod {
	start := clock.Now().Add(-time.Hour)
	end := clock.Now().Add(30 * 24 * time.Hour)
	cutoff := clock.Now().Add(24 * time.Hour)
	return BillingPeriod{Start: start, End: end, Cutoff: cutoff}
}

func postpaidRecord(t *testing.T, id string, amount int64, occurred, received time.Time, currency string) Record {
	t.Helper()
	rule, err := NewRule(RuleConfig{Version: "rating-v1", Kind: KindFixed, Target: Target{Currency: currency}, Rounding: RoundDown, FixedRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := Prepare(Observation{Account: settlementAccount, ID: id, Source: "meter", OccurredAt: occurred, Funding: Postpaid, Input: RateInput{ActionCount: amount}}, rule, received)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func finalize(t *testing.T, service *SettlementService, clock *settlementClock, batchID string, records []Record, period BillingPeriod) Batch {
	t.Helper()
	seedSettlementUsage(t, service, records)
	batch, err := service.finishClose(t.Context(), CloseInput{Account: settlementAccount, Operation: billing.OperationID("finalize-" + batchID), BatchID: batchID, Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestFinalizeFreezesPostpaidUsageAndOperationIdentity(t *testing.T) {
	service, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	first := postpaidRecord(t, "usage-b", 2, clock.Now(), clock.Now(), "USD")
	second := postpaidRecord(t, "usage-a", 3, clock.Now().Add(time.Hour), clock.Now(), "USD")
	seedSettlementUsage(t, service, []Record{first, second})
	input := CloseInput{Account: settlementAccount, Operation: "finalize-1", BatchID: "batch-1", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	batch, err := service.finishClose(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if batch.State != BatchReady || batch.Total != 5 || batch.Currency != "USD" || len(batch.Lines) != 2 {
		t.Fatalf("finalized batch = %+v", batch)
	}
	if batch.Lines[0].UsageID != "usage-a" || batch.Lines[1].UsageID != "usage-b" {
		t.Fatalf("lines were not deterministically ordered: %#v", batch.Lines)
	}
	retry, err := service.finishClose(context.Background(), input)
	if err != nil || retry.Fingerprint != batch.Fingerprint {
		t.Fatalf("reordered retry = %+v / %v", retry, err)
	}
	input.Currency = "EUR"
	if _, err = service.finishClose(context.Background(), input); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed operation payload error = %v", err)
	}
	stored, err := service.BatchSummary(context.Background(), settlementAccount, "batch-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.BatchSummary(context.Background(), settlementAccount, "batch-1")
	if err != nil || again.Total != stored.Total || again.LineCount != stored.LineCount {
		t.Fatalf("memory repository summary changed: stored=%+v again=%+v err=%v", stored, again, err)
	}
}

func TestPrepareBatchRejectsDuplicateCrossPeriodAndIncompatibleRecords(t *testing.T) {
	_, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	record := postpaidRecord(t, "duplicate", 1, clock.Now(), clock.Now(), "USD")
	duplicateInput := CloseInput{Account: settlementAccount, Operation: "duplicate-op", BatchID: "duplicate-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	if _, err := prepareBatch(duplicateInput, []Record{record, record}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate usage error = %v", err)
	}
	late := postpaidRecord(t, "late", 1, clock.Now(), clock.Now().Add(2*24*time.Hour), "USD")
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "late-op", BatchID: "late-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{late}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("post-cutoff record error = %v", err)
	}
	crossing := postpaidRecord(t, "crossing", 1, clock.Now().Add(12*time.Hour), clock.Now(), "USD")
	crossing.Observation.Interval = billing.Period{Start: clock.Now().Add(11 * time.Hour), End: clock.Now().Add(2 * 24 * time.Hour)}
	crossing.Fingerprint = Identity(crossing)
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "crossing-op", BatchID: "crossing-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{crossing}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("cross-period aggregate error = %v", err)
	}
	otherAccount := record
	otherAccount.Observation.Account = "other"
	otherAccount.Fingerprint = Identity(otherAccount)
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "account-op", BatchID: "account-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{otherAccount}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("foreign account error = %v", err)
	}
	euro := postpaidRecord(t, "euro", 1, clock.Now(), clock.Now(), "EUR")
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "currency-op", BatchID: "currency-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{euro}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("currency mismatch error = %v", err)
	}
}

func TestPrepareBatchRejectsPrepaidWaivedAndOverflow(t *testing.T) {
	_, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	ruleCredits, err := NewRule(RuleConfig{Version: "credits-v1", Kind: KindFixed, Target: Target{CreditUnit: "credits"}, Rounding: RoundDown, FixedRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	prepaid, err := Prepare(Observation{Account: settlementAccount, ID: "prepaid", Source: "meter", OccurredAt: clock.Now(), Funding: Prepaid, ReservationID: "reservation", Input: RateInput{ActionCount: 1}}, ruleCredits, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareBatch(CloseInput{Account: settlementAccount, Operation: "prepaid-op", BatchID: "prepaid-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{prepaid}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("prepaid error = %v", err)
	}
	ruleWaived, err := NewRule(RuleConfig{Version: "waived-v1", Kind: KindFixed, Target: Target{Currency: "USD"}, Rounding: RoundDown, FixedRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	waived, err := Prepare(Observation{Account: settlementAccount, ID: "waived", Source: "meter", OccurredAt: clock.Now(), Funding: Waived, Input: RateInput{ActionCount: 1, Waived: true}}, ruleWaived, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareBatch(CloseInput{Account: settlementAccount, Operation: "waived-op", BatchID: "waived-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{waived}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("waived error = %v", err)
	}
	max := postpaidRecord(t, "max", math.MaxInt64, clock.Now(), clock.Now(), "USD")
	one := postpaidRecord(t, "one", 1, clock.Now(), clock.Now(), "USD")
	if _, err = prepareBatch(CloseInput{Account: settlementAccount, Operation: "overflow-op", BatchID: "overflow-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}, []Record{max, one}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestPrepareBatchRequiresCompleteNonlinearPeriodAggregate(t *testing.T) {
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	period := BillingPeriod{Start: start, End: end, Cutoff: end}
	rule, err := NewRule(RuleConfig{Version: "aggregate-finalize", Kind: KindIncludedQuota, Target: Target{Currency: "USD"}, Rounding: RoundDown, Included: &IncludedQuotaConfig{Metric: "requests", Included: 1, Rate: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := Prepare(Observation{Account: settlementAccount, ID: "aggregate-complete", Source: "meter-a", OccurredAt: start.Add(time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "requests", Quantity: 3}}}}, rule, end)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "aggregate-complete-op", BatchID: "aggregate-complete-batch", Period: period, Currency: "USD", CreatedAt: end}, []Record{complete}); err != nil {
		t.Fatalf("complete aggregate rejected: %v", err)
	}
	individual := complete
	individual.Observation.ID = "aggregate-individual"
	individual.Observation.Interval = billing.Period{}
	individual.Fingerprint = Identity(individual)
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "aggregate-individual-op", BatchID: "aggregate-individual-batch", Period: period, Currency: "USD", CreatedAt: end}, []Record{individual}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("individual nonlinear usage error = %v", err)
	}
	duplicate := complete
	duplicate.Observation.ID = "aggregate-duplicate"
	duplicate.Observation.Source = "meter-b"
	duplicate.Fingerprint = Identity(duplicate)
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, Operation: "aggregate-duplicate-op", BatchID: "aggregate-duplicate-batch", Period: period, Currency: "USD", CreatedAt: end}, []Record{complete, duplicate}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate nonlinear usage error = %v", err)
	}
}

func TestPeriodGraceAndCloseReadiness(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	cutoff := end.Add(12 * time.Hour)
	period := BillingPeriod{Start: start, End: end, Cutoff: cutoff}
	record := postpaidRecord(t, "grace-usage", 1, start.Add(time.Hour), cutoff, "USD")
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, BatchID: "grace-batch", Period: period, Currency: "USD", CreatedAt: cutoff}, []Record{record}); err != nil {
		t.Fatalf("receipt in grace period rejected: %v", err)
	}
	afterEnd := postpaidRecord(t, "after-end", 1, end.Add(time.Hour), cutoff, "USD")
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, BatchID: "after-end-batch", Period: period, Currency: "USD", CreatedAt: cutoff}, []Record{afterEnd}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("occurrence after period end accepted: %v", err)
	}
	if _, err := prepareBatch(CloseInput{Account: settlementAccount, BatchID: "early-batch", Period: period, Currency: "USD", CreatedAt: end}, nil); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("early finalization accepted: %v", err)
	}
}

func TestCorrectionLinksLateUsageAndSignedAdjustments(t *testing.T) {
	service, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	originalRecord := postpaidRecord(t, "original-usage", 5, clock.Now(), clock.Now(), "USD")
	original := finalize(t, service, clock, "original-batch", []Record{originalRecord}, period)
	clock.Advance(2 * 24 * time.Hour)
	late := postpaidRecord(t, "late-usage", 3, period.Start.Add(2*time.Hour), clock.Now(), "USD")
	seedSettlementUsage(t, service, []Record{late})
	correctionInput := CorrectionInput{Account: settlementAccount, Operation: "correction-1", BatchID: "correction-batch", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, Adjustments: []Adjustment{{ID: "adj-1", OriginalUsageID: "original-usage", Amount: -1, Currency: "USD", Reason: "credit"}}, CreatedAt: clock.Now().Add(24 * time.Hour)}
	correction, err := service.Correct(context.Background(), correctionInput)
	if err != nil {
		t.Fatal(err)
	}
	if correction.ID != "correction-batch" || correction.Total != 2 || correction.LineCount != 2 || correction.State != BatchReady {
		t.Fatalf("correction summary = %+v", correction)
	}
	lines, _, more, err := service.BatchLinesPage(context.Background(), settlementAccount, correction.ID, "", 10)
	if err != nil || more || len(lines) != 2 {
		t.Fatalf("correction lines = %+v, more=%v, err=%v", lines, more, err)
	}
	unchanged, err := service.BatchSummary(context.Background(), settlementAccount, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Total != 5 || unchanged.State != BatchReady {
		t.Fatalf("original batch changed = %+v", unchanged)
	}
	if _, err = service.Correct(context.Background(), CorrectionInput{Account: settlementAccount, Operation: "correction-duplicate", BatchID: "correction-duplicate", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, CreatedAt: clock.Now().Add(24 * time.Hour)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("late usage billed twice error = %v", err)
	}
	if _, err = service.Correct(context.Background(), CorrectionInput{Account: settlementAccount, Operation: "correction-unknown", BatchID: "correction-unknown", OriginalBatchID: original.ID, Period: period, Currency: "USD", Adjustments: []Adjustment{{ID: "adj-unknown", OriginalUsageID: "missing", Amount: 1, Currency: "USD", Reason: "fix"}}, CreatedAt: clock.Now().Add(24 * time.Hour)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("unknown adjustment source error = %v", err)
	}
}

func TestSubmissionUnknownRequiresLookupAndFencesWorkers(t *testing.T) {
	service, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	record := postpaidRecord(t, "submit-usage", 7, clock.Now(), clock.Now(), "USD")
	finalize(t, service, clock, "submit-batch", []Record{record}, period)
	attempt, err := service.BeginSubmission(context.Background(), BeginSubmissionInput{Account: settlementAccount, Operation: "begin-submit", BatchID: "submit-batch", AttemptID: "attempt-1", Provider: "sim", IdempotencyKey: "idem-1", Capability: SettlementCapability{SupportsIdempotency: true, SupportsLookup: true}, CreatedAt: clock.Now()})
	if err != nil || attempt.State != AttemptSubmitting || attempt.Revision != 1 {
		t.Fatalf("begin submission = %+v / %v", attempt, err)
	}
	unknown, err := service.CompleteSubmission(context.Background(), CompleteSubmissionInput{Account: settlementAccount, Operation: "complete-unknown", BatchID: "submit-batch", AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: SubmissionUnknown, Reason: "request timed out", CompletedAt: clock.Now()})
	if err != nil || unknown.State != BatchUnknown || unknown.Revision != 2 {
		t.Fatalf("unknown submission = %+v / %v", unknown, err)
	}
	if _, err = service.BeginSubmission(context.Background(), BeginSubmissionInput{Account: settlementAccount, Operation: "blind-retry", BatchID: "submit-batch", AttemptID: "attempt-2", Provider: "sim", IdempotencyKey: "idem-2", Capability: SettlementCapability{SupportsIdempotency: true}, CreatedAt: clock.Now()}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("blind retry error = %v", err)
	}
	if _, err = service.CompleteSubmission(context.Background(), CompleteSubmissionInput{Account: settlementAccount, Operation: "stale-worker", BatchID: "submit-batch", AttemptID: "attempt-1", ExpectedRevision: 1, Status: SubmissionConfirmed, ProviderReference: "charge-stale", CompletedAt: clock.Now()}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale worker error = %v", err)
	}
	confirmed, err := service.ReconcileUnknown(context.Background(), ReconcileUnknownInput{Account: settlementAccount, Operation: "reconcile-submit", BatchID: "submit-batch", AttemptID: "attempt-1", ExpectedRevision: unknown.Revision, Status: SubmissionConfirmed, ProviderReference: "charge-authoritative", Evidence: "provider lookup", ReconciledAt: clock.Now()})
	if err != nil || confirmed.State != BatchConfirmed || confirmed.Revision != 3 {
		t.Fatalf("reconciled submission = %+v / %v", confirmed, err)
	}
	if _, err = service.ReconcileUnknown(context.Background(), ReconcileUnknownInput{Account: settlementAccount, Operation: "reconcile-stale", BatchID: "submit-batch", AttemptID: "attempt-1", ExpectedRevision: 2, Status: SubmissionConfirmed, ProviderReference: "charge-other", Evidence: "provider lookup", ReconciledAt: clock.Now()}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale reconciliation error = %v", err)
	}
}

func TestSubmissionRevisionOverflowIsRejected(t *testing.T) {
	batch := Batch{ID: "overflow-batch", State: BatchSubmitting, Revision: math.MaxInt64}
	attempt := Attempt{ID: "overflow-attempt", BatchID: batch.ID, State: AttemptSubmitting, Revision: math.MaxInt64, Capability: SettlementCapability{SupportsLookup: true}}
	if _, _, err := applyCompleteSubmission(batch, attempt, CompleteSubmissionInput{BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: math.MaxInt64, Status: SubmissionConfirmed, ProviderReference: "provider-charge", CompletedAt: time.Now()}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("completion revision overflow = %v", err)
	}
	batch.State = BatchUnknown
	attempt.State = AttemptUnknown
	if _, _, err := applyReconcileUnknown(batch, attempt, ReconcileUnknownInput{BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: math.MaxInt64, Status: SubmissionConfirmed, ProviderReference: "provider-charge", Evidence: "lookup", ReconciledAt: time.Now()}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("reconciliation revision overflow = %v", err)
	}
}

func TestSubmissionCapabilityAndTerminalIdempotency(t *testing.T) {
	service, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	finalize(t, service, clock, "terminal-batch", []Record{postpaidRecord(t, "terminal-usage", 1, clock.Now(), clock.Now(), "USD")}, period)
	if _, err := service.BeginSubmission(context.Background(), BeginSubmissionInput{Account: settlementAccount, Operation: "begin-capability", BatchID: "terminal-batch", AttemptID: "attempt-capability", Provider: "sim", IdempotencyKey: "idem-capability", CreatedAt: clock.Now()}); !errors.Is(err, ErrCapability) {
		t.Fatalf("missing capability error = %v", err)
	}
	attempt, err := service.BeginSubmission(context.Background(), BeginSubmissionInput{Account: settlementAccount, Operation: "begin-terminal", BatchID: "terminal-batch", AttemptID: "attempt-terminal", Provider: "sim", IdempotencyKey: "idem-terminal", Capability: SettlementCapability{SupportsIdempotency: true}, CreatedAt: clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := service.CompleteSubmission(context.Background(), CompleteSubmissionInput{Account: settlementAccount, Operation: "complete-terminal", BatchID: "terminal-batch", AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: SubmissionConfirmed, ProviderReference: "charge-terminal", CompletedAt: clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := service.CompleteSubmission(context.Background(), CompleteSubmissionInput{Account: settlementAccount, Operation: "complete-terminal", BatchID: "terminal-batch", AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: SubmissionConfirmed, ProviderReference: "charge-terminal", CompletedAt: clock.Now()})
	if err != nil || retry.Revision != confirmed.Revision || retry.State != BatchConfirmed {
		t.Fatalf("terminal retry = %+v / %v", retry, err)
	}
	if _, err = service.CompleteSubmission(context.Background(), CompleteSubmissionInput{Account: settlementAccount, Operation: "complete-terminal", BatchID: "terminal-batch", AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: SubmissionConfirmed, ProviderReference: "different-charge", CompletedAt: clock.Now()}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("terminal changed payload error = %v", err)
	}
}

// seedSettlementUsage is test-only ingestion into the reference transaction.
// It keeps the production settlement API free of usage-write responsibilities.
func seedSettlementUsage(t *testing.T, service *SettlementService, records []Record) {
	t.Helper()
	err := service.repo.WithinAccount(t.Context(), settlementAccount, func(tx SettlementTx) error {
		state := tx.(*settlementMemoryTx).state
		for _, record := range records {
			exists := false
			for _, old := range state.usage {
				if old.Observation.ID == record.Observation.ID {
					if old.Fingerprint != record.Fingerprint {
						return billing.ErrConflict
					}
					exists = true
				}
			}
			if !exists {
				state.usage = append(state.usage, Copy(record))
				state.watermark++
				state.sequences[record.Observation.ID] = state.watermark
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionRejectsMoreThanThousandInputsBeforeMutation(t *testing.T) {
	service, _, clock := newSettlementService()
	ids := make([]string, 1001)
	adjustments := make([]Adjustment, 1001)
	for i := range ids {
		ids[i] = fmt.Sprintf("late-%04d", i)
		adjustments[i] = Adjustment{ID: fmt.Sprintf("adjust-%04d", i), OriginalUsageID: "original", Amount: 1, Currency: "USD", Reason: "correction"}
	}
	_, err := service.Correct(t.Context(), CorrectionInput{Account: settlementAccount, Operation: "too-many", BatchID: "too-many-batch", OriginalBatchID: "original", Period: settlementPeriod(clock), Currency: "USD", UsageIDs: ids, Adjustments: adjustments, CreatedAt: clock.Now()})
	if !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("too many correction inputs error = %v", err)
	}
	if _, err := service.BatchSummary(t.Context(), settlementAccount, "too-many-batch"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("oversized correction mutated storage: %v", err)
	}
}

func TestBatchFromSummaryDoesNotMaterializeHistoricalLines(t *testing.T) {
	summary := BatchSummary{Account: settlementAccount, ID: "huge-header", LineCount: 1_000_000_000, State: BatchReady}
	batch := batchFromSummary(summary)
	if len(batch.Lines) != 0 || batch.ID != summary.ID || batch.OriginalBatchID != summary.OriginalBatchID {
		t.Fatalf("header conversion materialized or changed lines: len=%d batch=%+v", len(batch.Lines), batch)
	}
}

func TestSubmissionRejectsAttemptFromAnotherBatch(t *testing.T) {
	when := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	batch := Batch{ID: "batch-a", State: BatchSubmitting, Revision: 1}
	attempt := Attempt{ID: "attempt-b", BatchID: "batch-b", State: AttemptSubmitting, Revision: 1}
	_, _, err := applyCompleteSubmission(batch, attempt, CompleteSubmissionInput{BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: 1, Status: SubmissionConfirmed, ProviderReference: "provider-ref", CompletedAt: when})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("cross-batch completion error = %v", err)
	}
	unknownBatch := Batch{ID: "batch-a", State: BatchUnknown, Revision: 1}
	unknownAttempt := Attempt{ID: "attempt-b", BatchID: "batch-b", State: AttemptUnknown, Revision: 1, Capability: SettlementCapability{SupportsLookup: true}}
	_, _, err = applyReconcileUnknown(unknownBatch, unknownAttempt, ReconcileUnknownInput{BatchID: unknownBatch.ID, AttemptID: unknownAttempt.ID, ExpectedRevision: 1, Status: SubmissionRejected, Evidence: "lookup", ReconciledAt: when})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("cross-batch reconciliation error = %v", err)
	}
}

func TestCorrectionFingerprintSeparatesUsageFromAdjustments(t *testing.T) {
	// These payloads produced the same sequence of fingerprint fields before
	// section lengths were encoded. Reusing an operation must not alias them.
	usageOnly := CorrectionInput{Account: settlementAccount, Operation: "section-replay", BatchID: "correction", OriginalBatchID: "original", Currency: "USD", UsageIDs: []string{"1", "2", "3", "USD", "why"}}
	adjustmentOnly := usageOnly
	adjustmentOnly.UsageIDs = nil
	adjustmentOnly.Adjustments = []Adjustment{{ID: "1", OriginalUsageID: "2", Amount: 3, Currency: "USD", Reason: "why"}}
	if correctionFingerprint(usageOnly) == correctionFingerprint(adjustmentOnly) {
		t.Fatal("usage IDs and adjustment fields alias the same operation identity")
	}
}
