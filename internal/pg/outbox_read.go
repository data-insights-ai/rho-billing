package pg

import (
	"context"
	"database/sql"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

// Outbox retrieves retained command evidence for a restarted host worker.
func (s *Store) Outbox(ctx context.Context, account billing.AccountID, scope billing.Scope, id string) (integration.Delivery, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(account)) || !scope.Valid() || !billing.ValidID(id) {
		return integration.Delivery{}, billing.ErrInvalid
	}
	var out integration.Delivery
	var fingerprint string
	var deadline, begun sql.NullTime
	var observationID, resultState, resultReference, resultEvidence, resultFingerprint sql.NullString
	var resultMode, resultMessageFingerprint sql.NullString
	var expectedPrevious sql.NullString
	var resultFence sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT o.account_id,o.message_id,o.provider,o.merchant,o.environment,
		o.kind,o.direction,o.occurred_at,o.payload,o.fingerprint,o.state,o.fence,o.attempts,o.available_at,
		o.lease_deadline,o.last_error,COALESCE(o.provider_reference,''),o.begun_at,o.payload_pruned_at IS NOT NULL,
		r.observation_id,r.state,r.provider_reference,r.evidence,r.result_fingerprint,r.mode,r.message_fingerprint,r.claim_fence,r.expected_previous
		FROM billing_outbox o LEFT JOIN billing_outbox_results r ON r.account_id=o.account_id
		AND r.message_id=o.message_id AND r.observation_id=o.last_observation_id
		WHERE o.account_id=$1 AND o.message_id=$2`, string(account), id).Scan(
		&out.Message.Account, &out.Message.ID, &out.Message.Scope.Provider, &out.Message.Scope.Merchant,
		&out.Message.Scope.Environment, &out.Message.Kind, &out.Message.Direction, &out.Message.OccurredAt,
		&out.Message.Payload, &fingerprint, &out.State, &out.Fence, &out.Attempt, &out.AvailableAt,
		&deadline, &out.LastError, &out.ProviderReference, &begun, &out.PayloadPruned,
		&observationID, &resultState, &resultReference, &resultEvidence, &resultFingerprint,
		&resultMode, &resultMessageFingerprint, &resultFence, &expectedPrevious)
	if errors.Is(err, sql.ErrNoRows) {
		return integration.Delivery{}, billing.ErrNotFound
	}
	if err != nil {
		return integration.Delivery{}, err
	}
	if out.Message.ProviderScope() != scope {
		return integration.Delivery{}, billing.ErrConflict
	}
	if out.Message.Validate() != nil || out.Message.Direction != integration.Outbound || out.Fence < 0 || out.Attempt < 0 {
		return integration.Delivery{}, billing.ErrState
	}
	if out.PayloadPruned {
		if (out.State != "completed" && out.State != "rejected") || len(out.Message.Payload) != 0 {
			return integration.Delivery{}, billing.ErrState
		}
	} else if out.Message.Fingerprint() != fingerprint {
		return integration.Delivery{}, billing.ErrState
	}
	if deadline.Valid {
		out.Deadline = billing.CanonicalTime(deadline.Time)
	}
	if begun.Valid {
		out.BegunAt = billing.CanonicalTime(begun.Time)
	}
	if observationID.Valid {
		result := integration.OutboxResult{ObservationID: observationID.String, ExpectedPrevious: expectedPrevious.String, State: integration.OutboxState(resultState.String), ProviderReference: resultReference.String, Evidence: resultEvidence.String}
		if result.Validate() != nil || resultMessageFingerprint.String != fingerprint || resultFence.Int64 != out.Fence || outboxResultFingerprint(resultMode.String, fingerprint, resultFence.Int64, result) != resultFingerprint.String || string(result.State) != string(out.State) {
			return integration.Delivery{}, billing.ErrState
		}
		out.LastResult = &result
	}
	return out, nil
}
