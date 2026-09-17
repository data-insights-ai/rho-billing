package credit

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func (s *AllowanceService) RecordEligibility(ctx context.Context, in EligibilityObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in = normalizeEligibilityObservation(in)
	if err := in.Validate(); err != nil {
		return err
	}
	return s.repo.WithinAccount(ctx, in.Account, func(tx AllowanceTx) error { return recordEligibility(ctx, tx, in) })
}

func normalizeEligibilityObservation(in EligibilityObservation) EligibilityObservation {
	in.EffectiveAt = billing.CanonicalTime(in.EffectiveAt)
	in.ObservedAt = billing.CanonicalTime(in.ObservedAt)
	if len(in.Eligibility.Evidence) == 0 {
		in.Eligibility.Evidence = nil
	} else {
		in.Eligibility.Evidence = slices.Clone(in.Eligibility.Evidence)
	}
	for i := range in.Eligibility.Evidence {
		e := &in.Eligibility.Evidence[i]
		e.Covered.Start = billing.CanonicalTime(e.Covered.Start)
		e.Covered.End = billing.CanonicalTime(e.Covered.End)
	}
	return in
}

func recordEligibility(ctx context.Context, tx AllowanceTx, in EligibilityObservation) error {
	in = normalizeEligibilityObservation(in)
	if err := in.Validate(); err != nil {
		return err
	}
	schedule, err := tx.Schedule(ctx, in.ScheduleID)
	if err != nil {
		return err
	}
	if schedule.Account != in.Account || schedule.ID != in.ScheduleID {
		return billing.ErrConflict
	}
	want, err := eligibilityFingerprint(in)
	if err != nil {
		return err
	}
	existing, err := tx.EligibilityBySource(ctx, in.ScheduleID, in.SourceID)
	if err == nil {
		if existing.Account != in.Account || existing.ScheduleID != in.ScheduleID || existing.SourceID != in.SourceID {
			return billing.ErrConflict
		}
		got, err := eligibilityFingerprint(normalizeEligibilityObservation(existing))
		if err != nil {
			return err
		}
		if got != want {
			return billing.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, billing.ErrNotFound) {
		return err
	}
	tied, err := tx.EligibilityAtVersion(ctx, in.ScheduleID, in.EffectiveAt, in.ObservedAt)
	if err == nil {
		if tied.Account != in.Account || tied.ScheduleID != in.ScheduleID {
			return billing.ErrConflict
		}
		got, err := eligibilityFingerprint(normalizeEligibilityObservation(tied))
		if err != nil {
			return err
		}
		if got != want {
			return billing.ErrConflict
		}
	} else if !errors.Is(err, billing.ErrNotFound) {
		return err
	}
	return tx.InsertEligibility(ctx, in)
}

func eligibilityFingerprint(in EligibilityObservation) (string, error) {
	raw, err := json.Marshal(in.Eligibility)
	if err != nil {
		return "", err
	}
	return identity.Fingerprint(string(raw), identity.Instant(in.EffectiveAt), identity.Instant(in.ObservedAt), string(in.Eligibility.Status)), nil
}
