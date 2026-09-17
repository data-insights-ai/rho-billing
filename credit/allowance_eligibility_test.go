package credit

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestEligibilityRequiresExplicitCoverageAndBlocksFailureStates(t *testing.T) {
	period := billing.Period{Start: utcDate(2026, time.January, 1), End: utcDate(2026, time.February, 1)}
	eligible := Eligibility{Status: EligibilityPaid, Evidence: []EligibilityEvidence{{Kind: EvidencePayment, Reference: "payment-1", Covered: billing.Period{Start: utcDate(2025, time.December, 1), End: utcDate(2026, time.January, 15)}}}}
	if !errors.Is(eligible.Covers(period), ErrIneligible) {
		t.Fatal("partial payment coverage should reject")
	}
	eligible.Evidence = append(eligible.Evidence, EligibilityEvidence{Kind: EvidencePayment, Reference: "payment-2", Covered: billing.Period{Start: utcDate(2026, time.January, 15), End: utcDate(2026, time.February, 1)}})
	if err := eligible.Covers(period); err != nil {
		t.Fatal(err)
	}
	for _, status := range []EligibilityStatus{EligibilityCanceled, EligibilityPaused, EligibilityDelinquent} {
		if !errors.Is((Eligibility{Status: status}).Covers(period), ErrIneligible) {
			t.Fatalf("status %d should reject", status)
		}
	}
	trial := Eligibility{Status: EligibilityTrial, Evidence: []EligibilityEvidence{{Kind: EvidenceTrial, Reference: "trial-1", Policy: "trial-policy", Covered: period}}}
	if err := trial.Covers(period); err != nil {
		t.Fatal(err)
	}
	grace := Eligibility{Status: EligibilityGrace, Evidence: []EligibilityEvidence{{Kind: EvidenceGrace, Reference: "renewal-1", Policy: "grace-policy", Covered: period}}}
	if err := grace.Covers(period); err != nil {
		t.Fatal(err)
	}
}

func paidEligibility(start, end time.Time) Eligibility {
	return Eligibility{Status: EligibilityPaid, Evidence: []EligibilityEvidence{{Kind: EvidencePayment, Reference: "payment", Covered: billing.Period{Start: start, End: end}}}}
}
