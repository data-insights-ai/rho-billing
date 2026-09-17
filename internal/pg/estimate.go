package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

// EstimateUsage returns a read-only usage snapshot. Pending estimates use the
// stored rating evidence and never load the current rating rule. Once an
// original settlement batch exists, the result is the frozen batch evidence.
// Pending totals and validation scan the indexed eligible range; arbitrary
// period estimates therefore have bounded memory and page output, but their
// database work remains proportional to the selected range.
func (s *Store) EstimateUsage(ctx context.Context, in usage.EstimateInput) (usage.Estimate, error) {
	if s == nil || s.db == nil || !in.Valid() {
		return usage.Estimate{}, billing.ErrInvalid
	}
	in.Period = in.Period.UTC()
	asOf := s.now()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return usage.Estimate{}, err
	}
	defer tx.Rollback()
	if err = ensureEstimateAccount(ctx, tx, in.Account); err != nil {
		return usage.Estimate{}, err
	}
	batchID, found, err := findOriginalEstimateBatch(ctx, tx, in)
	if err != nil {
		return usage.Estimate{}, err
	}
	if found {
		batch, readErr := readEstimateBatchHeader(ctx, tx, in.Account, batchID)
		if readErr != nil {
			return usage.Estimate{}, readErr
		}
		if batch.Currency != in.Currency || !batch.Period.Start.Equal(in.Period.Start) || !batch.Period.End.Equal(in.Period.End) || batch.Period.Scope != in.Period.Scope {
			return usage.Estimate{}, billing.ErrConflict
		}
		out := usage.Estimate{
			Status:   usage.StatusFinalized,
			Account:  batch.Account,
			Period:   estimatePeriodFromBillingPeriod(batch.Period),
			Currency: batch.Currency,
			BatchID:  batch.ID,
			AsOf:     asOf,
			Total:    batch.Total,
		}
		out.Lines, out.NextAfter, err = readEstimateLinePage(ctx, tx, in.Account, batch.ID, batch.Currency, batch.Period.Scope, in.After, in.Limit)
		if err != nil {
			return usage.Estimate{}, err
		}
		if err = tx.Commit(); err != nil {
			return usage.Estimate{}, err
		}
		return out, nil
	}

	total, err := estimateUsageTotal(ctx, tx, in)
	if err != nil {
		return usage.Estimate{}, err
	}
	lines, nextAfter, err := readAndValidateEstimatePage(ctx, tx, in)
	if err != nil {
		return usage.Estimate{}, err
	}
	if err := rejectDuplicateEstimateAggregates(ctx, tx, in); err != nil {
		return usage.Estimate{}, err
	}
	out := usage.Estimate{Status: usage.StatusEstimated, Account: in.Account, Period: in.Period, Currency: in.Currency, AsOf: asOf, Total: total}
	out.Lines, out.NextAfter = lines, nextAfter
	if err = tx.Commit(); err != nil {
		return usage.Estimate{}, err
	}
	return out, nil
}

func ensureEstimateAccount(ctx context.Context, q querier, account billing.AccountID) error {
	var found string
	err := q.QueryRowContext(ctx, `SELECT account_id FROM billing_accounts WHERE account_id=$1`, string(account)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.ErrNotFound
	}
	return err
}

func findOriginalEstimateBatch(ctx context.Context, q querier, in usage.EstimateInput) (string, bool, error) {
	const published = `('ready','submitting','confirmed','rejected','unknown')`
	if in.BatchID != "" {
		var id string
		err := q.QueryRowContext(ctx, `SELECT batch_id FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state IN `+published, string(in.Account), in.BatchID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, billing.ErrNotFound
		}
		if err != nil {
			return "", false, err
		}
		return id, true, nil
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT batch_id FROM billing_settlement_batches WHERE account_id=$1 AND original_batch_id IS NULL AND state IN `+published+` AND period_start=$2 AND period_end=$3 AND scope_provider=$4 AND scope_merchant=$5 AND scope_environment=$6 AND scope_subscription_id=$7 AND scope_item_id=$8`, string(in.Account), databaseTime(in.Period.Start), databaseTime(in.Period.End), in.Period.Scope.Subscription.Scope.Provider, in.Period.Scope.Subscription.Scope.Merchant, in.Period.Scope.Subscription.Scope.Environment, in.Period.Scope.Subscription.ID, in.Period.Scope.ItemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func readEstimateBatchHeader(ctx context.Context, q querier, account billing.AccountID, id string) (usage.Batch, error) {
	var b usage.Batch
	var original sql.NullString
	err := q.QueryRowContext(ctx, `SELECT original_batch_id,period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,total,state,revision,created_at,updated_at,fingerprint FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state IN ('ready','submitting','confirmed','rejected','unknown')`, string(account), id).Scan(&original, &b.Period.Start, &b.Period.End, &b.Period.Cutoff, &b.Period.Scope.Subscription.Scope.Provider, &b.Period.Scope.Subscription.Scope.Merchant, &b.Period.Scope.Subscription.Scope.Environment, &b.Period.Scope.Subscription.ID, &b.Period.Scope.ItemID, &b.Currency, &b.Total, &b.State, &b.Revision, &b.CreatedAt, &b.UpdatedAt, &b.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Batch{}, billing.ErrNotFound
	}
	if err != nil {
		return usage.Batch{}, err
	}
	b.Account, b.ID = account, id
	if original.Valid {
		b.OriginalBatchID = original.String
	}
	return b, nil
}

func readEstimateLinePage(ctx context.Context, q querier, account billing.AccountID, batchID, currency string, scope usage.BillingScope, after string, limit int) ([]usage.EstimateLine, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT line_key,kind,usage_id,adjustment_id,original_usage_id,source,actor,project,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,occurred_at,interval_start,interval_end,amount,exact_amount,rule_version,currency,reason FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2 AND line_key COLLATE "C" > $3 ORDER BY line_key COLLATE "C" LIMIT $4`, string(account), batchID, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]usage.EstimateLine, 0, limit)
	var extra bool
	last := ""
	for rows.Next() {
		var key string
		var line usage.ChargeLine
		var occurred, intervalStart, intervalEnd sql.NullTime
		if err := rows.Scan(&key, &line.Kind, &line.UsageID, &line.AdjustmentID, &line.OriginalUsageID, &line.Source, &line.Actor, &line.Project, &line.Scope.Subscription.Scope.Provider, &line.Scope.Subscription.Scope.Merchant, &line.Scope.Subscription.Scope.Environment, &line.Scope.Subscription.ID, &line.Scope.ItemID, &occurred, &intervalStart, &intervalEnd, &line.Amount, &line.ExactAmount, &line.RuleVersion, &line.Currency, &line.Reason); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			extra = true
			continue
		}
		line.OccurredAt = scanTime(occurred)
		if intervalStart.Valid || intervalEnd.Valid {
			line.Interval.Start, line.Interval.End = scanTime(intervalStart), scanTime(intervalEnd)
		}
		if line.Currency != currency || line.Scope != scope {
			return nil, "", billing.ErrState
		}
		estimateLine := estimateLineFromChargeLine(line)
		if estimateLine.Key != key {
			return nil, "", billing.ErrState
		}
		out = append(out, estimateLine)
		last = key
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if !extra {
		return out, "", nil
	}
	return out, last, nil
}

func estimatePeriodFromBillingPeriod(period usage.BillingPeriod) usage.EstimatePeriod {
	return usage.EstimatePeriod{Start: period.Start, End: period.End, Cutoff: period.Cutoff, Scope: period.Scope}
}

func estimateLineFromRecord(record usage.Record) usage.EstimateLine {
	o := record.Observation
	return usage.EstimateLine{
		Key:         usage.UsageEstimateKey(o.ID),
		Kind:        "usage",
		UsageID:     o.ID,
		Source:      o.Source,
		Actor:       o.Actor,
		Project:     o.Project,
		Scope:       o.Scope,
		OccurredAt:  billing.CanonicalTime(o.OccurredAt),
		Interval:    billing.Period{Start: billing.CanonicalTime(o.Interval.Start), End: billing.CanonicalTime(o.Interval.End)},
		Amount:      record.Rating.Money.MinorUnits,
		ExactAmount: record.Rating.ExactAmount,
		RuleVersion: record.Rating.RuleVersion,
		Currency:    record.Rating.Money.Currency,
		Fingerprint: record.Fingerprint,
	}
}

func estimateLineFromChargeLine(line usage.ChargeLine) usage.EstimateLine {
	key := usage.UsageEstimateKey(line.UsageID)
	if line.Kind == "adjustment" {
		key = usage.AdjustmentEstimateKey(line.AdjustmentID)
	}
	return usage.EstimateLine{
		Key:             key,
		Kind:            line.Kind,
		UsageID:         line.UsageID,
		AdjustmentID:    line.AdjustmentID,
		OriginalUsageID: line.OriginalUsageID,
		Source:          line.Source,
		Actor:           line.Actor,
		Project:         line.Project,
		Scope:           line.Scope,
		OccurredAt:      line.OccurredAt,
		Interval:        line.Interval,
		Amount:          line.Amount,
		ExactAmount:     line.ExactAmount,
		RuleVersion:     line.RuleVersion,
		Currency:        line.Currency,
		Reason:          line.Reason,
	}
}

func validateEstimateRecord(record usage.Record, in usage.EstimateInput) error {
	o := record.Observation
	if o.Account != in.Account || !billing.ValidID(o.ID) || !billing.ValidID(o.Source) || o.Funding != usage.Postpaid || o.Input.Waived || record.Fingerprint == "" || record.Fingerprint != usage.Identity(record) || record.ReceivedAt.IsZero() || record.ReceivedAt.After(in.Period.Cutoff) || o.OccurredAt.Before(in.Period.Start) || !o.OccurredAt.Before(in.Period.End) || !in.Period.Cutoff.After(o.OccurredAt) || o.Scope != in.Period.Scope || !estimateIntervalContainment(o.Interval, in.Period) {
		return billing.ErrInvalid
	}
	if record.Rating.Money == nil || record.Rating.Credits != nil || record.Rating.Money.Currency != in.Currency || record.Rating.Target.Currency != in.Currency || record.Rating.Money.MinorUnits != record.Rating.RoundedAmount || record.Rating.RoundedAmount < 0 || record.Rating.ExactAmount == "" || record.Rating.RuleVersion == "" || record.Rating.Evidence.Waived {
		return billing.ErrInvalid
	}
	if err := usage.ValidatePostpaidAggregate(record, in.Period.Start, in.Period.End); err != nil {
		return err
	}
	return nil
}

func estimateIntervalContainment(interval billing.Period, period usage.EstimatePeriod) bool {
	if interval.Start.IsZero() && interval.End.IsZero() {
		return true
	}
	boundary := period.End
	if period.Cutoff.Before(boundary) {
		boundary = period.Cutoff
	}
	return interval.Valid() && !interval.Start.Before(period.Start) && !interval.End.After(boundary)
}

func estimateUsageTotal(ctx context.Context, q querier, in usage.EstimateInput) (int64, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM((record->'Rating'->>'RoundedAmount')::numeric),0)::text FROM billing_usage WHERE account_id=$1 AND funding='postpaid' AND scope_provider=$2 AND scope_merchant=$3 AND scope_environment=$4 AND scope_subscription_id=$5 AND scope_item_id=$6 AND occurred_at >= $7 AND occurred_at < $8 AND occurred_at < $9 AND ((record->>'ReceivedAt') IS NULL OR (record->>'ReceivedAt')::timestamptz <= $9)`, string(in.Account), in.Period.Scope.Subscription.Scope.Provider, in.Period.Scope.Subscription.Scope.Merchant, in.Period.Scope.Subscription.Scope.Environment, in.Period.Scope.Subscription.ID, in.Period.Scope.ItemID, databaseTime(in.Period.Start), databaseTime(in.Period.End), databaseTime(in.Period.Cutoff)).Scan(&raw)
	if err != nil {
		return 0, err
	}
	total, ok := new(big.Int).SetString(raw, 10)
	if !ok || !total.IsInt64() {
		return 0, billing.ErrOverflow
	}
	return total.Int64(), nil
}

func readAndValidateEstimatePage(ctx context.Context, q querier, in usage.EstimateInput) ([]usage.EstimateLine, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT record FROM billing_usage WHERE account_id=$1 AND funding='postpaid' AND scope_provider=$2 AND scope_merchant=$3 AND scope_environment=$4 AND scope_subscription_id=$5 AND scope_item_id=$6 AND occurred_at >= $7 AND occurred_at < $8 AND occurred_at < $9 ORDER BY usage_id COLLATE "C"`, string(in.Account), in.Period.Scope.Subscription.Scope.Provider, in.Period.Scope.Subscription.Scope.Merchant, in.Period.Scope.Subscription.Scope.Environment, in.Period.Scope.Subscription.ID, in.Period.Scope.ItemID, databaseTime(in.Period.Start), databaseTime(in.Period.End), databaseTime(in.Period.Cutoff))
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	lines := make([]usage.EstimateLine, 0, in.Limit)
	last := ""
	extra := false
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, "", err
		}
		var record usage.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, "", err
		}
		if record.ReceivedAt.After(in.Period.Cutoff) {
			continue
		}
		if err := validateEstimateRecord(record, in); err != nil {
			return nil, "", err
		}
		key := usage.UsageEstimateKey(record.Observation.ID)
		if key <= in.After {
			continue
		}
		if len(lines) < in.Limit {
			lines = append(lines, estimateLineFromRecord(record))
			last = key
			continue
		}
		extra = true
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if !extra {
		return lines, "", nil
	}
	return lines, last, nil
}

func rejectDuplicateEstimateAggregates(ctx context.Context, q querier, in usage.EstimateInput) error {
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM billing_usage WHERE account_id=$1 AND funding='postpaid' AND scope_provider=$2 AND scope_merchant=$3 AND scope_environment=$4 AND scope_subscription_id=$5 AND scope_item_id=$6 AND occurred_at >= $7 AND occurred_at < $8 AND occurred_at < $9 AND ((record->>'ReceivedAt') IS NULL OR (record->>'ReceivedAt')::timestamptz <= $9) AND record->'Rating'->>'Kind' IN ('3','4') GROUP BY scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,interval_start,interval_end,record->'Rating'->>'RuleVersion' HAVING COUNT(*) > 1 LIMIT 1`, string(in.Account), in.Period.Scope.Subscription.Scope.Provider, in.Period.Scope.Subscription.Scope.Merchant, in.Period.Scope.Subscription.Scope.Environment, in.Period.Scope.Subscription.ID, in.Period.Scope.ItemID, databaseTime(in.Period.Start), databaseTime(in.Period.End), databaseTime(in.Period.Cutoff)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return billing.ErrConflict
}

var _ usage.Estimator = (*Store)(nil)
