package integration

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

// SettlePrepaid never calls a payment provider.
func SettlePrepaid(ctx context.Context, uow UnitOfWork, o usage.Observation, rule *usage.Rule, operation billing.OperationID, now func() time.Time) (credit.Result, usage.Record, error) {
	if uow == nil || o.Funding != usage.Prepaid {
		return credit.Result{}, usage.Record{}, billing.ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	var result credit.Result
	var record usage.Record
	var rejection error
	err := uow.Atomic(ctx, o.Account, func(session Session) error {
		engine := credit.New(session.Credits(), now)
		reservation, err := engine.Reservation(ctx, o.Account, o.ReservationID)
		if err != nil {
			return err
		}
		if (o.Actor != "" && o.Actor != reservation.Actor) || (o.CreditScope != "" && o.CreditScope != reservation.Scope) {
			return billing.ErrConflict
		}
		// Missing attribution inherits the already-authorized reservation.
		// Project is application metadata; CreditScope is the enforced scope.
		o.Actor, o.CreditScope = reservation.Actor, reservation.Scope
		prepared, err := usage.Prepare(o, rule, now().UTC())
		if err != nil {
			return err
		}
		if prepared.Rating.Credits == nil {
			return billing.ErrInvalid
		}
		if reservation.Unit != prepared.Rating.Credits.Unit {
			return billing.ErrConflict
		}
		metrics := make([]credit.Metric, 0, len(prepared.Rating.Evidence.Metrics)+1)
		for _, m := range prepared.Rating.Evidence.Metrics {
			metrics = append(metrics, credit.Metric{Name: m.Name, Quantity: m.Quantity})
		}
		if len(metrics) == 0 {
			metrics = append(metrics, credit.Metric{Name: "actions", Quantity: prepared.Rating.Evidence.ActionCount})
		}
		result, err = engine.Settle(ctx, credit.SettleInput{Account: o.Account, Operation: operation, ReservationID: o.ReservationID, Actual: prepared.Rating.RoundedAmount, Evidence: credit.Evidence{UsageID: o.ID, RatingVersion: prepared.Rating.RuleVersion, Metrics: metrics}})
		if err != nil {
			if credit.IsRejection(err) {
				rejection = err
				return nil
			}
			return err
		}
		record, err = usage.New(session.Usage(), now).Record(ctx, prepared)
		return err
	})
	if err != nil {
		return credit.Result{}, usage.Record{}, err
	}
	if rejection != nil {
		return result, usage.Record{}, rejection
	}
	return result, record, nil
}
