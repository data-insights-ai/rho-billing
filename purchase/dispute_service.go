package purchase

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type disputeServiceState struct {
	funding    Funding
	adjustment AdjustmentRecord
}

func (s *Service) ApplyDispute(ctx context.Context, fact DisputeFact) (DisputeResult, error) {
	if s == nil || s.repo == nil || s.now == nil {
		return DisputeResult{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return DisputeResult{}, err
	}
	if err := fact.Validate(); err != nil {
		return DisputeResult{}, err
	}
	if fact.Recovery != nil {
		if fact.DebitAdjustmentID != "" && fact.DebitAdjustmentID != fact.Recovery.AdjustmentID {
			return DisputeResult{}, billing.ErrConflict
		}
		fact.DebitAdjustmentID = fact.Recovery.AdjustmentID
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return DisputeResult{}, billing.ErrInvalid
	}
	fact.OccurredAt = billing.CanonicalTime(fact.OccurredAt)
	var out DisputeResult
	err := s.repo.WithinAccount(ctx, fact.Account, func(tx Tx) error {
		if old, err := tx.DisputeEvent(ctx, fact.Scope, fact.EventID); err == nil {
			if old.Validate() != nil || old.Fact.Fingerprint() != fact.Fingerprint() {
				return billing.ErrConflict
			}
			out = old.Result
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		state, err := loadDisputeState(ctx, tx, fact)
		if err != nil {
			return err
		}
		if err := validateDisputeTime(fact, state.funding.PaidAt, now); err != nil {
			return err
		}
		if fact.Amount > state.funding.Gross {
			return billing.ErrInvalid
		}
		caseValue, err := tx.Dispute(ctx, fact.Scope, fact.DisputeID)
		newCase := errors.Is(err, billing.ErrNotFound)
		if err != nil && !newCase {
			return err
		}
		if newCase {
			caseValue = Dispute{Account: fact.Account, Scope: fact.Scope, ID: fact.DisputeID, IntentID: fact.IntentID, TransactionID: fact.TransactionID, Currency: fact.Currency, Amount: fact.Amount, Status: fact.Status, StatusOccurredAt: fact.OccurredAt, StatusEventID: fact.EventID, DebitAdjustmentID: fact.DebitAdjustmentID, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if err := caseValue.Validate(); err != nil {
				return err
			}
		} else {
			if err := caseValue.Validate(); err != nil {
				return err
			}
			if now.Before(caseValue.UpdatedAt) {
				return billing.ErrState
			}
			if caseValue.ID != fact.DisputeID || caseValue.Account != fact.Account || caseValue.Scope != fact.Scope || caseValue.IntentID != fact.IntentID || caseValue.TransactionID != fact.TransactionID || caseValue.Currency != fact.Currency || caseValue.Amount != fact.Amount {
				return billing.ErrConflict
			}
			if caseValue.DebitAdjustmentID != "" && fact.DebitAdjustmentID != "" && caseValue.DebitAdjustmentID != fact.DebitAdjustmentID {
				return billing.ErrConflict
			}
			if caseValue.DebitAdjustmentID == "" {
				caseValue.DebitAdjustmentID = fact.DebitAdjustmentID
			}
		}
		if fact.DebitAdjustmentID != "" {
			if caseValue.DebitAdjustmentID != fact.DebitAdjustmentID {
				return billing.ErrConflict
			}
			if err := validateDebit(state.adjustment, fact); err != nil {
				return err
			}
		}
		out = DisputeResult{Account: fact.Account, DisputeID: fact.DisputeID, EventID: fact.EventID, Applied: true}
		if !newCase {
			if caseValue.Revision >= math.MaxInt64 {
				return billing.ErrOverflow
			}
			caseValue.Revision++
			caseValue.UpdatedAt = now
		}
		if !newCase {
			applyStatus(&caseValue, fact, &out)
		}
		if fact.Recovery != nil {
			if err := applyDisputeRecovery(ctx, tx, &caseValue, fact, state.adjustment, now, &out); err != nil {
				return err
			}
		}
		if newCase {
			out.StatusApplied = true
			if fact.Recovery != nil {
				caseValue.UpdatedAt = now
			}
		}
		if err := caseValue.Validate(); err != nil {
			return err
		}
		if err := tx.SaveDispute(ctx, caseValue, func() int64 {
			if newCase {
				return 0
			}
			return caseValue.Revision - 1
		}()); err != nil {
			return err
		}
		record := DisputeRecord{Fact: fact, Result: out, CreatedAt: now}
		if err := record.Validate(); err != nil {
			return err
		}
		if err := tx.InsertDisputeEvent(ctx, record); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return DisputeResult{}, err
	}
	return out, nil
}

func loadDisputeState(ctx context.Context, tx Tx, f DisputeFact) (disputeServiceState, error) {
	intent, err := tx.Intent(ctx, f.IntentID)
	if err != nil {
		return disputeServiceState{}, err
	}
	if intent.Validate() != nil || intent.Account != f.Account || intent.ID != f.IntentID {
		return disputeServiceState{}, billing.ErrState
	}
	if intent.Scope != f.Scope || intent.Currency != f.Currency {
		return disputeServiceState{}, billing.ErrConflict
	}
	if intent.Payment != PaymentPaid {
		return disputeServiceState{}, billing.ErrNotFound
	}
	if intent.TransactionID != f.TransactionID {
		return disputeServiceState{}, billing.ErrConflict
	}
	funding, err := tx.Funding(ctx, f.Scope, f.TransactionID)
	if err != nil {
		return disputeServiceState{}, err
	}
	if err := funding.Validate(); err != nil || funding.Account != f.Account || funding.IntentID != f.IntentID || funding.Scope != f.Scope || funding.TransactionID != f.TransactionID || funding.Currency != f.Currency || !funding.PaidAt.Equal(intent.PaidAt) {
		return disputeServiceState{}, billing.ErrState
	}
	state := disputeServiceState{funding: funding}
	if f.DebitAdjustmentID != "" {
		state.adjustment, err = tx.Adjustment(ctx, f.DebitAdjustmentID)
		if err != nil {
			return state, err
		}
	}
	return state, nil
}
func validateDebit(a AdjustmentRecord, f DisputeFact) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if !a.Result.Applied || a.Input.ID != f.DebitAdjustmentID || a.Input.Account != f.Account || a.Input.IntentID != f.IntentID || a.Input.Scope != f.Scope || a.Input.TransactionID != f.TransactionID || a.Input.Currency != f.Currency || a.Input.Kind != AdjustmentChargeback {
		return billing.ErrState
	}
	var total int64
	for _, line := range a.Input.Lines {
		var err error
		total, err = checked.Add(total, line.Gross)
		if err != nil {
			return err
		}
	}
	if total > f.Amount {
		return billing.ErrConflict
	}
	return nil
}
func applyStatus(d *Dispute, f DisputeFact, out *DisputeResult) {
	if f.OccurredAt.Before(d.StatusOccurredAt) {
		out.StatusApplied = false
		out.IgnoredStatusReason = "stale_status"
		return
	}
	if f.OccurredAt.Equal(d.StatusOccurredAt) && f.Status != d.Status {
		out.StatusApplied = false
		out.IgnoredStatusReason = "equal_time_conflict"
		return
	}
	if f.OccurredAt.Equal(d.StatusOccurredAt) && f.Status == d.Status {
		out.StatusApplied = true
		return
	}
	if !allowedDisputeTransition(d.Status, f.Status) {
		out.StatusApplied = false
		out.IgnoredStatusReason = "invalid_transition"
		return
	}
	d.Status = f.Status
	d.StatusOccurredAt = f.OccurredAt
	d.StatusEventID = f.EventID
	out.StatusApplied = true
}
func allowedDisputeTransition(from, to DisputeStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case DisputeWarning:
		return to == DisputeOpen || to == DisputeUnderReview || to == DisputeLost || to == DisputeWon || to == DisputeClosed
	case DisputeOpen:
		return to == DisputeUnderReview || to == DisputeLost || to == DisputeWon || to == DisputeClosed
	case DisputeUnderReview:
		return to == DisputeLost || to == DisputeWon || to == DisputeClosed
	case DisputeLost:
		return to == DisputeUnderReview || to == DisputeWon || to == DisputeClosed
	case DisputeWon:
		return to == DisputeClosed
	case DisputeClosed:
		return false
	}
	return false
}
func applyDisputeRecovery(ctx context.Context, tx Tx, d *Dispute, f DisputeFact, a AdjustmentRecord, now time.Time, out *DisputeResult) error {
	r := *f.Recovery
	if r.AdjustmentID != d.DebitAdjustmentID {
		return billing.ErrConflict
	}
	if old, err := tx.DisputeRecovery(ctx, f.Scope, r.ID); err == nil {
		expected := DisputeRecoveryRecord{Account: f.Account, Scope: f.Scope, DisputeID: d.ID, IntentID: d.IntentID, TransactionID: d.TransactionID, Recovery: r}
		if old.Validate() != nil || old.RecoveryFingerprint() != expected.RecoveryFingerprint() {
			return billing.ErrConflict
		}
		out.RecoveryApplied = false
		return nil
	} else if !errors.Is(err, billing.ErrNotFound) {
		return err
	}
	if err := validateRecoveryLines(r.Lines, a.Input.Lines, d.Recovered); err != nil {
		return err
	}
	aggregated := make(map[string]PaidLine, len(d.Recovered)+len(r.Lines))
	for _, line := range d.Recovered {
		aggregated[line.LineID] = line
	}
	for _, line := range r.Lines {
		old := aggregated[line.LineID]
		merged, err := checkedLineAdd(old, line)
		if err != nil {
			return err
		}
		merged.LineID = line.LineID
		aggregated[line.LineID] = merged
	}
	d.Recovered = make([]PaidLine, 0, len(aggregated))
	for _, line := range aggregated {
		d.Recovered = append(d.Recovered, line)
	}
	d.Recovered = sortedDisputeLines(d.Recovered)
	d.UpdatedAt = now
	rec := DisputeRecoveryRecord{Account: f.Account, Scope: f.Scope, DisputeID: d.ID, IntentID: d.IntentID, TransactionID: d.TransactionID, Recovery: r, CreatedAt: now}
	if err := rec.Validate(); err != nil {
		return err
	}
	if err := tx.InsertDisputeRecovery(ctx, rec); err != nil {
		return err
	}
	out.RecoveryApplied = true
	return nil
}

func validateRecoveryLines(lines, original, prior []PaidLine) error {
	bases := make(map[string]PaidLine, len(original))
	used := make(map[string]PaidLine, len(prior))
	for _, line := range original {
		bases[line.LineID] = line
	}
	for _, line := range prior {
		base, ok := bases[line.LineID]
		if !ok || line.Gross > base.Gross || line.Tax > base.Tax || line.Gross-line.Tax > base.Gross-base.Tax {
			return billing.ErrConflict
		}
		used[line.LineID] = line
	}
	for _, line := range lines {
		base, ok := bases[line.LineID]
		if !ok {
			return billing.ErrConflict
		}
		merged, err := checkedLineAdd(used[line.LineID], line)
		if err != nil {
			return err
		}
		if merged.Gross > base.Gross || merged.Tax > base.Tax || merged.Gross-merged.Tax > base.Gross-base.Tax {
			return billing.ErrConflict
		}
		used[line.LineID] = merged
	}
	return nil
}

func (s *Service) Dispute(ctx context.Context, account billing.AccountID, scope billing.Scope, id string) (Dispute, error) {
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) || !scope.Valid() || !billing.ValidID(id) {
		return Dispute{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Dispute{}, err
	}
	var out Dispute
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		value, err := tx.Dispute(ctx, scope, id)
		if err != nil {
			return err
		}
		if err := value.Validate(); err != nil || value.Account != account || value.Scope != scope || value.ID != id {
			return billing.ErrState
		}
		value.Recovered = slices.Clone(value.Recovered)
		out = value
		return nil
	})
	if err != nil {
		return Dispute{}, err
	}
	return out, nil
}
