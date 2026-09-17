package pg

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

var errInjectedWrite = errors.New("injected persistence interruption")

type failingCreditRepository struct {
	credit.Repository
	stage string
}

func (r failingCreditRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.Tx) error) error {
	return r.Repository.WithinAccount(ctx, account, func(tx credit.Tx) error { return fn(failingCreditTx{Tx: tx, stage: r.stage}) })
}

type failingCreditTx struct {
	credit.Tx
	stage string
}

func (t failingCreditTx) PutLot(v credit.Lot) error {
	if err := t.Tx.PutLot(v); err != nil {
		return err
	}
	if t.stage == "lot" {
		return errInjectedWrite
	}
	return nil
}
func (t failingCreditTx) PutReservation(v credit.Reservation) error {
	if err := t.Tx.PutReservation(v); err != nil {
		return err
	}
	if t.stage == "reservation" {
		return errInjectedWrite
	}
	return nil
}
func (t failingCreditTx) AppendEntries(v []credit.Entry) error {
	if err := t.Tx.AppendEntries(v); err != nil {
		return err
	}
	if t.stage == "journal" {
		return errInjectedWrite
	}
	return nil
}
func (t failingCreditTx) PutOutcome(id billing.OperationID, v credit.Outcome) error {
	if err := t.Tx.PutOutcome(id, v); err != nil {
		return err
	}
	if t.stage == "outcome" {
		return errInjectedWrite
	}
	return nil
}

func TestSettlementInterruptionAtEveryPersistenceStage(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, stage := range []string{"lot", "reservation", "journal", "outcome"} {
			t.Run(backend+"/"+stage, func(t *testing.T) {
				var repo credit.Repository
				if backend == "memory" {
					repo = credit.NewMemoryRepository("acct-a")
				} else {
					store, _ := testStore(t)
					createTestAccounts(t, store)
					repo = store
				}
				ctx := t.Context()
				e := credit.New(repo, testTime)
				if _, err := e.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "paid", ValidFrom: testTime()}); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: testTime().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				var before []credit.Entry
				if err := repo.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error { var err error; before, err = tx.History(0, 100); return err }); err != nil {
					t.Fatal(err)
				}
				in := credit.SettleInput{Account: "acct-a", Operation: "settle", ReservationID: "hold", Actual: 3, Evidence: credit.Evidence{UsageID: "usage", RatingVersion: "rule", Metrics: []credit.Metric{{Name: "actions", Quantity: 3}}}}
				if _, err := credit.New(failingCreditRepository{Repository: repo, stage: stage}, testTime).Settle(ctx, in); !errors.Is(err, errInjectedWrite) || credit.IsRejection(err) {
					t.Fatalf("persistence failure misclassified: %v", err)
				}
				if err := repo.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
					after, err := tx.History(0, 100)
					if err != nil {
						return err
					}
					if !slices.Equal(before, after) {
						t.Fatal("partial journal committed")
					}
					if _, exists, err := tx.Outcome("settle"); err != nil || exists {
						t.Fatalf("partial outcome persisted %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				balance, err := e.Balance(ctx, "acct-a", "credits", "")
				if err != nil || balance.Available != 2 || balance.Held != 8 || balance.Consumed != 0 {
					t.Fatalf("partial balance committed %+v %v", balance, err)
				}
				if _, err := e.VerifyLedger(ctx, "acct-a"); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Settle(ctx, in); err != nil {
					t.Fatalf("interrupted operation cannot recover: %v", err)
				}
				if _, err := e.VerifyLedger(ctx, "acct-a"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
