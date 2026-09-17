package usage

import (
	"context"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"

	"github.com/data-insights-ai/rho-billing/subscription"
)

type Repository interface {
	WithinAccount(context.Context, billing.AccountID, func(Tx) error) error
}

type Tx interface {
	Record(context.Context, string) (Record, error)
	Rule(context.Context, string) (*Rule, error)
	Subscription(context.Context, billing.Reference) (subscription.Snapshot, error)
	Reservation(context.Context, string) (credit.Reservation, error)
	Candidates(context.Context, Observation, string, int) ([]Record, error)
	Save(context.Context, Record) error
	Page(context.Context, billing.Period, string, int) ([]Record, error)
	Cost(context.Context, string) (CostRecord, error)
	SaveCost(context.Context, CostRecord) error
}

type RuleResolver func(context.Context, string) (*Rule, error)

type Service struct {
	repo Repository
	now  func() time.Time
}

func New(repo Repository, now func() time.Time) *Service {
	if repo == nil {
		panic("usage: nil repository")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: now}
}

func (s *Service) RateAndRecord(ctx context.Context, o Observation, ruleVersion string) (Record, error) {
	if o.Funding == Prepaid || !billing.ValidID(ruleVersion) {
		return Record{}, billing.ErrInvalid
	}
	var out Record
	err := s.repo.WithinAccount(ctx, o.Account, func(tx Tx) error {
		rule, err := tx.Rule(ctx, ruleVersion)
		if err != nil {
			return err
		}
		if rule == nil || rule.Version() != ruleVersion {
			return billing.ErrConflict
		}
		prepared, err := Prepare(o, rule, billing.CanonicalTime(s.now()))
		if err != nil {
			return err
		}
		out, err = s.record(ctx, tx, prepared, rule, true)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return Copy(out), nil
}

func (s *Service) Record(ctx context.Context, in Record) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var out Record
	err := s.repo.WithinAccount(ctx, in.Observation.Account, func(tx Tx) error {
		var err error
		out, err = s.record(ctx, tx, in, nil, false)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return Copy(out), nil
}

func (s *Service) record(ctx context.Context, tx Tx, in Record, authoritative *Rule, prepared bool) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	in = canonicalRecord(in)
	if err := ValidateStoredRecord(in); err != nil {
		return Record{}, err
	}
	if in.Fingerprint == "" || in.Fingerprint != Identity(in) {
		return Record{}, billing.ErrInvalid
	}
	rule := authoritative
	if rule == nil {
		var err error
		rule, err = tx.Rule(ctx, in.Rating.RuleVersion)
		if err != nil {
			return Record{}, err
		}
	}
	if rule == nil {
		return Record{}, billing.ErrInvalid
	}
	if rule.Version() != in.Rating.RuleVersion {
		return Record{}, billing.ErrConflict
	}
	if !prepared {
		verified, err := Prepare(in.Observation, rule, in.ReceivedAt)
		if err != nil {
			return Record{}, err
		}
		if verified.Fingerprint != in.Fingerprint || !sameRating(verified.Rating, in.Rating) {
			return Record{}, billing.ErrConflict
		}
	}
	old, err := tx.Record(ctx, in.Observation.ID)
	if err == nil {
		if old.Observation.ID != in.Observation.ID || validateRead(in.Observation.Account, old) != nil || old.Fingerprint != in.Fingerprint {
			return Record{}, billing.ErrConflict
		}
		return Copy(old), nil
	}
	if !errors.Is(err, billing.ErrNotFound) {
		return Record{}, err
	}
	if err := validateScope(ctx, tx, in); err != nil {
		return Record{}, err
	}
	if err := validateReservation(ctx, tx, in); err != nil {
		return Record{}, err
	}
	if err := s.rejectOverlap(ctx, tx, in); err != nil {
		return Record{}, err
	}
	if err := tx.Save(ctx, in); err != nil {
		return Record{}, err
	}
	return in, nil
}

func (s *Service) Usage(ctx context.Context, account billing.AccountID, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	var out Record
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		var err error
		out, err = tx.Record(ctx, id)
		if err != nil {
			return err
		}
		if out.Observation.ID != id {
			return billing.ErrState
		}
		return validateRead(account, out)
	})
	if err != nil {
		return Record{}, err
	}
	return Copy(out), nil
}

func (s *Service) UsagePage(ctx context.Context, account billing.AccountID, period billing.Period, after string, limit int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !period.Valid() || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	var out []Record
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		rows, err := tx.Page(ctx, period, after, limit)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			return billing.ErrState
		}
		out = make([]Record, 0, len(rows))
		last := after
		for _, row := range rows {
			if row.Observation.ID <= last || !period.Contains(row.Observation.OccurredAt) {
				return billing.ErrState
			}
			if err := validateRead(account, row); err != nil {
				return err
			}
			out = append(out, Copy(row))
			last = row.Observation.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func validateRead(account billing.AccountID, row Record) error {
	if row.Observation.Account != account || ValidateStoredRecord(row) != nil || row.Fingerprint != Identity(row) {
		return billing.ErrState
	}
	return nil
}

func validateScope(ctx context.Context, tx Tx, in Record) error {
	scope := in.Observation.Scope
	if scope.AccountWide() {
		return nil
	}
	snapshot, err := tx.Subscription(ctx, scope.Subscription)
	if err != nil {
		return err
	}
	if subscription.Validate(snapshot) != nil || snapshot.Account != in.Observation.Account || snapshot.Ref != scope.Subscription {
		return billing.ErrConflict
	}
	for _, item := range snapshot.Items {
		if item.ID == scope.ItemID {
			return nil
		}
	}
	return billing.ErrNotFound
}

func validateReservation(ctx context.Context, tx Tx, in Record) error {
	if in.Observation.Funding != Prepaid {
		return nil
	}
	r, err := tx.Reservation(ctx, in.Observation.ReservationID)
	if err != nil {
		return err
	}
	ev := r.Evidence
	if r.State != "settled" || r.Consumed != in.Rating.RoundedAmount || r.Unit != in.Rating.Target.CreditUnit || ev.UsageID != in.Observation.ID || ev.RatingVersion != in.Rating.RuleVersion || r.Actor != in.Observation.Actor || r.Scope != in.Observation.CreditScope {
		return billing.ErrConflict
	}
	if r.ID != in.Observation.ReservationID || !sameEvidenceMetrics(ev.Metrics, in.Rating.Evidence.Metrics, in.Rating.Evidence.ActionCount) {
		return billing.ErrConflict
	}
	return nil
}

func (s *Service) rejectOverlap(ctx context.Context, tx Tx, in Record) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := tx.Candidates(ctx, in.Observation, after, 1000)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if len(rows) > 1000 {
			return billing.ErrState
		}
		for _, row := range rows {
			if row.Observation.ID <= after || validateRead(in.Observation.Account, row) != nil {
				return billing.ErrState
			}
			if Overlaps(in, row) {
				return billing.ErrConflict
			}
			after = row.Observation.ID
		}
		if len(rows) < 1000 {
			return nil
		}
	}
}

func sameEvidenceMetrics(actual []credit.Metric, expected []MetricQuantity, actionCount int64) bool {
	if len(expected) == 0 {
		return len(actual) == 1 && actual[0] == (credit.Metric{Name: "actions", Quantity: actionCount})
	}
	if len(actual) != len(expected) {
		return false
	}
	for i, metric := range expected {
		if actual[i].Name != metric.Name || actual[i].Quantity != metric.Quantity {
			return false
		}
	}
	return true
}
