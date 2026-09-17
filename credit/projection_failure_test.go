package credit

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type failingProjectionRepository struct {
	Repository
	fail bool
}
type failingProjectionTx struct {
	Tx
	fail bool
}

func (r *failingProjectionRepository) WithinAccount(ctx context.Context, a billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, a, func(tx Tx) error { return fn(failingProjectionTx{Tx: tx, fail: r.fail}) })
}
func (tx failingProjectionTx) StoredBalance(unit, scope string, at time.Time) (Balance, error) {
	if tx.fail {
		return Balance{}, billing.ErrState
	}
	return tx.Tx.StoredBalance(unit, scope, at)
}

func TestProjectionFailureDoesNotRecordBusinessRejection(t *testing.T) {
	repo := &failingProjectionRepository{Repository: NewMemoryRepository("projection"), fail: true}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := New(repo, func() time.Time { return now })
	input := GrantInput{Account: "projection", Operation: "retry", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "test", SourceRef: "source", ValidFrom: now}
	if _, err := engine.Grant(t.Context(), input); !errors.Is(err, billing.ErrState) || IsRejection(err) {
		t.Fatalf("projection error=%v", err)
	}
	repo.fail = false
	result, err := engine.Grant(t.Context(), input)
	if err != nil || result.Balance.Available != 3 {
		t.Fatalf("retried result=%+v error=%v", result, err)
	}
}

func (tx failingProjectionTx) CommittedForLimit(actor, unit string, period billing.Period, at time.Time) (int64, error) {
	if tx.fail {
		return 0, billing.ErrState
	}
	return tx.Tx.CommittedForLimit(actor, unit, period, at)
}

func TestCapReadFailureDoesNotRecordBusinessRejection(t *testing.T) {
	repo := &failingProjectionRepository{Repository: NewMemoryRepository("projection"), fail: true}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := New(repo, func() time.Time { return now })
	input := LimitInput{Account: "projection", Operation: "limit-retry", Limit: Limit{Actor: "actor", Unit: "credits", Period: billing.Period{Start: now, End: now.Add(time.Hour)}, Amount: 10}}
	if _, err := engine.SetLimit(t.Context(), input); !errors.Is(err, billing.ErrState) || IsRejection(err) {
		t.Fatalf("cap read error=%v", err)
	}
	repo.fail = false
	if _, err := engine.SetLimit(t.Context(), input); err != nil {
		t.Fatalf("retry cap after read failure: %v", err)
	}
}
