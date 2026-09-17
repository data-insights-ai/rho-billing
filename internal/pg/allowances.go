package pg

import (
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func scheduleStateString(s credit.ScheduleState) string { return string(s) }

func scheduleAdjustmentString(mode credit.AdjustmentMode) string {
	switch mode {
	case 0, credit.AdjustmentInitial:
		return "initial"
	case credit.AdjustmentReject:
		return "reject"
	case credit.AdjustmentDelta:
		return "delta"
	case credit.AdjustmentProrated:
		return "prorated"
	default:
		return ""
	}
}

func parseScheduleAdjustment(mode string) (credit.AdjustmentMode, error) {
	switch mode {
	case "", "initial":
		return credit.AdjustmentInitial, nil
	case "reject":
		return credit.AdjustmentReject, nil
	case "delta":
		return credit.AdjustmentDelta, nil
	case "prorated":
		return credit.AdjustmentProrated, nil
	default:
		return 0, billing.ErrConflict
	}
}

type boundAllowances struct{ session *session }

func (s *session) Allowances() credit.AllowanceRepository { return &boundAllowances{session: s} }

func normalizeStoredSchedule(in credit.Schedule) credit.Schedule {
	in.Assignment.Effective.Start = billing.CanonicalTime(in.Assignment.Effective.Start)
	in.Assignment.Effective.End = billing.CanonicalTime(in.Assignment.Effective.End)
	in.StateEffectiveAt = billing.CanonicalTime(in.StateEffectiveAt)
	if !in.Anchor.IsZero() {
		in.Anchor = billing.CanonicalTime(in.Anchor)
	}
	return in
}

func eligibilityStatus(s credit.EligibilityStatus) string {
	switch s {
	case credit.EligibilityPaid:
		return "paid"
	case credit.EligibilityTrial:
		return "trial"
	case credit.EligibilityGrace:
		return "grace"
	case credit.EligibilityCanceled:
		return "canceled"
	case credit.EligibilityPaused:
		return "paused"
	case credit.EligibilityDelinquent:
		return "delinquent"
	default:
		return ""
	}
}

func evidenceKind(k credit.EvidenceKind) string {
	switch k {
	case credit.EvidencePayment:
		return "payment"
	case credit.EvidenceTrial:
		return "trial"
	case credit.EvidenceGrace:
		return "grace"
	default:
		return ""
	}
}

var _ credit.AllowanceRepository = (*allowanceRepository)(nil)
var _ credit.AllowanceRepository = (*boundAllowances)(nil)
