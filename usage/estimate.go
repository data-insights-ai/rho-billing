package usage

import (
	"context"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type EstimateStatus string

const (
	StatusEstimated EstimateStatus = "estimated"
	StatusFinalized EstimateStatus = "finalized"
)

type EstimatePeriod struct {
	Start  time.Time
	End    time.Time
	Cutoff time.Time
	Scope  BillingScope
}

func (p EstimatePeriod) Valid() bool {
	return !p.Start.IsZero() && p.End.After(p.Start) && !p.Cutoff.Before(p.Start) && p.Scope.Valid()
}

func (p EstimatePeriod) UTC() EstimatePeriod {
	p.Start = billing.CanonicalTime(p.Start)
	p.End = billing.CanonicalTime(p.End)
	p.Cutoff = billing.CanonicalTime(p.Cutoff)
	return p
}

type EstimateInput struct {
	Account  billing.AccountID
	Period   EstimatePeriod
	Currency string
	BatchID  string
	After    string
	Limit    int
}

func (in EstimateInput) Valid() bool {
	return billing.ValidID(string(in.Account)) && in.Period.Valid() && validEstimateCurrency(in.Currency) && (in.BatchID == "" || billing.ValidID(in.BatchID)) && in.Limit >= 1 && in.Limit <= 1000
}

type EstimateLine struct {
	Key             string
	Kind            ChargeKind
	UsageID         string
	AdjustmentID    string
	OriginalUsageID string
	Source          string
	Actor           string
	Project         string
	Scope           BillingScope
	OccurredAt      time.Time
	Interval        billing.Period
	Amount          int64
	ExactAmount     string
	RuleVersion     string
	Currency        string
	Reason          string
	Fingerprint     string
}

type Estimate struct {
	Status    EstimateStatus
	Account   billing.AccountID
	Period    EstimatePeriod
	Currency  string
	BatchID   string
	AsOf      time.Time
	Total     int64
	Lines     []EstimateLine
	NextAfter string
}

type Estimator interface {
	EstimateUsage(context.Context, EstimateInput) (Estimate, error)
}

func UsageEstimateKey(id string) string { return "usage:" + id }

func AdjustmentEstimateKey(id string) string { return "adjustment:" + id }

func validEstimateCurrency(currency string) bool {
	if len(currency) != 3 || strings.ToUpper(currency) != currency {
		return false
	}
	for i := range currency {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}
