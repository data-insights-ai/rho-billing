package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

// P1-07/P1-09: both repositories execute exactly the same boundary scenario.
func TestCreditClockConformance(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo credit.Repository
			if backend == "memory" {
				repo = credit.NewMemoryRepository("acct-a")
			} else {
				s, _ := testStore(t)
				createTestAccounts(t, s)
				repo = s
			}
			ctx := t.Context()
			now := testTime().Add(123456789 * time.Nanosecond)
			clock := func() time.Time { return now }
			engine := credit.New(repo, clock)
			expiry := now.Add(time.Hour)
			input := credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5, Source: "test", SourceRef: "test", ValidFrom: now, ExpiresAt: expiry}
			result, err := engine.Grant(ctx, input)
			if err != nil || result.Balance.Available != 5 {
				t.Fatalf("grant %+v %v", result, err)
			}
			input.ValidFrom = billing.CanonicalTime(input.ValidFrom)
			input.ExpiresAt = billing.CanonicalTime(input.ExpiresAt)
			replay, err := engine.Grant(ctx, input)
			if err != nil || replay != result {
				t.Fatalf("canonical replay %+v %v", replay, err)
			}
			now = billing.CanonicalTime(expiry).Add(-time.Nanosecond)
			balance, err := engine.Balance(ctx, "acct-a", "credits", "")
			if err != nil || balance.Available != 5 {
				t.Fatalf("before expiry %+v %v", balance, err)
			}
			now = billing.CanonicalTime(expiry)
			balance, err = engine.Balance(ctx, "acct-a", "credits", "")
			if err != nil || balance.Available != 0 || balance.Expired != 5 {
				t.Fatalf("at expiry %+v %v", balance, err)
			}
		})
	}
}

// P6-03: an aggregate and its constituent stream must never both be charged.
func TestUsageMixedStreamConformance(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, aggregateFirst := range []bool{false, true} {
			t.Run(backend+map[bool]string{true: "/aggregate-first", false: "/event-first"}[aggregateFirst], func(t *testing.T) {
				config := usage.RuleConfig{Version: "mixed-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, FixedRate: "1"}
				rule, err := usage.NewRule(config)
				if err != nil {
					t.Fatal(err)
				}
				var repo usage.Repository
				if backend == "memory" {
					repo = usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct-a"}, Rules: func(context.Context, string) (*usage.Rule, error) { return rule, nil }})
				} else {
					s, _ := testStore(t)
					createTestAccounts(t, s)
					if err := s.PublishRating(t.Context(), config); err != nil {
						t.Fatal(err)
					}
					repo = s.UsageRepository()
				}
				start := testTime()
				individual := usage.Observation{Account: "acct-a", ID: "event", Source: "stream", OccurredAt: start.Add(time.Minute), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}
				aggregate := individual
				aggregate.ID = "aggregate"
				aggregate.Interval = billing.Period{Start: start, End: start.Add(time.Hour)}
				aggregate.Input.ActionCount = 20
				first, second := individual, aggregate
				if aggregateFirst {
					first, second = second, first
				}
				a, err := usage.Prepare(first, rule, start)
				if err != nil {
					t.Fatal(err)
				}
				b, err := usage.Prepare(second, rule, start)
				if err != nil {
					t.Fatal(err)
				}
				service := usage.New(repo, nil)
				if _, err := service.Record(t.Context(), a); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Record(t.Context(), b); !errors.Is(err, billing.ErrConflict) {
					t.Fatalf("double measurement accepted: %v", err)
				}
				// Half-open end permits the first event of the following bucket.
				boundary := individual
				boundary.ID = "boundary"
				boundary.OccurredAt = aggregate.Interval.End
				next, err := usage.Prepare(boundary, rule, start)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.Record(t.Context(), next); err != nil {
					t.Fatalf("adjacent event rejected: %v", err)
				}
			})
		}
	}
}

func TestPartialRevocationConformance(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, settle := range []bool{false, true} {
			t.Run(backend+map[bool]string{true: "/consume", false: "/release"}[settle], func(t *testing.T) {
				var repo credit.Repository
				if backend == "memory" {
					repo = credit.NewMemoryRepository("acct-a")
				} else {
					s, _ := testStore(t)
					createTestAccounts(t, s)
					repo = s
				}
				ctx := t.Context()
				now := testTime()
				engine := credit.New(repo, func() time.Time { return now })
				if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "purchase", ValidFrom: now}); err != nil {
					t.Fatal(err)
				}
				if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				in := credit.RevokeAmountInput{Account: "acct-a", Operation: "partial-refund", LotID: "lot", Reason: "partial refund", Amount: 5}
				result, err := engine.RevokeAmount(ctx, in)
				if err != nil || result.Balance.Revoked != 2 || result.Exposure != 3 {
					t.Fatalf("refund %+v %v", result, err)
				}
				replay, err := engine.RevokeAmount(ctx, in)
				if err != nil || replay != result {
					t.Fatalf("replay %+v %v", replay, err)
				}
				if settle {
					_, err = engine.Settle(ctx, credit.SettleInput{Account: "acct-a", Operation: "finish", ReservationID: "hold", Actual: 4, Evidence: credit.Evidence{UsageID: "usage", RatingVersion: "v1", Metrics: []credit.Metric{{Name: "actions", Quantity: 4}}}})
				} else {
					_, err = engine.Release(ctx, credit.ReleaseInput{Account: "acct-a", Operation: "finish", ReservationID: "hold", Reason: "cancelled"})
				}
				if err != nil {
					t.Fatal(err)
				}
				b, err := engine.Balance(ctx, "acct-a", "credits", "")
				if err != nil {
					t.Fatal(err)
				}
				if settle {
					if b.Available != 4 || b.Consumed != 4 || b.Revoked != 2 {
						t.Fatalf("authorized consumption %+v", b)
					}
				} else {
					if b.Available != 5 || b.Revoked != 5 {
						t.Fatalf("refunded hold became spendable %+v", b)
					}
				}
				entries, err := repo.History(ctx, "acct-a", 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var pending int64
				for _, entry := range entries {
					pending += entry.PendingRevocation
				}
				if pending != 0 {
					t.Fatalf("pending history did not clear: %d", pending)
				}
				audit, err := engine.VerifyLedger(ctx, "acct-a")
				if err != nil || audit.Lots != 1 {
					t.Fatalf("audit %+v %v", audit, err)
				}
				if err := repo.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
					lots, err := tx.Lots()
					if err != nil {
						return err
					}
					lot := lots[0]
					lot.Available--
					lot.Consumed++
					return tx.PutLot(lot)
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := engine.VerifyLedger(ctx, "acct-a"); !errors.Is(err, billing.ErrState) {
					t.Fatalf("undetected projection drift: %v", err)
				}
			})
		}
	}
}
