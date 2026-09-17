package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func (t *settlementTx) IngestionWatermark() (int64, error) {
	var watermark int64
	err := t.tx.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(ingestion_sequence), 0) FROM billing_usage WHERE account_id=$1`, string(t.account)).Scan(&watermark)
	if err != nil {
		return 0, err
	}
	if watermark < 0 {
		return 0, billing.ErrState
	}
	return watermark, nil
}

// UsagePage must select exactly the rows the close will accept. A row the page
// returns but prepareUsageLines rejects aborts the whole page, and since the
// cursor never advances and billing_usage is insert-only, the period could
// never be closed again. The interval predicate therefore mirrors
// validPeriodContainment.
func (t *settlementTx) UsagePage(period usage.BillingPeriod, watermark int64, after int64, limit int) ([]usage.UsageEntry, int64, bool, error) {
	if !period.Valid() || limit < 1 || limit > 1000 || after < 0 || watermark < 0 {
		return nil, 0, false, billing.ErrInvalid
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT ingestion_sequence,fingerprint,record FROM billing_usage WHERE account_id=$1 AND funding='postpaid' AND ingestion_sequence<=$2 AND ingestion_sequence>$3 AND scope_provider=$4 AND scope_merchant=$5 AND scope_environment=$6 AND scope_subscription_id=$7 AND scope_item_id=$8 AND occurred_at>=$9 AND occurred_at<$10 AND occurred_at<$11 AND (record->>'ReceivedAt')::timestamptz <= $11 AND (interval_start IS NULL OR (interval_start>=$9 AND interval_end<=LEAST($10,$11))) ORDER BY ingestion_sequence LIMIT $12`, string(t.account), int64(watermark), after, period.Scope.Subscription.Scope.Provider, period.Scope.Subscription.Scope.Merchant, period.Scope.Subscription.Scope.Environment, period.Scope.Subscription.ID, period.Scope.ItemID, databaseTime(period.Start), databaseTime(period.End), databaseTime(period.Cutoff), limit+1)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	entries := make([]usage.UsageEntry, 0, limit+1)
	for rows.Next() {
		var entry usage.UsageEntry
		var fingerprint string
		var raw []byte
		if err = rows.Scan(&entry.Sequence, &fingerprint, &raw); err != nil {
			return nil, 0, false, err
		}
		if err = json.Unmarshal(raw, &entry.Record); err != nil {
			return nil, 0, false, err
		}
		if fingerprint == "" || entry.Record.Fingerprint != fingerprint || usage.Identity(entry.Record) != fingerprint {
			return nil, 0, false, billing.ErrConflict
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, false, err
	}
	done := len(entries) <= limit
	if !done {
		entries = entries[:limit]
	}
	next := after
	if len(entries) > 0 {
		next = entries[len(entries)-1].Sequence
	}
	return entries, next, done, nil
}

func (t *settlementTx) CloseByOperation(operation billing.OperationID) (usage.CloseJob, bool, error) {
	return readCloseJob(t.ctx, t.tx, t.account, `operation_id=$2`, string(operation))
}

func (t *settlementTx) CloseByKey(period usage.BillingPeriod) (usage.CloseJob, bool, error) {
	if !period.Valid() {
		return usage.CloseJob{}, false, billing.ErrInvalid
	}
	args := []any{databaseTime(period.Start), databaseTime(period.End), period.Scope.Subscription.Scope.Provider, period.Scope.Subscription.Scope.Merchant, period.Scope.Subscription.Scope.Environment, period.Scope.Subscription.ID, period.Scope.ItemID}
	job, found, err := readCloseJob(t.ctx, t.tx, t.account, `period_start=$2 AND period_end=$3 AND scope_provider=$4 AND scope_merchant=$5 AND scope_environment=$6 AND scope_subscription_id=$7 AND scope_item_id=$8 AND state <> 'canceled'`, args...)
	if err != nil || found {
		return job, found, err
	}
	// Existing first-release batches may precede resumable jobs. Their original
	// financial period is still reserved after an additive schema upgrade.
	params := append([]any{string(t.account)}, args...)
	err = t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM billing_settlement_batches WHERE account_id=$1 AND original_batch_id IS NULL AND period_start=$2 AND period_end=$3 AND scope_provider=$4 AND scope_merchant=$5 AND scope_environment=$6 AND scope_subscription_id=$7 AND scope_item_id=$8)`, params...).Scan(&found)
	return usage.CloseJob{}, found, err
}

func readCloseJob(ctx context.Context, q querier, account billing.AccountID, predicate string, args ...any) (usage.CloseJob, bool, error) {
	query := `SELECT job_id,operation_id,batch_id,period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,watermark,cursor,processed,staged_total,staged_checksum,state,revision,created_at,updated_at FROM billing_settlement_close_jobs WHERE account_id=$1 AND ` + predicate
	params := append([]any{string(account)}, args...)
	var j usage.CloseJob
	err := q.QueryRowContext(ctx, query, params...).Scan(&j.BatchID, &j.Operation, &j.BatchID, &j.Period.Start, &j.Period.End, &j.Period.Cutoff, &j.Period.Scope.Subscription.Scope.Provider, &j.Period.Scope.Subscription.Scope.Merchant, &j.Period.Scope.Subscription.Scope.Environment, &j.Period.Scope.Subscription.ID, &j.Period.Scope.ItemID, &j.Currency, &j.Watermark, &j.Cursor, &j.Processed, &j.StagedTotal, &j.StagedChecksum, &j.State, &j.Revision, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.CloseJob{}, false, nil
	}
	if err != nil {
		return usage.CloseJob{}, false, err
	}
	j.Account = account
	j.Period = j.Period.UTC()
	return j, true, nil
}

func (t *settlementTx) ReadClose(id string) (usage.CloseJob, bool, error) {
	return readCloseJob(t.ctx, t.tx, t.account, `job_id=$2`, id)
}

func (t *settlementTx) CreateClose(job usage.CloseJob) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_close_jobs(account_id,job_id,operation_id,batch_id,period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,watermark,cursor,processed,staged_total,staged_checksum,state,revision,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, string(t.account), job.BatchID, string(job.Operation), job.BatchID, databaseTime(job.Period.Start), databaseTime(job.Period.End), databaseTime(job.Period.Cutoff), job.Period.Scope.Subscription.Scope.Provider, job.Period.Scope.Subscription.Scope.Merchant, job.Period.Scope.Subscription.Scope.Environment, job.Period.Scope.Subscription.ID, job.Period.Scope.ItemID, job.Currency, job.Watermark, job.Cursor, job.Processed, job.StagedTotal, job.StagedChecksum, string(job.State), job.Revision, databaseTime(job.CreatedAt), databaseTime(job.UpdatedAt))
	return mapSettlementConstraint(err)
}

func (t *settlementTx) StageBatch(batch usage.Batch) error {
	if batch.Account != t.account || !billing.ValidID(batch.ID) || batch.State != usage.BatchPreparing || batch.Fingerprint == "" {
		return billing.ErrInvalid
	}
	result, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_batches(account_id,batch_id,original_batch_id,period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,total,state,revision,created_at,updated_at,fingerprint) VALUES($1,$2,NULL,$3,$4,$5,$6,$7,$8,$9,$10,$11,0,'preparing',0,$12,$13,$14) ON CONFLICT(account_id,batch_id) DO NOTHING`, string(t.account), batch.ID, databaseTime(batch.Period.Start), databaseTime(batch.Period.End), databaseTime(batch.Period.Cutoff), batch.Period.Scope.Subscription.Scope.Provider, batch.Period.Scope.Subscription.Scope.Merchant, batch.Period.Scope.Subscription.Scope.Environment, batch.Period.Scope.Subscription.ID, batch.Period.Scope.ItemID, batch.Currency, databaseTime(batch.CreatedAt), databaseTime(batch.UpdatedAt), batch.Fingerprint)
	if err != nil {
		return mapSettlementConstraint(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var valid bool
	err = t.tx.QueryRowContext(t.ctx, `SELECT state='preparing' AND currency=$3 AND period_start=$4 AND period_end=$5 AND cutoff=$6 AND scope_provider=$7 AND scope_merchant=$8 AND scope_environment=$9 AND scope_subscription_id=$10 AND scope_item_id=$11 FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2`, string(t.account), batch.ID, batch.Currency, databaseTime(batch.Period.Start), databaseTime(batch.Period.End), databaseTime(batch.Period.Cutoff), batch.Period.Scope.Subscription.Scope.Provider, batch.Period.Scope.Subscription.Scope.Merchant, batch.Period.Scope.Subscription.Scope.Environment, batch.Period.Scope.Subscription.ID, batch.Period.Scope.ItemID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return billing.ErrConflict
	}
	return nil
}

func (t *settlementTx) StageChunk(chunk usage.CloseChunk) error {
	batchID, lines, total, checksum, now := chunk.BatchID, chunk.Lines, chunk.Total, chunk.Checksum, chunk.Now
	if len(chunk.NonlinearKeys) > len(lines) {
		return billing.ErrInvalid
	}
	if len(chunk.NonlinearKeys) > 0 {
		result, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_close_nonlinear(account_id,job_id,aggregate_key) SELECT $1,$2,unnest($3::text[]) ON CONFLICT DO NOTHING`, string(t.account), batchID, chunk.NonlinearKeys)
		if err != nil {
			return mapSettlementConstraint(err)
		}
		if err = expectSettlementRows(result, int64(len(chunk.NonlinearKeys))); err != nil {
			return err
		}
	}
	if !billing.ValidID(batchID) || len(lines) > 1000 || total < 0 || checksum == "" || now.IsZero() {
		return billing.ErrInvalid
	}
	if len(lines) > 0 {
		ids := make([]string, len(lines))
		for i, line := range lines {
			ids[i] = line.UsageID
		}
		var claimed bool
		if err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM billing_settlement_usage_claims WHERE account_id=$1 AND usage_id=ANY($2))`, string(t.account), ids).Scan(&claimed); err != nil {
			return err
		}
		if claimed {
			return billing.ErrConflict
		}
		result, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_pending_claims(account_id,usage_id,job_id,source_fingerprint,reserved_at) SELECT account_id,usage_id,$3,fingerprint,$4 FROM billing_usage WHERE account_id=$1 AND usage_id=ANY($2) ON CONFLICT DO NOTHING`, string(t.account), ids, batchID, databaseTime(now))
		if err != nil {
			return mapSettlementConstraint(err)
		}
		if err = expectSettlementRows(result, int64(len(lines))); err != nil {
			return err
		}
		var query strings.Builder
		query.WriteString(`INSERT INTO billing_settlement_lines(account_id,batch_id,line_key,kind,usage_id,adjustment_id,original_usage_id,source,actor,project,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,occurred_at,interval_start,interval_end,amount,exact_amount,rule_version,currency,reason) VALUES `)
		args := make([]any, 0, len(lines)*23)
		for i, line := range lines {
			if i > 0 {
				query.WriteByte(',')
			}
			query.WriteByte('(')
			for j := range 23 {
				if j > 0 {
					query.WriteByte(',')
				}
				fmt.Fprintf(&query, "$%d", len(args)+j+1)
			}
			query.WriteByte(')')
			args = append(args, string(t.account), batchID, lineKeyForSettlement(line), line.Kind, line.UsageID, line.AdjustmentID, line.OriginalUsageID, line.Source, line.Actor, line.Project, line.Scope.Subscription.Scope.Provider, line.Scope.Subscription.Scope.Merchant, line.Scope.Subscription.Scope.Environment, line.Scope.Subscription.ID, line.Scope.ItemID, nullTime(line.OccurredAt), nullTime(line.Interval.Start), nullTime(line.Interval.End), line.Amount, line.ExactAmount, line.RuleVersion, line.Currency, line.Reason)
		}
		result, err = t.tx.ExecContext(t.ctx, query.String(), args...)
		if err != nil {
			return mapSettlementConstraint(err)
		}
		if err = expectSettlementRows(result, int64(len(lines))); err != nil {
			return err
		}
	}
	if err := t.verifyStagedSources(batchID, lines); err != nil {
		return err
	}

	result, err := t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_batches SET total=total+$3,line_count=line_count+$4,fingerprint=$5,updated_at=$6 WHERE account_id=$1 AND batch_id=$2 AND state='preparing'`, string(t.account), batchID, total, int64(len(lines)), checksum, databaseTime(now))
	if err != nil {
		return err
	}
	return expectSettlementRows(result, 1)
}

func expectSettlementRows(result sql.Result, expected int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != expected {
		return billing.ErrConflict
	}
	return nil
}

func (t *settlementTx) CompareAndSwapClose(id string, expected int64, next usage.CloseJob) error {
	result, err := t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_close_jobs SET cursor=$3,processed=$4,staged_total=$5,staged_checksum=$6,state=$7,revision=$8,updated_at=$9 WHERE account_id=$1 AND job_id=$2 AND revision=$10`, string(t.account), id, next.Cursor, next.Processed, next.StagedTotal, next.StagedChecksum, string(next.State), next.Revision, databaseTime(next.UpdatedAt), expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	if next.State == usage.CloseCanceled {
		var remaining bool
		if err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2) OR EXISTS(SELECT 1 FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2) OR EXISTS(SELECT 1 FROM billing_settlement_close_nonlinear WHERE account_id=$1 AND job_id=$2)`, string(t.account), id).Scan(&remaining); err != nil {
			return err
		}
		if remaining {
			return billing.ErrConflict
		}
		_, err := t.tx.ExecContext(t.ctx, `DELETE FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state='canceling'`, string(t.account), id)
		return err
	}
	return nil
}

func (t *settlementTx) PublishBatch(job usage.CloseJob) error {
	result, err := t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_batches SET state='ready',updated_at=$3 WHERE account_id=$1 AND batch_id=$2 AND state='preparing' AND total=$4 AND line_count=$5 AND fingerprint=$6 AND EXISTS(SELECT 1 FROM billing_settlement_close_jobs j WHERE j.account_id=$1 AND j.job_id=$2 AND j.state='ready' AND j.revision=$7)`, string(t.account), job.BatchID, databaseTime(job.UpdatedAt), job.StagedTotal, job.Processed, job.StagedChecksum, job.Revision)
	if err != nil {
		return err
	}
	return expectSettlementRows(result, 1)
}

func (t *settlementTx) BeginCancel(job usage.CloseJob) error {
	result, err := t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_close_jobs SET state='canceling',revision=revision+1,cursor=0,updated_at=$4 WHERE account_id=$1 AND job_id=$2 AND revision=$3 AND state IN ('preparing','ready')`, string(t.account), job.BatchID, job.Revision, databaseTime(job.UpdatedAt))
	if err != nil {
		return err
	}
	if err = expectSettlementRows(result, 1); err != nil {
		return err
	}
	// A close canceled before its first advance has no staged batch yet.
	_, err = t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_batches SET state='canceling',updated_at=$3 WHERE account_id=$1 AND batch_id=$2 AND state='preparing'`, string(t.account), job.BatchID, databaseTime(job.UpdatedAt))
	return err
}

func (t *settlementTx) CleanupClosePage(id string, after int64, limit int) (int64, bool, error) {
	if !billing.ValidID(id) || limit < 1 || limit > 1000 || after < 0 {
		return 0, false, billing.ErrInvalid
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT p.usage_id,u.ingestion_sequence FROM billing_settlement_pending_claims p JOIN billing_usage u USING(account_id,usage_id) WHERE p.account_id=$1 AND p.job_id=$2 ORDER BY p.usage_id COLLATE "C" LIMIT $3`, string(t.account), id, limit+1)
	if err != nil {
		return 0, false, err
	}
	ids := make([]string, 0, limit+1)
	next := after
	for rows.Next() {
		var id string
		var sequence int64
		if err = rows.Scan(&id, &sequence); err != nil {
			rows.Close()
			return 0, false, err
		}
		ids = append(ids, id)
		if sequence > next {
			next = sequence
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, err
	}
	done := len(ids) <= limit
	if !done {
		ids = ids[:limit]
	}
	if len(ids) > 0 {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = usage.UsageEstimateKey(id)
		}
		result, err := t.tx.ExecContext(t.ctx, `DELETE FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2 AND line_key=ANY($3)`, string(t.account), id, keys)
		if err != nil {
			return 0, false, err
		}
		if err = expectSettlementRows(result, int64(len(ids))); err != nil {
			return 0, false, err
		}
		result, err = t.tx.ExecContext(t.ctx, `DELETE FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2 AND usage_id=ANY($3)`, string(t.account), id, ids)
		if err != nil {
			return 0, false, err
		}
		if err = expectSettlementRows(result, int64(len(ids))); err != nil {
			return 0, false, err
		}
	}
	result, err := t.tx.ExecContext(t.ctx, `DELETE FROM billing_settlement_close_nonlinear WHERE account_id=$1 AND job_id=$2 AND aggregate_key IN (SELECT aggregate_key FROM billing_settlement_close_nonlinear WHERE account_id=$1 AND job_id=$2 ORDER BY aggregate_key LIMIT $3)`, string(t.account), id, limit)
	if err != nil {
		return 0, false, err
	}
	if _, err = result.RowsAffected(); err != nil {
		return 0, false, err
	}
	var keysRemain bool
	if err = t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM billing_settlement_close_nonlinear WHERE account_id=$1 AND job_id=$2)`, string(t.account), id).Scan(&keysRemain); err != nil {
		return 0, false, err
	}
	return next, done && !keysRemain, nil
}

func (t *settlementTx) BatchSummary(id string) (usage.BatchSummary, bool, error) {
	var out usage.BatchSummary
	err := t.tx.QueryRowContext(t.ctx, `SELECT COALESCE(original_batch_id,''),period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,total,state,revision,created_at,updated_at,line_count FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state IN ('ready','submitting','confirmed','rejected','unknown')`, string(t.account), id).Scan(&out.OriginalBatchID, &out.Period.Start, &out.Period.End, &out.Period.Cutoff, &out.Period.Scope.Subscription.Scope.Provider, &out.Period.Scope.Subscription.Scope.Merchant, &out.Period.Scope.Subscription.Scope.Environment, &out.Period.Scope.Subscription.ID, &out.Period.Scope.ItemID, &out.Currency, &out.Total, &out.State, &out.Revision, &out.CreatedAt, &out.UpdatedAt, &out.LineCount)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.BatchSummary{}, false, nil
	}
	if err != nil {
		return usage.BatchSummary{}, false, err
	}
	out.Account = t.account
	out.ID = id
	out.Period = out.Period.UTC()
	return out, true, nil
}

func (t *settlementTx) BatchLinesPage(id, after string, limit int) ([]usage.ChargeLine, string, bool, error) {
	if !billing.ValidID(id) || limit < 1 || limit > 1000 {
		return nil, "", false, billing.ErrInvalid
	}
	batch, found, err := t.BatchSummary(id)
	if err != nil {
		return nil, "", false, err
	}
	if !found {
		return nil, "", false, billing.ErrNotFound
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT line_key,kind,usage_id,adjustment_id,original_usage_id,source,actor,project,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,occurred_at,interval_start,interval_end,amount,exact_amount,rule_version,currency,reason FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2 AND line_key COLLATE "C">$3 ORDER BY line_key COLLATE "C" LIMIT $4`, string(t.account), id, after, limit+1)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	lines := make([]usage.ChargeLine, 0, limit+1)
	keys := make([]string, 0, limit+1)
	for rows.Next() {
		var key string
		var line usage.ChargeLine
		var occurred, start, end sql.NullTime
		if err = rows.Scan(&key, &line.Kind, &line.UsageID, &line.AdjustmentID, &line.OriginalUsageID, &line.Source, &line.Actor, &line.Project, &line.Scope.Subscription.Scope.Provider, &line.Scope.Subscription.Scope.Merchant, &line.Scope.Subscription.Scope.Environment, &line.Scope.Subscription.ID, &line.Scope.ItemID, &occurred, &start, &end, &line.Amount, &line.ExactAmount, &line.RuleVersion, &line.Currency, &line.Reason); err != nil {
			return nil, "", false, err
		}
		line.OccurredAt = scanTime(occurred)
		line.Interval = billing.Period{Start: scanTime(start), End: scanTime(end)}
		if key != lineKeyForSettlement(line) || line.Currency != batch.Currency || line.Scope != batch.Period.Scope {
			return nil, "", false, billing.ErrConflict
		}
		lines = append(lines, line)
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, "", false, err
	}
	more := len(lines) > limit
	next := ""
	if more {
		lines = lines[:limit]
		next = keys[limit-1]
	}
	return lines, next, more, nil
}

// Recheck a bounded chunk after its writes. This preserves the stored-source
// consistency guard without one source query per line.
func (t *settlementTx) verifyStagedSources(batchID string, lines []usage.ChargeLine) error {
	return t.verifyUsageSources(batchID, lines, true)
}
