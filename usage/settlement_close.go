package usage

import (
	"context"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type CloseState string

const (
	ClosePreparing CloseState = "preparing"
	CloseReady     CloseState = "ready"
	ClosePublished CloseState = "published"
	CloseCanceling CloseState = "canceling"
	CloseCanceled  CloseState = "canceled"
)

type CloseJob struct {
	Account        billing.AccountID
	Operation      billing.OperationID
	BatchID        string
	Period         BillingPeriod
	Currency       string
	Watermark      int64
	Cursor         int64
	Processed      int64
	StagedTotal    int64
	StagedChecksum string
	State          CloseState
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type CloseInput struct {
	Account   billing.AccountID
	Operation billing.OperationID
	BatchID   string
	Period    BillingPeriod
	Currency  string
	CreatedAt time.Time
}

type CloseRepository interface {
	WithinClose(context.Context, billing.AccountID, func(CloseTx) error) error
}

type CloseTx interface {
	IngestionWatermark() (int64, error)
	UsagePage(BillingPeriod, int64, int64, int) ([]UsageEntry, int64, bool, error)
	CloseByOperation(billing.OperationID) (CloseJob, bool, error)
	CloseByKey(BillingPeriod) (CloseJob, bool, error)
	CreateClose(CloseJob) error
	ReadClose(string) (CloseJob, bool, error)
	StageBatch(Batch) error
	StageChunk(CloseChunk) error
	CompareAndSwapClose(string, int64, CloseJob) error
	PublishBatch(CloseJob) error
	BeginCancel(CloseJob) error
	CleanupClosePage(string, int64, int) (int64, bool, error)
	BatchSummary(string) (BatchSummary, bool, error)
	BatchLinesPage(string, string, int) ([]ChargeLine, string, bool, error)
}

type CloseChunk struct {
	BatchID       string
	Lines         []ChargeLine
	Total         int64
	Checksum      string
	NonlinearKeys []string
	Now           time.Time
}

type UsageEntry struct {
	Sequence int64
	Record   Record
}

type BatchSummary struct {
	Account         billing.AccountID
	ID              string
	Period          BillingPeriod
	Currency        string
	Total           int64
	State           BatchState
	Revision        int64
	LineCount       int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	OriginalBatchID string
}

func (s *SettlementService) closeRepository() (CloseRepository, error) {
	return s.repo, nil
}

func (s *SettlementService) StartClose(ctx context.Context, in CloseInput) (CloseJob, error) {
	if err := ctx.Err(); err != nil {
		return CloseJob{}, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return CloseJob{}, err
	}
	if !validOperation(in.Account, in.Operation) {
		return CloseJob{}, billing.ErrInvalid
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = billing.CanonicalTime(s.now())
	} else {
		in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	}
	period, err := validateBatchInput(in.Account, in.BatchID, in.Period.UTC(), in.Currency, in.CreatedAt)
	if err != nil {
		return CloseJob{}, err
	}
	var out CloseJob
	err = r.WithinClose(ctx, in.Account, func(tx CloseTx) error {
		if old, found, e := tx.CloseByOperation(in.Operation); e != nil {
			return e
		} else if found {
			if closeFingerprint(old) != closeFingerprintInput(in, period) {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if _, found, e := tx.CloseByKey(period); e != nil {
			return e
		} else if found {
			return billing.ErrConflict
		}
		watermark, e := tx.IngestionWatermark()
		if e != nil {
			return e
		}
		out = CloseJob{Account: in.Account, Operation: in.Operation, BatchID: in.BatchID, Period: period, Currency: in.Currency, Watermark: watermark, State: ClosePreparing, CreatedAt: in.CreatedAt, UpdatedAt: in.CreatedAt}
		out.StagedChecksum = identity.Fingerprint("close-lines", string(in.Account), in.BatchID)
		if e = tx.CreateClose(out); e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		return CloseJob{}, err
	}
	return out, nil
}

func (s *SettlementService) AdvanceClose(ctx context.Context, account billing.AccountID, jobID string, expectedRevision int64, limit int) (CloseJob, error) {
	if err := validateCloseRequest(ctx, account, jobID, expectedRevision, limit); err != nil {
		return CloseJob{}, err
	}
	r, err := s.closeRepository()
	if err != nil || limit < 1 || limit > 1000 {
		if err != nil {
			return CloseJob{}, err
		}
		return CloseJob{}, billing.ErrInvalid
	}
	var out CloseJob
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		job, found, e := tx.ReadClose(jobID)
		if e != nil {
			return e
		}
		if !found {
			return billing.ErrNotFound
		}
		if job.Account != account || job.BatchID != jobID {
			return billing.ErrState
		}
		if job.Revision != expectedRevision {
			return billing.ErrConflict
		}
		if job.State == CloseCanceling {
			next, done, cleanupErr := tx.CleanupClosePage(job.BatchID, job.Cursor, limit)
			if cleanupErr != nil {
				return cleanupErr
			}
			job.Cursor = next
			if done {
				job.State = CloseCanceled
			}
			job.Revision++
			job.UpdatedAt = billing.CanonicalTime(s.now())
			if cleanupErr = tx.CompareAndSwapClose(job.BatchID, expectedRevision, job); cleanupErr != nil {
				return cleanupErr
			}
			out = job
			return nil
		}
		if job.State != ClosePreparing {
			return billing.ErrState
		}
		rows, next, done, e := tx.UsagePage(job.Period, job.Watermark, job.Cursor, limit)
		if e != nil {
			return e
		}
		if len(rows) > limit {
			return billing.ErrState
		}
		records := make([]Record, len(rows))
		for i := range rows {
			if rows[i].Sequence <= 0 || rows[i].Sequence <= job.Cursor || rows[i].Sequence > job.Watermark || (i > 0 && rows[i].Sequence <= rows[i-1].Sequence) {
				return billing.ErrConflict
			}
			records[i] = rows[i].Record
		}
		if len(rows) == 0 {
			if next != job.Cursor {
				return billing.ErrConflict
			}
		} else if next != rows[len(rows)-1].Sequence {
			return billing.ErrConflict
		}
		lines, total, e := prepareUsageLines(account, job.Currency, job.Period, records, false)
		if e != nil {
			return e
		}
		batch := Batch{Account: account, ID: job.BatchID, Period: job.Period, Currency: job.Currency, State: BatchPreparing, CreatedAt: job.CreatedAt, UpdatedAt: billing.CanonicalTime(s.now()), Fingerprint: job.StagedChecksum}
		if e = tx.StageBatch(batch); e != nil {
			return e
		}
		nonlinearKeys := make([]string, 0, len(records))
		for _, record := range records {
			if key := PostpaidAggregateIdentity(record, job.Period.Start, job.Period.End); key != "" {
				nonlinearKeys = append(nonlinearKeys, key)
			}
		}
		chunkChecksum := identity.Fingerprint("close-lines", job.StagedChecksum, linesFingerprint(lines))
		if e = tx.StageChunk(CloseChunk{BatchID: job.BatchID, Lines: lines, Total: total, Checksum: chunkChecksum, NonlinearKeys: nonlinearKeys, Now: billing.CanonicalTime(s.now())}); e != nil {
			return e
		}
		stagedTotal, e := checked.Add(job.StagedTotal, total)
		if e != nil {
			return e
		}
		if len(rows) > 0 && next <= job.Cursor {
			return billing.ErrConflict
		}
		processed, e := checked.Add(job.Processed, int64(len(lines)))
		if e != nil {
			return e
		}
		job.Cursor, job.Processed, job.StagedTotal = next, processed, stagedTotal
		job.StagedChecksum = chunkChecksum
		if done {
			job.State = CloseReady
		}
		job.Revision++
		job.UpdatedAt = billing.CanonicalTime(s.now())
		if e = tx.CompareAndSwapClose(job.BatchID, expectedRevision, job); e != nil {
			return e
		}
		out = job
		return nil
	})
	if err != nil {
		return CloseJob{}, err
	}
	return out, nil
}

func (s *SettlementService) PublishClose(ctx context.Context, account billing.AccountID, jobID string, expectedRevision int64) (CloseJob, error) {
	if err := validateCloseRequest(ctx, account, jobID, expectedRevision, 0); err != nil {
		return CloseJob{}, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return CloseJob{}, err
	}
	var out CloseJob
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		job, found, e := tx.ReadClose(jobID)
		if e != nil {
			return e
		}
		if !found {
			return billing.ErrNotFound
		}
		if job.Account != account || job.BatchID != jobID {
			return billing.ErrState
		}
		if job.Revision != expectedRevision || job.State != CloseReady {
			return billing.ErrConflict
		}
		job.UpdatedAt = billing.CanonicalTime(s.now())
		if e = tx.PublishBatch(job); e != nil {
			return e
		}
		job.State, job.Revision = ClosePublished, job.Revision+1
		if e = tx.CompareAndSwapClose(job.BatchID, expectedRevision, job); e != nil {
			return e
		}
		out = job
		return nil
	})
	if err != nil {
		return CloseJob{}, err
	}
	return out, nil
}

func (s *SettlementService) CancelClose(ctx context.Context, account billing.AccountID, jobID string, expectedRevision int64) (CloseJob, error) {
	if err := validateCloseRequest(ctx, account, jobID, expectedRevision, 0); err != nil {
		return CloseJob{}, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return CloseJob{}, err
	}
	var out CloseJob
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		job, found, e := tx.ReadClose(jobID)
		if e != nil {
			return e
		}
		if !found {
			return billing.ErrNotFound
		}
		if job.Account != account || job.BatchID != jobID {
			return billing.ErrState
		}
		if job.Revision != expectedRevision {
			return billing.ErrConflict
		}
		if job.State == ClosePublished || job.State == CloseCanceled {
			return billing.ErrState
		}
		job.Cursor = 0
		job.UpdatedAt = billing.CanonicalTime(s.now())
		if e = tx.BeginCancel(job); e != nil {
			return e
		}
		job.State, job.Revision = CloseCanceling, job.Revision+1
		out = job
		return nil
	})
	if err != nil {
		return CloseJob{}, err
	}
	return out, nil
}

func (s *SettlementService) CloseJob(ctx context.Context, account billing.AccountID, id string) (CloseJob, error) {
	if err := validateCloseRequest(ctx, account, id, 0, 0); err != nil {
		return CloseJob{}, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return CloseJob{}, err
	}
	var out CloseJob
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		var ok bool
		out, ok, err = tx.ReadClose(id)
		if ok && (out.Account != account || out.BatchID != id) {
			return billing.ErrState
		}
		if !ok && err == nil {
			err = billing.ErrNotFound
		}
		return err
	})
	if err != nil {
		return CloseJob{}, err
	}
	return out, err
}

func (s *SettlementService) BatchSummary(ctx context.Context, account billing.AccountID, id string) (BatchSummary, error) {
	if err := validateCloseRequest(ctx, account, id, 0, 0); err != nil {
		return BatchSummary{}, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return BatchSummary{}, err
	}
	var out BatchSummary
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		var ok bool
		out, ok, err = tx.BatchSummary(id)
		if !ok && err == nil {
			err = billing.ErrNotFound
		}
		return err
	})
	if err != nil {
		return BatchSummary{}, err
	}
	return out, err
}

func (s *SettlementService) BatchLinesPage(ctx context.Context, account billing.AccountID, id, after string, limit int) ([]ChargeLine, string, bool, error) {
	if err := validateCloseRequest(ctx, account, id, 0, limit); err != nil {
		return nil, "", false, err
	}
	r, err := s.closeRepository()
	if err != nil {
		return nil, "", false, err
	}
	var lines []ChargeLine
	var next string
	var hasMore bool
	err = r.WithinClose(ctx, account, func(tx CloseTx) error {
		lines, next, hasMore, err = tx.BatchLinesPage(id, after, limit)
		return err
	})
	if err != nil {
		return nil, "", false, err
	}
	return lines, next, hasMore, err
}

func validateCloseRequest(ctx context.Context, account billing.AccountID, id string, revision int64, limit int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(id) || revision < 0 || limit < 0 || limit > 1000 {
		return billing.ErrInvalid
	}
	return nil
}

func lineFingerprint(line ChargeLine) string {
	return identity.Fingerprint("close-line", string(line.Kind), line.UsageID, line.AdjustmentID, line.OriginalUsageID, line.Source, line.Actor, line.Project, scopeFingerprint(line.Scope), identity.Instant(line.OccurredAt), identity.Instant(line.Interval.Start), identity.Instant(line.Interval.End), fmt.Sprint(line.Amount), line.ExactAmount, line.RuleVersion, line.Currency, line.Reason)
}
func linesFingerprint(lines []ChargeLine) string {
	fields := make([]string, 0, len(lines))
	for _, line := range lines {
		fields = append(fields, lineFingerprint(line))
	}
	return identity.Fingerprint(fields...)
}
func closeFingerprint(job CloseJob) string {
	return closeFingerprintInput(CloseInput{Account: job.Account, Operation: job.Operation, BatchID: job.BatchID, Period: job.Period, Currency: job.Currency, CreatedAt: job.CreatedAt}, job.Period)
}
func closeFingerprintInput(in CloseInput, period BillingPeriod) string {
	return identity.Fingerprint("close", string(in.Account), string(in.Operation), in.BatchID, identity.Instant(period.Start), identity.Instant(period.End), identity.Instant(period.Cutoff), scopeFingerprint(period.Scope), in.Currency)
}
