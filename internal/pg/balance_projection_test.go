package pg

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestCreditBalanceProjectionBackfillPagesAndUnseenLots(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "projection-pages", "projection-pages-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
		t.Fatal(err)
	}
	markProjectionUnready(t, db, "projection-pages")
	for i, id := range []string{"lot-01", "lot-02", "lot-03", "lot-04"} {
		insertProjectionLot(t, db, "projection-pages", id, "", int64(i+1))
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoredCreditBalance(ctx, "projection-pages", "credits", ""); !errors.Is(err, billing.ErrState) {
		t.Fatalf("unready projection read error = %v, want ErrState", err)
	}
	done, err := store.BuildCreditBalanceProjection(ctx, "projection-pages", 2)
	if err != nil || done {
		t.Fatalf("first build done=%v err=%v, want incomplete", done, err)
	}
	// A mutation at or before the cursor is maintained immediately. A later
	// unseen lot remains for the next keyset page.
	if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET available=10, initial=10 WHERE account_id='projection-pages' AND lot_id='lot-01'`); err != nil {
		t.Fatal(err)
	}
	insertProjectionLot(t, db, "projection-pages", "lot-99", "", 20)
	// A newly inserted key behind the cursor must be counted by the trigger,
	// while an update ahead of it must be counted only by the later page.
	insertProjectionLot(t, db, "projection-pages", "lot-00", "", 8)
	if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET available=6, initial=6 WHERE account_id='projection-pages' AND lot_id='lot-04'`); err != nil {
		t.Fatal(err)
	}
	done, err = store.BuildCreditBalanceProjection(ctx, "projection-pages", 2)
	if err != nil || done {
		t.Fatalf("second build done=%v err=%v, want incomplete", done, err)
	}
	done, err = store.BuildCreditBalanceProjection(ctx, "projection-pages", 2)
	if err != nil || !done {
		t.Fatalf("final build done=%v err=%v, want complete", done, err)
	}
	got, err := store.StoredCreditBalance(ctx, "projection-pages", "credits", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available != 49 {
		t.Fatalf("available=%d, want 49 from mutations on both sides of the cursor", got.Available)
	}
	// A repeated completed backfill must not add any page twice.
	if done, err := store.BuildCreditBalanceProjection(ctx, "projection-pages", 1); err != nil || !done {
		t.Fatalf("repeat completed build done=%v err=%v", done, err)
	}
	if repeated, err := store.StoredCreditBalance(ctx, "projection-pages", "credits", ""); err != nil || repeated != got {
		t.Fatalf("repeated balance=%+v err=%v, want %+v", repeated, err, got)
	}
}

func TestCreditBalanceProjectionScopeAndAccountIsolation(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	for _, account := range []string{"projection-a", "projection-b"} {
		if err := store.CreateAccount(ctx, billing.AccountID(account), account+"-subject"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"projection-a", "projection-b"} {
		markProjectionUnready(t, db, account)
	}
	insertProjectionLot(t, db, "projection-a", "global", "", 10)
	insertProjectionLot(t, db, "projection-a", "project", "project-a", 7)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := store.BuildCreditBalanceProjection(ctx, "projection-a", 100); err != nil || !done {
		t.Fatalf("account A build done=%v err=%v", done, err)
	}
	for scope, want := range map[string]int64{"": 17, "project-a": 17, "other": 10} {
		got, err := store.StoredCreditBalance(ctx, "projection-a", "credits", scope)
		if err != nil || got.Available != want {
			t.Fatalf("scope %q balance=%+v err=%v, want available %d", scope, got, err, want)
		}
	}
	if _, err := store.StoredCreditBalance(ctx, "projection-b", "credits", ""); !errors.Is(err, billing.ErrState) {
		t.Fatalf("unbuilt account read error=%v, want ErrState", err)
	}
}

func TestCreditBalanceProjectionRollbackAndFutureOverflow(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "projection-rollback", "projection-rollback-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
		t.Fatal(err)
	}
	markProjectionUnready(t, db, "projection-rollback")
	insertProjectionLot(t, db, "projection-rollback", "lot-01", "", 5)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := store.BuildCreditBalanceProjection(ctx, "projection-rollback", 10); err != nil || !done {
		t.Fatalf("build done=%v err=%v", done, err)
	}
	wantErr := errors.New("rollback")
	err := store.WithinAccount(ctx, "projection-rollback", func(tx credit.Tx) error {
		lot := testProjectionLot("lot-01", "", 5)
		lot.Available = 1
		lot.Initial = 5
		lot.Consumed = 4
		if err := tx.PutLot(lot); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("rollback error=%v", err)
	}
	got, err := store.StoredCreditBalance(ctx, "projection-rollback", "credits", "")
	if err != nil || got.Available != 5 || got.Consumed != 0 {
		t.Fatalf("rolled back balance=%+v err=%v", got, err)
	}

	store, db = testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	if err := store.CreateAccount(ctx, "projection-overflow", "projection-overflow-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
		t.Fatal(err)
	}
	markProjectionUnready(t, db, "projection-overflow")
	insertProjectionLot(t, db, "projection-overflow", "lot-01", "", math.MaxInt64)
	insertProjectionLot(t, db, "projection-overflow", "lot-02", "", math.MaxInt64)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := store.BuildCreditBalanceProjection(ctx, "projection-overflow", 10); err != nil || !done {
		t.Fatalf("overflow build done=%v err=%v", done, err)
	}
	if _, err := store.StoredCreditBalance(ctx, "projection-overflow", "credits", ""); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow read error=%v, want ErrOverflow", err)
	}
}

func insertProjectionLot(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, account, id, scope string, amount int64) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO billing_lots
		(account_id,lot_id,unit_code,unit_scale,scope,source,source_ref,valid_from,granted_at,
		 initial,available,held,consumed,expired,revoked,pending_revocation)
		VALUES ($1,$2,'credits',1,$3,'test',$2,$4,$4,$5,$5,0,0,0,0,0)`,
		account, id, scope, projectionTestTime(), amount)
	if err != nil {
		t.Fatal(err)
	}
}

func markProjectionUnready(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, account string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE billing_credit_balance_projection_state SET last_lot_id=NULL, ready=false WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
}

func testProjectionLot(id, scope string, amount int64) credit.Lot {
	now := projectionTestTime()
	return credit.Lot{ID: id, Unit: billing.Unit{Code: "credits", Scale: 1}, Scope: scope, Source: "test", SourceRef: id, ValidFrom: now, GrantedAt: now, Initial: amount, Available: amount}
}

func projectionTestTime() time.Time {
	return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
}
