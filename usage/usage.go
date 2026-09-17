package usage

import (
	"slices"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type Funding string

const (
	Postpaid Funding = "postpaid"
	Prepaid  Funding = "prepaid"
	Waived   Funding = "waived"
)

type BillingScope struct {
	Subscription billing.Reference
	ItemID       string
}

func (s BillingScope) AccountWide() bool {
	return s.Subscription == (billing.Reference{}) && s.ItemID == ""
}

func (s BillingScope) Valid() bool {
	if s.AccountWide() {
		return true
	}
	return s.Subscription.Valid() && billing.ValidID(s.ItemID)
}

type Observation struct {
	Account                    billing.AccountID
	ID, Source, Actor, Project string
	CreditScope                string
	Scope                      BillingScope
	OccurredAt                 time.Time
	Interval                   billing.Period
	Funding                    Funding
	ReservationID              string
	Input                      RateInput
}
type Record struct {
	Observation Observation
	Rating      Result
	ReceivedAt  time.Time
	Fingerprint string
}

func Prepare(o Observation, rule *Rule, now time.Time) (Record, error) {
	o.OccurredAt = billing.CanonicalTime(o.OccurredAt)
	o.Interval.Start = billing.CanonicalTime(o.Interval.Start)
	o.Interval.End = billing.CanonicalTime(o.Interval.End)
	now = billing.CanonicalTime(now)
	if rule == nil || !billing.ValidID(string(o.Account)) || !billing.ValidID(o.ID) || !billing.ValidID(o.Source) || !o.Scope.Valid() || o.OccurredAt.IsZero() || now.IsZero() {
		return Record{}, billing.ErrInvalid
	}
	if o.Funding != Postpaid && o.Funding != Prepaid && o.Funding != Waived {
		return Record{}, billing.ErrInvalid
	}
	if (o.Funding == Prepaid) != billing.ValidID(o.ReservationID) {
		return Record{}, billing.ErrInvalid
	}
	if o.Funding == Waived && !o.Input.Waived || o.Funding != Waived && o.Input.Waived {
		return Record{}, billing.ErrInvalid
	}
	if !o.Interval.Start.IsZero() || !o.Interval.End.IsZero() {
		if !o.Interval.Valid() || !o.Interval.Contains(o.OccurredAt) {
			return Record{}, billing.ErrInvalid
		}
	}
	rated, err := rule.Rate(o.Input)
	if err != nil {
		return Record{}, err
	}
	if o.Funding == Postpaid && rated.Money == nil || o.Funding == Prepaid && rated.Credits == nil {
		return Record{}, billing.ErrInvalid
	}
	o.Input.Metrics = slices.Clone(rated.Evidence.Metrics)
	o.Input.Adjustment = rated.Evidence.Adjustment
	o.OccurredAt = billing.CanonicalTime(o.OccurredAt)
	o.Interval.Start = billing.CanonicalTime(o.Interval.Start)
	o.Interval.End = billing.CanonicalTime(o.Interval.End)
	r := Record{Observation: o, Rating: rated, ReceivedAt: billing.CanonicalTime(now)}
	r.Fingerprint = Identity(r)
	return r, nil
}

func Identity(r Record) string {
	o := r.Observation
	rr := r.Rating
	f := []string{string(o.Account), o.ID, o.Source, o.Actor, o.Project, o.Scope.Subscription.Scope.Provider, o.Scope.Subscription.Scope.Merchant, o.Scope.Subscription.Scope.Environment, o.Scope.Subscription.ID, o.Scope.ItemID, identity.Instant(o.OccurredAt), identity.Instant(o.Interval.Start), identity.Instant(o.Interval.End), string(o.Funding), o.ReservationID,
		rr.RuleVersion, rr.Kind.String(), rr.Target.Currency, rr.Target.CreditUnit, rr.Rounding.String(), rr.ExactAmount, strconv.FormatInt(rr.RoundedAmount, 10), strconv.FormatInt(o.Input.ActionCount, 10), strconv.FormatBool(o.Input.Waived), o.Input.Adjustment, o.Input.AdjustmentReason}
	for _, m := range o.Input.Metrics {
		f = append(f, m.Name, strconv.FormatInt(m.Quantity, 10))
	}
	f = append(f, "credit-scope", o.CreditScope)
	return identity.Fingerprint(f...)
}

// ValidatePostpaidAggregate: included-quota and graduated ratings need the
// complete billing window as one aggregate; rating individual events and
// summing them would reset quota or tier state for every event.
func ValidatePostpaidAggregate(record Record, start, end time.Time) error {
	if record.Observation.Funding != Postpaid || (record.Rating.Kind != KindIncludedQuota && record.Rating.Kind != KindGraduated) {
		return nil
	}
	interval := record.Observation.Interval
	if !interval.Valid() || !billing.CanonicalTime(interval.Start).Equal(billing.CanonicalTime(start)) || !billing.CanonicalTime(interval.End).Equal(billing.CanonicalTime(end)) {
		return billing.ErrInvalid
	}
	return nil
}

// PostpaidAggregateIdentity excludes source and rule version so a second stream
// or mid-period published rule cannot reset included quota or graduated tiers.
// The recorded rule version remains frozen rating evidence.
func PostpaidAggregateIdentity(record Record, start, end time.Time) string {
	if record.Observation.Funding != Postpaid || (record.Rating.Kind != KindIncludedQuota && record.Rating.Kind != KindGraduated) {
		return ""
	}
	o := record.Observation
	return identity.Fingerprint(string(o.Account), o.Scope.Subscription.Scope.Provider, o.Scope.Subscription.Scope.Merchant, o.Scope.Subscription.Scope.Environment, o.Scope.Subscription.ID, o.Scope.ItemID, identity.Instant(start), identity.Instant(end))
}

func Copy(r Record) Record {
	r.Observation.Input.Metrics = slices.Clone(r.Observation.Input.Metrics)
	r.Rating.Evidence.Metrics = slices.Clone(r.Rating.Evidence.Metrics)
	if r.Rating.Money != nil {
		v := *r.Rating.Money
		r.Rating.Money = &v
	}
	if r.Rating.Credits != nil {
		v := *r.Rating.Credits
		r.Rating.Credits = &v
	}
	return r
}

func ValidateStoredRecord(record Record) error {
	o := record.Observation
	if !billing.ValidID(string(o.Account)) || !billing.ValidID(o.ID) || !billing.ValidID(o.Source) || !o.Scope.Valid() || o.OccurredAt.IsZero() || record.ReceivedAt.IsZero() {
		return billing.ErrInvalid
	}
	if o.Funding != Postpaid && o.Funding != Prepaid && o.Funding != Waived {
		return billing.ErrInvalid
	}
	if (o.Funding == Prepaid) != billing.ValidID(o.ReservationID) {
		return billing.ErrInvalid
	}
	if (o.Funding == Waived) != o.Input.Waived {
		return billing.ErrInvalid
	}
	if !o.Interval.Start.IsZero() || !o.Interval.End.IsZero() {
		if !o.Interval.Valid() || !o.Interval.Contains(o.OccurredAt) {
			return billing.ErrInvalid
		}
	}
	if record.Rating.RuleVersion == "" || record.Rating.ExactAmount == "" || record.Rating.RoundedAmount < 0 || record.Rating.Evidence.Waived != o.Input.Waived {
		return billing.ErrInvalid
	}
	if o.Input.Waived && record.Rating.RoundedAmount != 0 {
		return billing.ErrInvalid
	}
	if err := validateStoredResult(o.Funding, record.Rating); err != nil {
		return err
	}
	if record.Rating.Evidence.ActionCount != o.Input.ActionCount || record.Rating.Evidence.Adjustment != o.Input.Adjustment || record.Rating.Evidence.AdjustmentReason != o.Input.AdjustmentReason || !slices.Equal(record.Rating.Evidence.Metrics, o.Input.Metrics) {
		return billing.ErrInvalid
	}
	return nil
}

func validateStoredResult(funding Funding, result Result) error {
	if (result.Money == nil) == (result.Credits == nil) {
		return billing.ErrInvalid
	}
	if funding == Prepaid {
		if result.Credits == nil || result.Target.CreditUnit == "" || result.Target.Currency != "" || result.Credits.Unit != result.Target.CreditUnit || result.Credits.Subunits != result.RoundedAmount {
			return billing.ErrInvalid
		}
		return nil
	}
	if funding == Waived {
		if result.Money != nil {
			if result.Target.Currency == "" || result.Target.CreditUnit != "" || result.Money.Currency != result.Target.Currency || result.Money.MinorUnits != result.RoundedAmount {
				return billing.ErrInvalid
			}
			return nil
		}
		if result.Target.CreditUnit == "" || result.Target.Currency != "" || result.Credits.Unit != result.Target.CreditUnit || result.Credits.Subunits != result.RoundedAmount {
			return billing.ErrInvalid
		}
		return nil
	}
	if result.Money == nil || result.Target.Currency == "" || result.Target.CreditUnit != "" || result.Money.Currency != result.Target.Currency || result.Money.MinorUnits != result.RoundedAmount {
		return billing.ErrInvalid
	}
	return nil
}

func canonicalRecord(record Record) Record {
	record.Observation.OccurredAt = billing.CanonicalTime(record.Observation.OccurredAt)
	record.Observation.Interval.Start = billing.CanonicalTime(record.Observation.Interval.Start)
	record.Observation.Interval.End = billing.CanonicalTime(record.Observation.Interval.End)
	record.ReceivedAt = billing.CanonicalTime(record.ReceivedAt)
	return record
}

func sameRating(a, b Result) bool {
	if a.RuleVersion != b.RuleVersion || a.Kind != b.Kind || a.Target != b.Target || a.Rounding != b.Rounding || a.ExactAmount != b.ExactAmount || a.RoundedAmount != b.RoundedAmount || !sameMoney(a.Money, b.Money) || !sameCredits(a.Credits, b.Credits) {
		return false
	}
	return a.Evidence.ActionCount == b.Evidence.ActionCount && a.Evidence.Waived == b.Evidence.Waived && a.Evidence.Adjustment == b.Evidence.Adjustment && a.Evidence.AdjustmentReason == b.Evidence.AdjustmentReason && slices.Equal(a.Evidence.Metrics, b.Evidence.Metrics)
}

func sameMoney(a, b *Money) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameCredits(a, b *Credits) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func Overlaps(a, b Record) bool {
	x, y := a.Observation, b.Observation
	if x.Account != y.Account || x.Source != y.Source || x.Scope != y.Scope {
		return false
	}
	var overlap bool
	switch {
	case x.Interval.Valid() && y.Interval.Valid():
		overlap = x.Interval.Start.Before(y.Interval.End) && y.Interval.Start.Before(x.Interval.End)
	case x.Interval.Valid():
		overlap = x.Interval.Contains(y.OccurredAt)
	case y.Interval.Valid():
		overlap = y.Interval.Contains(x.OccurredAt)
	}
	if !overlap {
		return false
	}
	if x.Input.ActionCount > 0 && y.Input.ActionCount > 0 {
		return true
	}
	for _, m := range x.Input.Metrics {
		for _, n := range y.Input.Metrics {
			if m.Name == n.Name {
				return true
			}
		}
	}
	return false
}
