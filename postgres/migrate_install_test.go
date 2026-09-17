package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestMigrateInstallFromZeroReachesUsableStore(t *testing.T) {
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BILLING_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("configured PostgreSQL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("billing_install_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	work := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = work.Close() })
	store := New(work)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account := billing.AccountID("install-zero")
	if err := store.CreateAccount(ctx, account, "install-zero-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store.Credits(), func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "install-grant", LotID: "install-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "install", SourceRef: "zero", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	bal, err := engine.Balance(ctx, account, "credits", "")
	if err != nil || bal.Available != 3 {
		t.Fatalf("install-from-zero balance=%+v err=%v", bal, err)
	}
}
