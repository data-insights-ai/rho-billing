package credit

import (
	"cmp"
	"slices"

	billing "github.com/data-insights-ai/rho-billing"
)

type EligibilityStatus uint8

const (
	EligibilityPaid EligibilityStatus = iota + 1
	EligibilityTrial
	EligibilityGrace
	EligibilityCanceled
	EligibilityPaused
	EligibilityDelinquent
)

type EvidenceKind uint8

const (
	EvidencePayment EvidenceKind = iota + 1
	EvidenceTrial
	EvidenceGrace
)

type EligibilityEvidence struct {
	Kind      EvidenceKind
	Reference string
	Covered   billing.Period
	Policy    string // required for trial and grace evidence
}

type Eligibility struct {
	Status   EligibilityStatus
	Evidence []EligibilityEvidence
}

func (e Eligibility) Covers(period billing.Period) error {
	if !period.Valid() {
		return billing.ErrInvalid
	}
	if e.Status < EligibilityPaid || e.Status > EligibilityDelinquent {
		return billing.ErrInvalid
	}
	if e.Status >= EligibilityCanceled {
		return ErrIneligible
	}
	wanted := EvidencePayment
	switch e.Status {
	case EligibilityTrial:
		wanted = EvidenceTrial
	case EligibilityGrace:
		wanted = EvidenceGrace
	}
	coverage := make([]billing.Period, 0, len(e.Evidence))
	for _, item := range e.Evidence {
		if err := item.validate(); err != nil {
			return err
		}
		if item.Kind == wanted {
			coverage = append(coverage, item.Covered)
		}
	}
	if !covers(coverage, period) {
		return ErrIneligible
	}
	return nil
}

func (e EligibilityEvidence) validate() error {
	if e.Kind < EvidencePayment || e.Kind > EvidenceGrace || !e.Covered.Valid() || !billing.ValidID(e.Reference) {
		return billing.ErrInvalid
	}
	if (e.Kind == EvidenceTrial || e.Kind == EvidenceGrace) && !billing.ValidID(e.Policy) {
		return billing.ErrInvalid
	}
	return nil
}

func covers(intervals []billing.Period, wanted billing.Period) bool {
	if len(intervals) == 0 {
		return false
	}
	slices.SortFunc(intervals, func(a, b billing.Period) int {
		return cmp.Or(a.Start.Compare(b.Start), a.End.Compare(b.End))
	})
	cursor := wanted.Start
	for _, interval := range intervals {
		if interval.End.Before(cursor) || interval.End.Equal(cursor) {
			continue
		}
		if interval.Start.After(cursor) {
			return false
		}
		if interval.End.After(cursor) {
			cursor = interval.End
		}
		if !cursor.Before(wanted.End) {
			return true
		}
	}
	return !cursor.Before(wanted.End)
}
