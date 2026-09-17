package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func (t *allowanceTransaction) EligibilityBySource(ctx context.Context, scheduleID, sourceID string) (credit.EligibilityObservation, error) {
	return readEligibilityObservation(t.session.tx.QueryRowContext(ctx, `SELECT source_id,effective_at,observed_at,eligibility,fingerprint FROM billing_allowance_eligibility_events WHERE account_id=$1 AND schedule_id=$2 AND source_id=$3`, string(t.session.account), scheduleID, sourceID), t.session.account, scheduleID)
}
func (t *allowanceTransaction) EligibilityAtVersion(ctx context.Context, scheduleID string, effectiveAt, observedAt time.Time) (credit.EligibilityObservation, error) {
	return readEligibilityObservation(t.session.tx.QueryRowContext(ctx, `SELECT source_id,effective_at,observed_at,eligibility,fingerprint FROM billing_allowance_eligibility_events WHERE account_id=$1 AND schedule_id=$2 AND effective_at=$3 AND observed_at=$4 ORDER BY source_id LIMIT 1`, string(t.session.account), scheduleID, databaseTime(effectiveAt), databaseTime(observedAt)), t.session.account, scheduleID)
}
func readEligibilityObservation(row *sql.Row, account billing.AccountID, scheduleID string) (credit.EligibilityObservation, error) {
	in := credit.EligibilityObservation{Account: account, ScheduleID: scheduleID}
	var raw []byte
	var fingerprint string
	err := row.Scan(&in.SourceID, &in.EffectiveAt, &in.ObservedAt, &raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.EligibilityObservation{}, billing.ErrNotFound
	}
	if err != nil {
		return credit.EligibilityObservation{}, err
	}
	if err := json.Unmarshal(raw, &in.Eligibility); err != nil {
		return credit.EligibilityObservation{}, err
	}
	in.EffectiveAt = billing.CanonicalTime(in.EffectiveAt)
	in.ObservedAt = billing.CanonicalTime(in.ObservedAt)
	encoded, err := json.Marshal(in.Eligibility)
	if err != nil {
		return credit.EligibilityObservation{}, err
	}
	expected := identity.Fingerprint(string(encoded), identity.Instant(in.EffectiveAt), identity.Instant(in.ObservedAt), string(in.Eligibility.Status))
	if expected != fingerprint {
		return credit.EligibilityObservation{}, billing.ErrConflict
	}
	return in, nil
}
func (t *allowanceTransaction) InsertEligibility(ctx context.Context, in credit.EligibilityObservation) error {
	s := t.session
	if err := s.scope(in.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Eligibility)
	if err != nil {
		return err
	}
	fp := identity.Fingerprint(string(raw), identity.Instant(in.EffectiveAt), identity.Instant(in.ObservedAt), string(in.Eligibility.Status))
	_, err = s.tx.ExecContext(ctx, `INSERT INTO billing_allowance_eligibility_events(account_id,schedule_id,source_id,effective_at,observed_at,status,eligibility,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, string(s.account), in.ScheduleID, in.SourceID, in.EffectiveAt, in.ObservedAt, eligibilityStatus(in.Eligibility.Status), raw, fp)
	if err != nil {
		return err
	}
	for _, evidence := range in.Eligibility.Evidence {
		if _, err = s.tx.ExecContext(ctx, `INSERT INTO billing_allowance_eligibility_evidence(account_id,schedule_id,source_id,kind,reference,policy,covered_start,covered_end) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, string(s.account), in.ScheduleID, in.SourceID, evidenceKind(evidence.Kind), evidence.Reference, evidence.Policy, evidence.Covered.Start, evidence.Covered.End); err != nil {
			return err
		}
	}
	return t.recordAllowanceChange(ctx, in.ScheduleID, in.EffectiveAt, "eligibility")
}

func (t *allowanceTransaction) recordAllowanceChange(ctx context.Context, scheduleID string, effectiveAt time.Time, kind string) error {
	_, err := t.session.tx.ExecContext(ctx, `INSERT INTO billing_allowance_change_journal(account_id,schedule_id,changed_at,effective_at,kind) VALUES($1,$2,CURRENT_TIMESTAMP,$3,$4)`, string(t.session.account), scheduleID, databaseTime(effectiveAt), kind)
	return err
}
