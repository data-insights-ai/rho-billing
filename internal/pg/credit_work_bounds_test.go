package pg

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func insertWorkLots(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, account string, now time.Time, count int, expires time.Time) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)
		ON CONFLICT (unit_code) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO billing_lots (account_id,lot_id,unit_code,unit_scale,scope,source,source_ref,
			valid_from,expires_at,granted_at,revoked_at,initial,available,held,consumed,expired,revoked,pending_revocation)
		SELECT $1, 'work-' || lpad(i::text,5,'0'), 'credits',1,'','work',
			'work-' || lpad(i::text,5,'0'),$2::timestamptz,$3::timestamptz,$2::timestamptz,
			NULL,1,1,0,0,0,0,0 FROM generate_series(1,$4) AS values(i)`, account, now.Add(-time.Hour), expires, count)
	if err != nil {
		t.Fatal(err)
	}
}

func readyWorkProjection(t *testing.T, store *Store, account billing.AccountID) {
	t.Helper()
	for {
		done, err := store.BuildCreditBalanceProjection(t.Context(), account, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
}

func TestPostgresCreditWorkBoundsExpiredLotsResume(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-work-expired")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	insertWorkLots(t, db, string(account), now, 1001, now.Add(-time.Minute))
	readyWorkProjection(t, store, account)
	engine := credit.New(store, func() time.Time { return now })
	first, err := engine.Sweep(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasMore {
		t.Fatalf("first sweep did not report remaining expired work: %+v", first)
	}
	second, err := engine.Sweep(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if second.HasMore {
		t.Fatalf("second sweep still reports work: %+v", second)
	}
	var expired int
	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		lots, err := tx.Lots()
		if err != nil {
			return err
		}
		for _, lot := range lots {
			if lot.Expired == 1 {
				expired++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if expired != 1001 {
		t.Fatalf("expired lots=%d, want 1001", expired)
	}
}

func TestPostgresCreditWorkBoundsEligibleBudget(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		account    string
		amount     int64
		wantBudget bool
	}{
		{account: "credit-work-small", amount: 1},
		{account: "credit-work-fragmented", amount: 1001, wantBudget: true},
	} {
		store, db := testStore(t)
		account := billing.AccountID(tc.account)
		if err := store.CreateAccount(ctx, account, tc.account); err != nil {
			t.Fatal(err)
		}
		insertWorkLots(t, db, tc.account, now, 1001, now.Add(time.Hour))
		readyWorkProjection(t, store, account)
		engine := credit.New(store, func() time.Time { return now })
		_, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "work-reserve", ReservationID: "work-reservation", Actor: "actor", Unit: "credits", Amount: tc.amount, Deadline: now.Add(time.Hour)})
		if tc.wantBudget {
			if !errors.Is(err, credit.ErrAllocationBudget) {
				t.Fatalf("reserve error=%v, want allocation budget", err)
			}
			if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
				rows, e := tx.Reservations()
				if e != nil {
					return e
				}
				if len(rows) != 0 {
					t.Fatalf("partial reservations=%+v", rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresCreditWorkBoundsBatchedFinishAfterExpiry(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-work-finish")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store, func() time.Time { return start })
	for i := range 2 {
		if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: billing.OperationID("work-grant-" + string(rune('a'+i))), LotID: "finish-lot-" + string(rune('a'+i)), Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5, Source: "work", SourceRef: "finish-" + string(rune('a'+i)), ValidFrom: start, ExpiresAt: start.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "work-hold", ReservationID: "work-hold", Actor: "actor", Unit: "credits", Amount: 10, Deadline: start.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	now := start.Add(time.Hour + time.Minute)
	engine = credit.New(store, func() time.Time { return now })
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "work-settle", ReservationID: "work-hold", Actual: 4, Evidence: credit.Evidence{UsageID: "work-usage", RatingVersion: "work-rating", Metrics: []credit.Metric{{Name: "units", Quantity: 4}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Balance(ctx, account, "credits", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Held != 0 || got.Consumed != 4 || got.Expired != 6 {
		t.Fatalf("balance=%+v", got)
	}
}

func TestPostgresCreditWorkBoundsTimeoutReleaseFeedsSameCommandReserve(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-work-timeout-reuse")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store, func() time.Time { return start })
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: "reuse-grant", LotID: "reuse-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "work", SourceRef: "reuse", ValidFrom: start, ExpiresAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reuse-first", ReservationID: "reuse-first", Actor: "actor", Unit: "credits", Amount: 10, Deadline: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	now := start.Add(2 * time.Minute)
	engine = credit.New(store, func() time.Time { return now })
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reuse-second", ReservationID: "reuse-second", Actor: "actor", Unit: "credits", Amount: 10, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatalf("same-command timeout release reserve: %v", err)
	}
}

func TestPostgresCreditWorkBoundsHistoricalLargeReservationRecovery(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-work-historical")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	insertWorkLots(t, db, string(account), now, 1001, now.Add(time.Hour))
	if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET available=0,held=1 WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (account_id,reservation_id,actor,unit,scope,created_at,deadline,state,authorized,consumed,limit_period_start,limit_period_end,evidence)
		VALUES ($1,'historical-hold','actor','credits','',$2::timestamptz,$2::timestamptz + interval '1 hour','held',1001,0,NULL,NULL,'{}'::jsonb)`, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservation_allocations(account_id,reservation_id,position,lot_id,amount)
		SELECT $1,'historical-hold',i-1,'work-' || lpad(i::text,5,'0'),1 FROM generate_series(1,1001) AS values(i)`, account); err != nil {
		t.Fatal(err)
	}
	readyWorkProjection(t, store, account)
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "historical-settle", ReservationID: "historical-hold", Actual: 0, Evidence: credit.Evidence{UsageID: "historical-usage", RatingVersion: "historical-rating", Metrics: []credit.Metric{{Name: "units", Quantity: 0}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Balance(ctx, account, "credits", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available != 1001 || got.Held != 0 {
		t.Fatalf("historical recovery balance=%+v", got)
	}
}
