package usage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestBoundedCloseUsesMultiplePages(t *testing.T) {
	clock := newSettlementClock()
	period := settlementPeriod(clock)
	records := make([]Record, 1001)
	for i := range records {
		records[i] = postpaidRecord(t, "many-"+formatCloseID(i), 1, period.Start.Add(time.Duration(i)*time.Second), period.Cutoff, "USD")
	}
	repo := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: records})
	service := NewSettlement(repo, clock.Now)
	job, err := service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "many-op", BatchID: "many-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1000)
	if err != nil || job.Processed != 1000 || job.State != ClosePreparing {
		t.Fatalf("first page=%+v err=%v", job, err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1000)
	if err != nil || job.Processed != 1001 || job.State != CloseReady {
		t.Fatalf("second page=%+v err=%v", job, err)
	}
}

func TestBoundedCloseCancellationCleansInPages(t *testing.T) {
	clock := newSettlementClock()
	period := settlementPeriod(clock)
	records := []Record{postpaidRecord(t, "cancel-a", 1, clock.Now(), period.Cutoff, "USD"), postpaidRecord(t, "cancel-b", 1, clock.Now().Add(time.Hour), period.Cutoff, "USD")}
	repo := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: records})
	service := NewSettlement(repo, clock.Now)
	job, err := service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "cancel-op", BatchID: "cancel-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.CancelClose(t.Context(), settlementAccount, job.BatchID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil || job.State != CloseCanceled {
		t.Fatalf("cancel cleanup=%+v err=%v", job, err)
	}
	if _, err = service.BatchSummary(t.Context(), settlementAccount, job.BatchID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("canceled batch visible: %v", err)
	}
}

func TestBoundedCloseCancellationFollowsIngestionSequence(t *testing.T) {
	clock := newSettlementClock()
	period := settlementPeriod(clock)
	records := []Record{
		postpaidRecord(t, "z-sequence", 1, clock.Now(), period.Cutoff, "USD"),
		postpaidRecord(t, "a-sequence", 1, clock.Now().Add(time.Hour), period.Cutoff, "USD"),
	}
	repo := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: records})
	service := NewSettlement(repo, clock.Now)
	job, err := service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "sequence-cancel", BatchID: "sequence-cancel-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 10)
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.CancelClose(t.Context(), settlementAccount, job.BatchID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil || job.State != CloseCanceling {
		t.Fatalf("first cleanup=%+v err=%v", job, err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil || job.State != CloseCanceled {
		t.Fatalf("second cleanup=%+v err=%v", job, err)
	}
	if _, err = service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "sequence-reuse", BatchID: "sequence-reuse-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}); err != nil {
		t.Fatalf("close key not reusable: %v", err)
	}
}

func TestCloseReplayConflictAndCommitFailure(t *testing.T) {
	clock := newSettlementClock()
	period := settlementPeriod(clock)
	record := postpaidRecord(t, "replay-close", 2, clock.Now(), period.Cutoff, "USD")
	base := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: []Record{record}})
	service := NewSettlement(base, clock.Now)
	in := CloseInput{Account: settlementAccount, Operation: "replay-op", BatchID: "replay-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	first, err := service.StartClose(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.StartClose(t.Context(), in)
	if err != nil || replay.BatchID != first.BatchID || replay.CreatedAt != first.CreatedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.Currency = "EUR"
	if _, err := service.StartClose(t.Context(), changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed close err=%v", err)
	}
	failure := errors.New("commit failed")
	baseFail := NewMemorySettlementRepository(ReferenceAccount{ID: "acct-fail"})
	failing := &failingCloseRepository{SettlementRepository: baseFail, failure: failure}
	failed := NewSettlement(failing, clock.Now)
	result, err := failed.StartClose(t.Context(), CloseInput{Account: "acct-fail", Operation: "failed-op", BatchID: "failed-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if !errors.Is(err, failure) || result.BatchID != "" {
		t.Fatalf("failed close result=%+v err=%v", result, err)
	}
}

type failingCloseRepository struct {
	SettlementRepository
	failure error
}

func (r *failingCloseRepository) WithinClose(ctx context.Context, account billing.AccountID, fn func(CloseTx) error) error {
	if err := r.SettlementRepository.WithinClose(ctx, account, fn); err != nil {
		return err
	}
	return r.failure
}

func formatCloseID(i int) string { return fmt.Sprintf("%04d", i) }

func TestBoundedCloseResumesPublishesAndPages(t *testing.T) {
	clock := newSettlementClock()
	period := settlementPeriod(clock)
	first := postpaidRecord(t, "close-a", 2, period.Start.Add(timeHour), period.Cutoff, "USD")
	second := postpaidRecord(t, "close-b", 3, period.Start.Add(2*timeHour), period.Cutoff, "USD")
	repo := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: []Record{first, second}})
	service := NewSettlement(repo, clock.Now)
	job, err := service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "close-op", BatchID: "close-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil || job.Processed != 1 || job.State != ClosePreparing {
		t.Fatalf("first checkpoint=%+v err=%v", job, err)
	}
	job, err = service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 1)
	if err != nil || job.Processed != 2 || job.State != CloseReady {
		t.Fatalf("final checkpoint=%+v err=%v", job, err)
	}
	job, err = service.PublishClose(t.Context(), settlementAccount, job.BatchID, job.Revision)
	if err != nil || job.State != ClosePublished {
		t.Fatalf("published=%+v err=%v", job, err)
	}
	summary, err := service.BatchSummary(t.Context(), settlementAccount, job.BatchID)
	if err != nil || summary.Total != 5 || summary.LineCount != 2 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	lines, next, more, err := service.BatchLinesPage(t.Context(), settlementAccount, job.BatchID, "", 1)
	if err != nil || len(lines) != 1 || !more || next == "" {
		t.Fatalf("page1=%+v next=%q more=%v err=%v", lines, next, more, err)
	}
	lines, _, more, err = service.BatchLinesPage(t.Context(), settlementAccount, job.BatchID, next, 1)
	if err != nil || len(lines) != 1 || more {
		t.Fatalf("page2=%+v more=%v err=%v", lines, more, err)
	}
	_, err = service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "close-other", BatchID: "close-other", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlap close err=%v", err)
	}
}

const timeHour = 60 * 60 * 1e9

// poisonedCloseRepository returns one usage page whose sequence is outside the
// frozen watermark, driving the real AdvanceClose page validation.
type poisonedCloseRepository struct {
	SettlementRepository
	sequence int64
}

type poisonedCloseTx struct {
	CloseTx
	sequence int64
}

func (r poisonedCloseRepository) WithinClose(ctx context.Context, account billing.AccountID, fn func(CloseTx) error) error {
	return r.SettlementRepository.WithinClose(ctx, account, func(tx CloseTx) error {
		return fn(poisonedCloseTx{CloseTx: tx, sequence: r.sequence})
	})
}

func (t poisonedCloseTx) UsagePage(period BillingPeriod, watermark, after int64, limit int) ([]UsageEntry, int64, bool, error) {
	rows, next, done, err := t.CloseTx.UsagePage(period, watermark, after, limit)
	if err != nil || len(rows) == 0 {
		return rows, next, done, err
	}
	// Keep the rest of the page self-consistent so the only thing that can
	// reject it is the watermark bound itself.
	rows[0].Sequence = t.sequence
	if len(rows) == 1 {
		next = t.sequence
	}
	return rows, next, done, nil
}

// A sequence the close never froze must be refused whichever side of the bound
// it falls on. The negative case previously depended on an earlier clause
// short-circuiting ahead of a uint64 conversion that would have wrapped it into
// a huge positive value and slipped past the watermark bound.
func TestAdvanceCloseRejectsSequencesOutsideTheFrozenWatermark(t *testing.T) {
	for _, sequence := range []int64{-1, 1 << 62} {
		clock := newSettlementClock()
		period := settlementPeriod(clock)
		record := postpaidRecord(t, "poisoned-usage", 1, period.Start.Add(time.Second), period.Cutoff, "USD")
		base := NewMemorySettlementRepository(ReferenceAccount{ID: settlementAccount, Usage: []Record{record}})
		service := NewSettlement(poisonedCloseRepository{SettlementRepository: base, sequence: sequence}, clock.Now)
		job, err := service.StartClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "poison-op", BatchID: "poison-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.AdvanceClose(t.Context(), settlementAccount, job.BatchID, job.Revision, 10); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("sequence %d advanced with err=%v, want conflict", sequence, err)
		}
	}
}
