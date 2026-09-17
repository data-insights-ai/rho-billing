package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/usage"
)

type settlementRepository struct {
	store *Store
	bound *settlementTx
}

func (s *Store) Settlements() usage.SettlementRepository { return &settlementRepository{store: s} }

func (s *session) Settlements() usage.SettlementRepository {
	return &settlementRepository{store: s.store, bound: &settlementTx{tx: s.tx, ctx: s.ctx, account: s.account}}
}

func (r *settlementRepository) WithinClose(ctx context.Context, account billing.AccountID, fn func(usage.CloseTx) error) error {
	if r == nil || r.store == nil || r.store.db == nil || fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.bound != nil {
		if account != r.bound.account {
			return billing.ErrNotFound
		}
		return fn(&settlementTx{tx: r.bound.tx, ctx: ctx, account: account})
	}
	return r.store.Atomic(ctx, account, func(scope integration.Session) error {
		return scope.Settlements().WithinClose(ctx, account, fn)
	})
}

func (s *session) WithinClose(ctx context.Context, account billing.AccountID, fn func(usage.CloseTx) error) error {
	return (&settlementRepository{store: s.store, bound: &settlementTx{tx: s.tx, ctx: s.ctx, account: s.account}}).WithinClose(ctx, account, fn)
}

type settlementTx struct {
	tx      *sql.Tx
	ctx     context.Context
	account billing.AccountID
}

func (t *settlementTx) BatchHeader(id string) (usage.BatchRecord, bool, error) {
	var s usage.BatchSummary
	var count int64
	var fingerprint string
	err := t.tx.QueryRowContext(t.ctx, `SELECT period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,total,state,revision,created_at,updated_at,fingerprint,line_count,COALESCE(original_batch_id,'') FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state NOT IN ('preparing','canceling')`, string(t.account), id).Scan(&s.Period.Start, &s.Period.End, &s.Period.Cutoff, &s.Period.Scope.Subscription.Scope.Provider, &s.Period.Scope.Subscription.Scope.Merchant, &s.Period.Scope.Subscription.Scope.Environment, &s.Period.Scope.Subscription.ID, &s.Period.Scope.ItemID, &s.Currency, &s.Total, &s.State, &s.Revision, &s.CreatedAt, &s.UpdatedAt, &fingerprint, &count, &s.OriginalBatchID)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.BatchRecord{}, false, nil
	}
	if err != nil {
		return usage.BatchRecord{}, false, err
	}
	s.Account = t.account
	s.ID = id
	s.LineCount = count
	s.Period = s.Period.UTC()
	return usage.BatchRecord{Summary: s, Fingerprint: fingerprint}, true, nil
}

func (t *settlementTx) UsageByIDs(ids []string) ([]usage.Record, error) {
	if len(ids) > 1000 {
		return nil, billing.ErrInvalid
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT usage_id,record FROM billing_usage WHERE account_id=$1 AND usage_id=ANY($2::text[])`, string(t.account), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[string]usage.Record, len(ids))
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var record usage.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		byID[id] = record
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byID) != len(ids) {
		return nil, billing.ErrNotFound
	}
	out := make([]usage.Record, 0, len(ids))
	for _, id := range ids {
		record, ok := byID[id]
		if !ok {
			return nil, billing.ErrNotFound
		}
		out = append(out, record)
	}
	return out, nil
}

func (t *settlementTx) OriginalUsageIDs(batchID string, ids []string) ([]string, error) {
	if !billing.ValidID(batchID) || len(ids) > 1000 {
		return nil, billing.ErrInvalid
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT usage_id FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2 AND usage_id=ANY($3::text[])`, string(t.account), batchID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(found))
	for _, id := range ids {
		if _, ok := found[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (t *settlementTx) UsageClaims(req []usage.UsageClaimRequest) (map[string]string, error) {
	if len(req) > 1000 {
		return nil, billing.ErrInvalid
	}
	out := make(map[string]string, len(req))
	if len(req) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(req))
	for _, r := range req {
		ids = append(ids, r.UsageID)
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT usage_id,batch_id FROM billing_settlement_usage_claims WHERE account_id=$1 AND usage_id=ANY($2::text[]) UNION ALL SELECT usage_id,job_id FROM billing_settlement_pending_claims WHERE account_id=$1 AND usage_id=ANY($2::text[])`, string(t.account), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		if _, duplicate := out[id]; duplicate {
			return nil, billing.ErrConflict
		}
		out[id] = owner
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *settlementTx) ReserveUsageClaims(batchID string, req []usage.UsageClaimRequest) error {
	if !billing.ValidID(batchID) || len(req) > 1000 {
		return billing.ErrInvalid
	}
	if len(req) == 0 {
		return nil
	}
	ids := make([]string, 0, len(req))
	fingerprints := make([]string, 0, len(req))
	for _, r := range req {
		if !billing.ValidID(r.UsageID) || r.SourceFingerprint == "" {
			return billing.ErrInvalid
		}
		ids = append(ids, r.UsageID)
		fingerprints = append(fingerprints, r.SourceFingerprint)
	}
	var pendingOwner string
	if err := t.tx.QueryRowContext(t.ctx, `SELECT COALESCE((SELECT job_id FROM billing_settlement_pending_claims WHERE account_id=$1 AND usage_id=ANY($2::text[]) LIMIT 1),'')`, string(t.account), ids).Scan(&pendingOwner); err != nil {
		return err
	}
	if pendingOwner != "" && pendingOwner != batchID {
		return billing.ErrConflict
	}
	if _, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_usage_claims(account_id,usage_id,batch_id,source_fingerprint) SELECT $1,u,batch,f FROM unnest($2::text[],$3::text[]) AS x(u,f) CROSS JOIN (SELECT $4::text AS batch) b ON CONFLICT DO NOTHING`, string(t.account), ids, fingerprints, batchID); err != nil {
		return err
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT usage_id,batch_id,source_fingerprint FROM billing_settlement_usage_claims WHERE account_id=$1 AND usage_id=ANY($2::text[])`, string(t.account), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	got := make(map[string][2]string, len(req))
	for rows.Next() {
		var id, owner, fp string
		if err := rows.Scan(&id, &owner, &fp); err != nil {
			return err
		}
		got[id] = [2]string{owner, fp}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		pair, ok := got[id]
		if !ok || pair[0] != batchID || pair[1] != fingerprints[i] {
			return billing.ErrConflict
		}
	}
	return nil
}

func (t *settlementTx) InsertCorrectionBatch(record usage.BatchRecord, lines []usage.ChargeLine) error {
	b := record.Summary
	if b.Account != t.account || !billing.ValidID(b.ID) || record.Fingerprint == "" || b.State != usage.BatchReady || b.LineCount != int64(len(lines)) || len(lines) > 1000 || !billing.ValidID(b.OriginalBatchID) || !b.Period.Valid() || b.Revision != 0 || b.CreatedAt.IsZero() || b.UpdatedAt.IsZero() {
		return billing.ErrInvalid
	}
	if b.OriginalBatchID != "" {
		var exists bool
		if err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM billing_settlement_batches WHERE account_id=$1 AND batch_id=$2 AND state NOT IN ('preparing','canceling'))`, string(t.account), b.OriginalBatchID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return billing.ErrNotFound
		}
	}
	var total int64
	for _, line := range lines {
		if line.Currency != b.Currency {
			return billing.ErrInvalid
		}
		var err error
		total, err = checked.Add(total, line.Amount)
		if err != nil {
			return err
		}
	}
	if total != b.Total {
		return billing.ErrConflict
	}
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_batches(account_id,batch_id,original_batch_id,period_start,period_end,cutoff,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,currency,total,state,revision,created_at,updated_at,fingerprint,line_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, string(t.account), b.ID, nullString(b.OriginalBatchID), databaseTime(b.Period.Start), databaseTime(b.Period.End), databaseTime(b.Period.Cutoff), b.Period.Scope.Subscription.Scope.Provider, b.Period.Scope.Subscription.Scope.Merchant, b.Period.Scope.Subscription.Scope.Environment, b.Period.Scope.Subscription.ID, b.Period.Scope.ItemID, b.Currency, b.Total, string(b.State), b.Revision, databaseTime(b.CreatedAt), databaseTime(b.UpdatedAt), record.Fingerprint, b.LineCount)
	if err != nil {
		return mapSettlementConstraint(err)
	}
	if len(lines) == 0 {
		return nil
	}
	var q strings.Builder
	q.WriteString(`INSERT INTO billing_settlement_lines(account_id,batch_id,line_key,kind,usage_id,adjustment_id,original_usage_id,source,actor,project,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,occurred_at,interval_start,interval_end,amount,exact_amount,rule_version,currency,reason) VALUES `)
	args := make([]any, 0, len(lines)*23)
	for i, line := range lines {
		if i > 0 {
			q.WriteByte(',')
		}
		q.WriteByte('(')
		for j := 0; j < 23; j++ {
			if j > 0 {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "$%d", len(args)+j+1)
		}
		q.WriteByte(')')
		args = append(args, string(t.account), b.ID, lineKeyForSettlement(line), line.Kind, line.UsageID, line.AdjustmentID, line.OriginalUsageID, line.Source, line.Actor, line.Project, line.Scope.Subscription.Scope.Provider, line.Scope.Subscription.Scope.Merchant, line.Scope.Subscription.Scope.Environment, line.Scope.Subscription.ID, line.Scope.ItemID, nullTime(line.OccurredAt), nullTime(line.Interval.Start), nullTime(line.Interval.End), line.Amount, line.ExactAmount, line.RuleVersion, line.Currency, line.Reason)
	}
	res, err := t.tx.ExecContext(t.ctx, q.String(), args...)
	if err != nil {
		return mapSettlementConstraint(err)
	}
	if err := expectSettlementRows(res, int64(len(lines))); err != nil {
		return err
	}
	return t.verifyCorrectionSources(b.ID, lines)
}

// Verify after insertion: trigger-time changes must roll back the entire command.
func (t *settlementTx) verifyCorrectionSources(batchID string, lines []usage.ChargeLine) error {
	return t.verifyUsageSources(batchID, lines, false)
}

// Read both bounded ID sets independently, so verification does not join
// the current page to a growing claim history.
func (t *settlementTx) verifyUsageSources(batchID string, lines []usage.ChargeLine, staged bool) error {
	ids := make([]string, 0, len(lines))
	byID := make(map[string]usage.ChargeLine, len(lines))
	for _, line := range lines {
		if line.Kind != "usage" {
			continue
		}
		if _, duplicate := byID[line.UsageID]; duplicate {
			return billing.ErrConflict
		}
		ids = append(ids, line.UsageID)
		byID[line.UsageID] = line
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 1000 {
		return billing.ErrInvalid
	}
	// OFFSET 0 keeps each lateral lookup correlated to one primary key;
	// prepared ANY predicates otherwise admitted scans of growing claim history.
	claimQuery := `SELECT c.usage_id,c.source_fingerprint,c.batch_id FROM unnest($2::text[]) AS wanted(id) CROSS JOIN LATERAL (SELECT usage_id,source_fingerprint,batch_id FROM billing_settlement_usage_claims WHERE account_id=$1 AND usage_id=wanted.id OFFSET 0) c`
	if staged {
		claimQuery = `SELECT c.usage_id,c.source_fingerprint,c.job_id FROM unnest($2::text[]) AS wanted(id) CROSS JOIN LATERAL (SELECT usage_id,source_fingerprint,job_id FROM billing_settlement_pending_claims WHERE account_id=$1 AND usage_id=wanted.id OFFSET 0) c`
	}
	rows, err := t.tx.QueryContext(t.ctx, claimQuery, string(t.account), ids)
	if err != nil {
		return err
	}
	claims := make(map[string]string, len(ids))
	for rows.Next() {
		var id, fingerprint, owner string
		if err := rows.Scan(&id, &fingerprint, &owner); err != nil {
			rows.Close()
			return err
		}
		if _, wanted := byID[id]; !wanted || owner != batchID {
			rows.Close()
			return billing.ErrConflict
		}
		if _, duplicate := claims[id]; duplicate {
			rows.Close()
			return billing.ErrConflict
		}
		claims[id] = fingerprint
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if len(claims) != len(ids) {
		return billing.ErrConflict
	}
	rows, err = t.tx.QueryContext(t.ctx, `SELECT usage_id,source,occurred_at,funding,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,fingerprint,record FROM billing_usage WHERE account_id=$1 AND usage_id=ANY($2::text[])`, string(t.account), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var e settlementUsageEvidence
		if err := rows.Scan(&id, &e.Source, &e.OccurredAt, &e.Funding, &e.Scope.Subscription.Scope.Provider, &e.Scope.Subscription.Scope.Merchant, &e.Scope.Subscription.Scope.Environment, &e.Scope.Subscription.ID, &e.Scope.ItemID, &e.Fingerprint, &e.Raw); err != nil {
			return err
		}
		line, found := byID[id]
		if !found {
			return billing.ErrConflict
		}
		if err := e.verify(t.account, line, claims[id]); err != nil {
			return err
		}
		delete(byID, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(byID) != 0 {
		return billing.ErrConflict
	}
	return nil
}

func (t *settlementTx) UpdateBatchState(summary usage.BatchSummary, expected int64) error {
	res, err := t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_batches SET state=$3,revision=$4,updated_at=$5 WHERE account_id=$1 AND batch_id=$2 AND revision=$6 AND state NOT IN ('preparing','canceling')`, string(t.account), summary.ID, string(summary.State), summary.Revision, databaseTime(summary.UpdatedAt), expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	return nil
}

func (r *settlementRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(usage.SettlementTx) error) error {
	if r == nil || r.store == nil || r.store.db == nil || fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.bound != nil {
		if account != r.bound.account {
			return billing.ErrNotFound
		}
		if err := r.bound.ctx.Err(); err != nil {
			return err
		}
		return fn(&settlementTx{tx: r.bound.tx, ctx: ctx, account: account})
	}
	return r.store.Atomic(ctx, account, func(scope integration.Session) error {
		return scope.Settlements().WithinAccount(ctx, account, fn)
	})
}

func (r *settlementRepository) Attempt(ctx context.Context, account billing.AccountID, id string) (usage.Attempt, error) {
	if r == nil || r.store == nil || r.store.db == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return usage.Attempt{}, billing.ErrInvalid
	}
	q, err := r.reader(ctx, account)
	if err != nil {
		return usage.Attempt{}, err
	}
	return readSettlementAttempt(ctx, q, account, id)
}

func readSettlementAttempt(ctx context.Context, q querier, account billing.AccountID, id string) (usage.Attempt, error) {
	var a usage.Attempt
	err := q.QueryRowContext(ctx, `SELECT batch_id,provider,idempotency_key,provider_reference,state,revision,supports_idempotency,supports_lookup,reason,created_at,updated_at FROM billing_settlement_attempts WHERE account_id=$1 AND attempt_id=$2`, string(account), id).Scan(&a.BatchID, &a.Provider, &a.IdempotencyKey, &a.ProviderReference, &a.State, &a.Revision, &a.Capability.SupportsIdempotency, &a.Capability.SupportsLookup, &a.Reason, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Attempt{}, billing.ErrNotFound
	}
	if err != nil {
		return usage.Attempt{}, err
	}
	a.ID = id
	return a, nil
}

func (t *settlementTx) Attempt(id string) (usage.Attempt, bool, error) {
	a, err := readSettlementAttempt(t.ctx, t.tx, t.account, id)
	if errors.Is(err, billing.ErrNotFound) {
		return usage.Attempt{}, false, nil
	}
	if err != nil {
		return usage.Attempt{}, false, err
	}
	return a, true, nil
}

func (t *settlementTx) PutAttempt(attempt usage.Attempt) error {
	if !billing.ValidID(attempt.ID) || !billing.ValidID(attempt.BatchID) || attempt.BatchID == "" || attempt.Provider == "" || attempt.IdempotencyKey == "" || attempt.Revision < 1 {
		return billing.ErrInvalid
	}
	old, err := readSettlementAttempt(t.ctx, t.tx, t.account, attempt.ID)
	if err == nil {
		if old.BatchID != attempt.BatchID || old.Provider != attempt.Provider || old.IdempotencyKey != attempt.IdempotencyKey || old.Capability != attempt.Capability || !old.CreatedAt.Equal(attempt.CreatedAt) {
			return billing.ErrConflict
		}
		_, err = t.tx.ExecContext(t.ctx, `UPDATE billing_settlement_attempts SET provider_reference=$3,state=$4,revision=$5,reason=$6,updated_at=$7 WHERE account_id=$1 AND attempt_id=$2`, string(t.account), attempt.ID, attempt.ProviderReference, string(attempt.State), attempt.Revision, attempt.Reason, databaseTime(attempt.UpdatedAt))
		return err
	}
	if !errors.Is(err, billing.ErrNotFound) {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_attempts(account_id,attempt_id,batch_id,provider,idempotency_key,provider_reference,state,revision,supports_idempotency,supports_lookup,reason,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, string(t.account), attempt.ID, attempt.BatchID, attempt.Provider, attempt.IdempotencyKey, attempt.ProviderReference, string(attempt.State), attempt.Revision, attempt.Capability.SupportsIdempotency, attempt.Capability.SupportsLookup, attempt.Reason, databaseTime(attempt.CreatedAt), databaseTime(attempt.UpdatedAt))
	return mapSettlementConstraint(err)
}

func (t *settlementTx) FindAttempt(provider, idempotencyKey string) (usage.Attempt, bool, error) {
	var id string
	err := t.tx.QueryRowContext(t.ctx, `SELECT attempt_id FROM billing_settlement_attempts WHERE account_id=$1 AND provider=$2 AND idempotency_key=$3`, string(t.account), provider, idempotencyKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Attempt{}, false, nil
	}
	if err != nil {
		return usage.Attempt{}, false, err
	}
	a, err := readSettlementAttempt(t.ctx, t.tx, t.account, id)
	return a, err == nil, err
}

func (t *settlementTx) Outcome(operation billing.OperationID) (usage.Outcome, bool, error) {
	var outcome usage.Outcome
	var raw []byte
	err := t.tx.QueryRowContext(t.ctx, `SELECT fingerprint,error,result FROM billing_settlement_operations WHERE account_id=$1 AND operation_id=$2`, string(t.account), string(operation)).Scan(&outcome.Fingerprint, &outcome.Error, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Outcome{}, false, nil
	}
	if err != nil {
		return usage.Outcome{}, false, err
	}
	// Batch is stored as the result object so attempt outcomes need a wrapper.
	var result struct {
		Batch   usage.BatchSummary `json:"Batch"`
		Attempt usage.Attempt      `json:"Attempt"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return usage.Outcome{}, false, err
	}
	outcome.Batch, outcome.Attempt = result.Batch, result.Attempt
	return outcome, true, nil
}

func (t *settlementTx) PutOutcome(operation billing.OperationID, outcome usage.Outcome) error {
	if !billing.ValidID(string(operation)) || outcome.Fingerprint == "" {
		return billing.ErrInvalid
	}
	result, err := json.Marshal(struct {
		Batch   usage.BatchSummary `json:"Batch"`
		Attempt usage.Attempt      `json:"Attempt"`
	}{outcome.Batch, outcome.Attempt})
	if err != nil {
		return err
	}
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO billing_settlement_operations(account_id,operation_id,fingerprint,error,result) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, string(t.account), string(operation), outcome.Fingerprint, outcome.Error, result)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var oldFingerprint, oldError string
	var oldResult []byte
	if err = t.tx.QueryRowContext(t.ctx, `SELECT fingerprint,error,result FROM billing_settlement_operations WHERE account_id=$1 AND operation_id=$2`, string(t.account), string(operation)).Scan(&oldFingerprint, &oldError, &oldResult); err != nil {
		return err
	}
	if oldFingerprint != outcome.Fingerprint || oldError != outcome.Error || !sameSettlementJSON(oldResult, result) {
		return billing.ErrConflict
	}
	return nil
}

func sameSettlementJSON(a, b []byte) bool {
	var left, right settlementResult
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return sameBatchSummary(left.Batch, right.Batch) && sameSettlementAttempt(left.Attempt, right.Attempt)
}

type settlementResult struct {
	Batch   usage.BatchSummary `json:"Batch"`
	Attempt usage.Attempt      `json:"Attempt"`
}

func sameSettlementAttempt(a, b usage.Attempt) bool {
	return a.BatchID == b.BatchID && a.ID == b.ID && a.Provider == b.Provider && a.IdempotencyKey == b.IdempotencyKey && a.ProviderReference == b.ProviderReference && a.State == b.State && a.Revision == b.Revision && a.Capability == b.Capability && a.Reason == b.Reason && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

func (r *settlementRepository) reader(ctx context.Context, account billing.AccountID) (querier, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.bound == nil {
		return r.store.db, nil
	}
	if account != r.bound.account {
		return nil, billing.ErrNotFound
	}
	if err := r.bound.ctx.Err(); err != nil {
		return nil, err
	}
	return r.bound.tx, nil
}

var _ usage.SettlementRepository = (*settlementRepository)(nil)
var _ usage.CloseRepository = (*settlementRepository)(nil)

type settlementUsageEvidence struct {
	Source, Funding, Fingerprint string
	OccurredAt                   time.Time
	Scope                        usage.BillingScope
	Raw                          json.RawMessage
}

func (e settlementUsageEvidence) verify(account billing.AccountID, line usage.ChargeLine, sourceFingerprint string) error {
	if e.Fingerprint != sourceFingerprint || sourceFingerprint == "" {
		return billing.ErrConflict
	}
	var record usage.Record
	if err := json.Unmarshal(e.Raw, &record); err != nil {
		return err
	}
	o, money := record.Observation, record.Rating.Money
	if o.Account != account || o.ID != line.UsageID || record.Fingerprint != sourceFingerprint || record.Fingerprint != usage.Identity(record) || e.Funding != string(usage.Postpaid) || e.Source != o.Source || e.Scope != o.Scope || !databaseTime(e.OccurredAt).Equal(databaseTime(o.OccurredAt)) || money == nil || record.Rating.Credits != nil || line.Source != o.Source || line.Actor != o.Actor || line.Project != o.Project || line.Scope != o.Scope || !databaseTime(line.OccurredAt).Equal(databaseTime(o.OccurredAt)) || !databaseTime(line.Interval.Start).Equal(databaseTime(o.Interval.Start)) || !databaseTime(line.Interval.End).Equal(databaseTime(o.Interval.End)) || line.Amount != money.MinorUnits || line.ExactAmount != record.Rating.ExactAmount || line.RuleVersion != record.Rating.RuleVersion || line.Currency != money.Currency || money.Currency != record.Rating.Target.Currency {
		return billing.ErrConflict
	}
	return nil
}

func sameBatchSummary(a, b usage.BatchSummary) bool {
	return a.Account == b.Account && a.ID == b.ID && a.OriginalBatchID == b.OriginalBatchID && a.Period.Scope == b.Period.Scope && a.Period.Start.Equal(b.Period.Start) && a.Period.End.Equal(b.Period.End) && a.Period.Cutoff.Equal(b.Period.Cutoff) && a.Currency == b.Currency && a.Total == b.Total && a.State == b.State && a.Revision == b.Revision && a.LineCount == b.LineCount && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

func lineKeyForSettlement(line usage.ChargeLine) string {
	if line.Kind == "adjustment" {
		return "adjustment:" + line.AdjustmentID
	}
	return "usage:" + line.UsageID
}

func mapSettlementConstraint(err error) error {
	if err == nil {
		return nil
	}
	// All callers hold the account lock, so constraint failures here represent
	// immutable identity conflicts rather than an expected concurrent race.
	if strings.Contains(err.Error(), "billing_settlement_") || strings.Contains(err.Error(), "duplicate key") {
		return fmt.Errorf("%w: settlement persistence: %v", billing.ErrConflict, err)
	}
	return err
}

func lockSettlementAccount(ctx context.Context, tx *sql.Tx, account billing.AccountID) error {
	var found string
	err := tx.QueryRowContext(ctx, `SELECT account_id FROM billing_accounts WHERE account_id=$1 FOR UPDATE`, string(account)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.ErrNotFound
	}
	return err
}
