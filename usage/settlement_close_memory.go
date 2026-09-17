package usage

import (
	"context"
	"slices"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
)

func (r *settlementMemoryRepository) WithinClose(ctx context.Context, account billing.AccountID, fn func(CloseTx) error) error {
	ac, err := r.account(account)
	if err != nil {
		return err
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	state := cloneSettlementState(ac.state)
	if err = fn(&settlementMemoryTx{state: &state}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	ac.state = state
	return nil
}

func (tx *settlementMemoryTx) IngestionWatermark() (int64, error) { return tx.state.watermark, nil }

func (tx *settlementMemoryTx) UsagePage(period BillingPeriod, watermark int64, after int64, limit int) ([]UsageEntry, int64, bool, error) {
	if !period.Valid() || limit < 1 || limit > 1000 || watermark > tx.state.watermark {
		return nil, 0, false, billing.ErrInvalid
	}
	rows := make([]UsageEntry, 0, limit+1)
	for _, record := range tx.state.usage {
		o := record.Observation
		if tx.state.sequences[o.ID] > watermark || int64(tx.state.sequences[o.ID]) <= after || o.Funding != Postpaid || o.Scope != period.Scope || o.OccurredAt.Before(period.Start) || !o.OccurredAt.Before(period.End) || !o.OccurredAt.Before(period.Cutoff) || record.ReceivedAt.After(period.Cutoff) {
			continue
		}
		rows = append(rows, UsageEntry{Sequence: int64(tx.state.sequences[o.ID]), Record: Copy(record)})
	}
	slices.SortFunc(rows, func(a, b UsageEntry) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	done := len(rows) <= limit
	if !done {
		rows = rows[:limit]
	}
	next := after
	if len(rows) > 0 {
		next = rows[len(rows)-1].Sequence
	}
	return rows, next, done, nil
}

func (tx *settlementMemoryTx) CloseByOperation(operation billing.OperationID) (CloseJob, bool, error) {
	for _, job := range tx.state.closes {
		if job.Operation == operation {
			return job, true, nil
		}
	}
	return CloseJob{}, false, nil
}

func (tx *settlementMemoryTx) CloseByKey(period BillingPeriod) (CloseJob, bool, error) {
	for _, job := range tx.state.closes {
		if job.Period.Scope == period.Scope && job.Period.Start.Equal(period.Start) && job.Period.End.Equal(period.End) && job.State != CloseCanceled {
			return job, true, nil
		}
	}
	return CloseJob{}, false, nil
}

func (tx *settlementMemoryTx) CreateClose(job CloseJob) error {
	if _, ok := tx.state.closes[job.BatchID]; ok {
		return billing.ErrConflict
	}
	tx.state.closes[job.BatchID] = job
	return nil
}

func (tx *settlementMemoryTx) ReadClose(id string) (CloseJob, bool, error) {
	job, ok := tx.state.closes[id]
	return job, ok, nil
}

func (tx *settlementMemoryTx) StageBatch(batch Batch) error {
	if old, ok := tx.state.batches[batch.ID]; ok {
		if old.Account != batch.Account || !old.Period.Equal(batch.Period) || old.Currency != batch.Currency {
			return billing.ErrConflict
		}
		return nil
	}
	batch.Lines = nil
	tx.state.batches[batch.ID] = cloneBatch(batch)
	return nil
}

func (tx *settlementMemoryTx) StageChunk(chunk CloseChunk) error {
	batchID, lines, total, checksum, now := chunk.BatchID, chunk.Lines, chunk.Total, chunk.Checksum, chunk.Now
	if checksum == "" || total < 0 {
		return billing.ErrInvalid
	}
	batch, ok := tx.state.batches[batchID]
	if !ok {
		return billing.ErrNotFound
	}
	keys := tx.state.nonlinear[batchID]
	if keys == nil {
		keys = map[string]struct{}{}
		tx.state.nonlinear[batchID] = keys
	}
	for _, key := range chunk.NonlinearKeys {
		if _, exists := keys[key]; exists {
			return billing.ErrConflict
		}
	}
	for _, key := range chunk.NonlinearKeys {
		keys[key] = struct{}{}
	}
	for _, line := range lines {
		key := lineKey(line)
		if old, ok := tx.state.claims[line.UsageID]; ok && old != batchID {
			return billing.ErrConflict
		}
		if old, ok := tx.state.pending[line.UsageID]; ok && old != batchID {
			return billing.ErrConflict
		}
		tx.state.pending[line.UsageID] = batchID
		found := false
		for _, old := range batch.Lines {
			if lineKey(old) == key {
				if old != line {
					return billing.ErrConflict
				}
				found = true
				break
			}
		}
		if !found {
			batch.Lines = append(batch.Lines, line)
		}
	}
	batch.Total += total
	batch.UpdatedAt = now
	tx.state.batches[batchID] = cloneBatch(batch)
	return nil
}

func (tx *settlementMemoryTx) CompareAndSwapClose(id string, expected int64, next CloseJob) error {
	old, ok := tx.state.closes[id]
	if !ok {
		return billing.ErrNotFound
	}
	if old.Revision != expected {
		return billing.ErrConflict
	}
	tx.state.closes[id] = next
	return nil
}

func (tx *settlementMemoryTx) PublishBatch(job CloseJob) error {
	id := job.BatchID
	batch, ok := tx.state.batches[id]
	if !ok {
		return billing.ErrNotFound
	}
	if batch.State != BatchPreparing {
		return billing.ErrConflict
	}
	batch.State = BatchReady
	batch.Fingerprint = batchFingerprint(batch)
	tx.state.batches[id] = cloneBatch(batch)
	return nil
}

func (tx *settlementMemoryTx) BeginCancel(job CloseJob) error {
	id, expected := job.BatchID, job.Revision
	stored, ok := tx.state.closes[id]
	if !ok {
		return billing.ErrNotFound
	}
	if stored.Revision != expected {
		return billing.ErrConflict
	}
	job.State = CloseCanceling
	job.Revision = stored.Revision + 1
	tx.state.closes[id] = job
	if batch, found := tx.state.batches[id]; found {
		batch.State = BatchCanceling
		tx.state.batches[id] = batch
	}
	return nil
}

func (tx *settlementMemoryTx) CleanupClosePage(id string, after int64, limit int) (int64, bool, error) {
	if limit < 1 || limit > 1000 {
		return 0, false, billing.ErrInvalid
	}
	keys := make([]string, 0, limit+1)
	for usageID, jobID := range tx.state.pending {
		if jobID == id && int64(tx.state.sequences[usageID]) > after {
			keys = append(keys, usageID)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		as, bs := tx.state.sequences[a], tx.state.sequences[b]
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
		return strings.Compare(a, b)
	})
	done := len(keys) <= limit
	if !done {
		keys = keys[:limit]
	}
	for _, key := range keys {
		delete(tx.state.pending, key)
		if batch, ok := tx.state.batches[id]; ok {
			kept := batch.Lines[:0]
			for _, line := range batch.Lines {
				if line.UsageID != key {
					kept = append(kept, line)
				}
			}
			batch.Lines = kept
			tx.state.batches[id] = cloneBatch(batch)
		}
	}
	next := after
	if len(keys) > 0 {
		next = int64(tx.state.sequences[keys[len(keys)-1]])
	}
	return next, done, nil
}

func (tx *settlementMemoryTx) BatchSummary(id string) (BatchSummary, bool, error) {
	b, ok := tx.state.batches[id]
	if !ok || b.State == BatchPreparing || b.State == BatchCanceling {
		return BatchSummary{}, false, nil
	}
	return BatchSummary{Account: b.Account, ID: b.ID, OriginalBatchID: b.OriginalBatchID, Period: b.Period, Currency: b.Currency, Total: b.Total, State: b.State, Revision: b.Revision, LineCount: int64(len(b.Lines)), CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}, true, nil
}

func (tx *settlementMemoryTx) BatchLinesPage(id, after string, limit int) ([]ChargeLine, string, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, "", false, billing.ErrInvalid
	}
	b, ok := tx.state.batches[id]
	if !ok || b.State == BatchPreparing || b.State == BatchCanceling {
		return nil, "", false, billing.ErrNotFound
	}
	lines := slices.Clone(b.Lines)
	slices.SortFunc(lines, func(a, b ChargeLine) int { return strings.Compare(lineKey(a), lineKey(b)) })
	start := 0
	for start < len(lines) && strings.Compare(lineKey(lines[start]), after) <= 0 {
		start++
	}
	lines = lines[start:]
	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}
	next := after
	if len(lines) > 0 {
		next = lineKey(lines[len(lines)-1])
	}
	return slices.Clone(lines), next, hasMore, nil
}

var _ CloseRepository = (*settlementMemoryRepository)(nil)
var _ CloseTx = (*settlementMemoryTx)(nil)
