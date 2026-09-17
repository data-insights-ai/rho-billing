package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestProvenanceCancellationAndDelayedExpiry(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo credit.Repository
			if backend == "memory" {
				repo = credit.NewMemoryRepository("acct-a")
			} else {
				store, _ := testStore(t)
				createTestAccounts(t, store)
				repo = store
			}
			ctx := t.Context()
			now := testTime()
			expiry := now.Add(time.Hour)
			e := credit.New(repo, func() time.Time { return now })
			for _, source := range []string{"purchase", "promotion", "subscription_allowance", "adjustment"} {
				if _, err := e.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: billing.OperationID(source), LotID: source, Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5, Source: source, SourceRef: "cause-" + source, ValidFrom: now, ExpiresAt: expiry}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor-without-cap", Unit: "credits", Amount: 12, Deadline: expiry}); err != nil {
				t.Fatal(err)
			}
			cancel := credit.ReleaseInput{Account: "acct-a", Operation: "cancel", ReservationID: "hold", Reason: "user canceled before execution"}
			first, err := e.Release(ctx, cancel)
			if err != nil {
				t.Fatal(err)
			}
			if replay, err := e.Release(ctx, cancel); err != nil || replay != first {
				t.Fatalf("cancel replay %v %v", replay, err)
			}
			if _, err := e.Settle(ctx, credit.SettleInput{Account: "acct-a", Operation: "late", ReservationID: "hold", Actual: 1, Evidence: credit.Evidence{UsageID: "never-executed", RatingVersion: "v1", Metrics: []credit.Metric{{Name: "actions", Quantity: 1}}}}); !errors.Is(err, billing.ErrState) {
				t.Fatalf("canceled work settled: %v", err)
			}
			now = expiry.Add(72 * time.Hour)
			balance, err := e.Balance(ctx, "acct-a", "credits", "")
			if err != nil || balance.Expired != 20 || balance.Available != 0 || balance.Held != 0 {
				t.Fatalf("delayed expiry %+v %v", balance, err)
			}
			if err := repo.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
				lots, err := tx.Lots()
				if err != nil {
					return err
				}
				if len(lots) != 4 {
					t.Fatalf("provenance lots merged: %d", len(lots))
				}
				for _, lot := range lots {
					if lot.ID != lot.Source || lot.SourceRef != "cause-"+lot.Source || lot.Initial != 5 || lot.Expired != 5 || !lot.ExpiresAt.Equal(expiry) {
						t.Fatalf("provenance changed: %+v", lot)
					}
				}
				entries, err := tx.History(0, 100)
				if err != nil {
					return err
				}
				expiries := 0
				for _, entry := range entries {
					if entry.Kind == "expiry" {
						expiries++
						if !entry.EffectiveAt.Equal(expiry) || !entry.RecordedAt.Equal(now) {
							t.Fatalf("worker time replaced validity: %+v", entry)
						}
					}
				}
				if expiries != 4 {
					t.Fatalf("missing expiry history: %d", expiries)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.VerifyLedger(ctx, "acct-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
