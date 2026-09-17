package usage

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type CostState string

const (
	CostEstimated CostState = "estimated"
	CostActual    CostState = "actual"
	CostMissing   CostState = "missing"
)

type CostInput struct {
	Account        billing.AccountID
	ID             string
	UsageID        string
	Resource       string
	Model          string
	Quantity       int64
	RuleVersion    string
	Currency       string
	Amount         int64
	ExactAmount    string
	State          CostState
	CorrectsID     string
	MissingReason  string
	SourceCurrency string
	OccurredAt     time.Time
}

type CostRecord struct {
	Account        billing.AccountID
	ID             string
	UsageID        string
	Resource       string
	Model          string
	Quantity       int64
	RuleVersion    string
	Currency       string
	Amount         int64
	ExactAmount    string
	State          CostState
	CorrectsID     string
	MissingReason  string
	SourceCurrency string
	OccurredAt     time.Time
	RecordedAt     time.Time
	Fingerprint    string
}

func (s *Service) RecordCost(ctx context.Context, in CostInput) (CostRecord, error) {
	if err := ctx.Err(); err != nil {
		return CostRecord{}, err
	}
	prepared, err := prepareCost(in, billing.CanonicalTime(s.now()))
	if err != nil {
		return CostRecord{}, err
	}
	var out CostRecord
	err = s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		old, err := tx.Cost(ctx, prepared.ID)
		if err == nil {
			if old.ID != prepared.ID || CostIdentity(old) != prepared.Fingerprint || old.Fingerprint != prepared.Fingerprint {
				return billing.ErrConflict
			}
			out = CopyCost(old)
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if prepared.UsageID != "" {
			usageRecord, err := tx.Record(ctx, prepared.UsageID)
			if err != nil {
				return err
			}
			if usageRecord.Observation.Account != prepared.Account || usageRecord.Observation.ID != prepared.UsageID {
				return billing.ErrState
			}
		}
		if prepared.CorrectsID != "" {
			original, err := tx.Cost(ctx, prepared.CorrectsID)
			if err != nil {
				return err
			}
			if original.Account != prepared.Account || original.Resource != prepared.Resource || original.Model != prepared.Model || original.CorrectsID != "" {
				return billing.ErrConflict
			}
		}
		if err := tx.SaveCost(ctx, prepared); err != nil {
			return err
		}
		out = CopyCost(prepared)
		return nil
	})
	if err != nil {
		return CostRecord{}, err
	}
	return out, nil
}

func (s *Service) Cost(ctx context.Context, account billing.AccountID, id string) (CostRecord, error) {
	if err := ctx.Err(); err != nil {
		return CostRecord{}, err
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return CostRecord{}, billing.ErrInvalid
	}
	var out CostRecord
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		row, err := tx.Cost(ctx, id)
		if err != nil {
			return err
		}
		if row.Account != account || row.ID != id || ValidateStoredCost(row) != nil || row.Fingerprint != CostIdentity(row) {
			return billing.ErrState
		}
		out = CopyCost(row)
		return nil
	})
	if err != nil {
		return CostRecord{}, err
	}
	return out, nil
}

func prepareCost(in CostInput, now time.Time) (CostRecord, error) {
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	now = billing.CanonicalTime(now)
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Resource) || !billing.ValidID(in.Model) || !billing.ValidID(in.RuleVersion) || !validEstimateCurrency(in.Currency) || in.OccurredAt.IsZero() || now.IsZero() {
		return CostRecord{}, billing.ErrInvalid
	}
	if in.UsageID != "" && !billing.ValidID(in.UsageID) {
		return CostRecord{}, billing.ErrInvalid
	}
	if in.CorrectsID != "" && (!billing.ValidID(in.CorrectsID) || in.CorrectsID == in.ID) {
		return CostRecord{}, billing.ErrInvalid
	}
	if in.Quantity < 0 || in.Amount < 0 {
		return CostRecord{}, billing.ErrInvalid
	}
	if in.SourceCurrency != "" && in.SourceCurrency != in.Currency {
		return CostRecord{}, billing.ErrInvalid
	}
	switch in.State {
	case CostEstimated, CostActual:
		if in.MissingReason != "" {
			return CostRecord{}, billing.ErrInvalid
		}
		if err := validateCostExact(in.State, in.Amount, in.ExactAmount); err != nil {
			return CostRecord{}, err
		}
	case CostMissing:
		if in.MissingReason == "" || in.Amount != 0 {
			return CostRecord{}, billing.ErrInvalid
		}
		if in.ExactAmount != "" && in.ExactAmount != "0" {
			return CostRecord{}, billing.ErrInvalid
		}
		in.ExactAmount = "0"
	default:
		return CostRecord{}, billing.ErrInvalid
	}
	record := CostRecord{
		Account: in.Account, ID: in.ID, UsageID: in.UsageID, Resource: in.Resource, Model: in.Model,
		Quantity: in.Quantity, RuleVersion: in.RuleVersion, Currency: in.Currency, Amount: in.Amount,
		ExactAmount: in.ExactAmount, State: in.State, CorrectsID: in.CorrectsID, MissingReason: in.MissingReason,
		SourceCurrency: in.SourceCurrency, OccurredAt: in.OccurredAt, RecordedAt: now,
	}
	record.Fingerprint = CostIdentity(record)
	return record, nil
}

func validateCostExact(state CostState, amount int64, exact string) error {
	if exact == "" {
		return billing.ErrInvalid
	}
	rat, ok := new(big.Rat).SetString(exact)
	if !ok || rat.Sign() < 0 {
		return billing.ErrInvalid
	}
	if state == CostMissing {
		if amount != 0 || rat.Sign() != 0 {
			return billing.ErrInvalid
		}
		return nil
	}
	if !rat.IsInt() {
		return nil
	}
	if !rat.Num().IsInt64() {
		return billing.ErrOverflow
	}
	if rat.Num().Int64() != amount {
		return billing.ErrInvalid
	}
	return nil
}

// CostIdentity excludes RecordedAt so an exact replay does not mint a new identity when the clock moves.
func CostIdentity(r CostRecord) string {
	return identity.Fingerprint(
		string(r.Account), r.ID, r.UsageID, r.Resource, r.Model, strconv.FormatInt(r.Quantity, 10),
		r.RuleVersion, r.Currency, strconv.FormatInt(r.Amount, 10), r.ExactAmount, string(r.State),
		r.CorrectsID, r.MissingReason, r.SourceCurrency, identity.Instant(r.OccurredAt),
	)
}

func ValidateStoredCost(record CostRecord) error {
	in := CostInput{
		Account: record.Account, ID: record.ID, UsageID: record.UsageID, Resource: record.Resource,
		Model: record.Model, Quantity: record.Quantity, RuleVersion: record.RuleVersion, Currency: record.Currency,
		Amount: record.Amount, ExactAmount: record.ExactAmount, State: record.State, CorrectsID: record.CorrectsID,
		MissingReason: record.MissingReason, SourceCurrency: record.SourceCurrency, OccurredAt: record.OccurredAt,
	}
	prepared, err := prepareCost(in, record.RecordedAt)
	if err != nil {
		return err
	}
	if prepared.Fingerprint != record.Fingerprint {
		return billing.ErrInvalid
	}
	return nil
}

func CopyCost(r CostRecord) CostRecord { return r }
