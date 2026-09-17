package pg

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

// TestLiveLotsRetiredRowScalability records query-plan evidence for the live
// lot read. The fixture inserts projections directly because this test measures
// the query over a fixed persisted state; it is not a ledger or financial
// throughput test.
func TestLiveLotsRetiredRowScalability(t *testing.T) {
	for _, retired := range []int{10, 1000, 100000} {
		t.Run(fmt.Sprintf("retired-%d", retired), func(t *testing.T) {
			store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
			ctx := t.Context()
			const account = "live-query-plan"
			if err := store.CreateAccount(ctx, account, account+"-subject"); err != nil {
				t.Fatal(err)
			}
			markProjectionUnready(t, db, account)
			if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
				t.Fatal(err)
			}
			insertLiveQueryFixture(t, db, account, retired)
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}

			for {
				done, err := store.BuildCreditBalanceProjection(ctx, account, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			balance, err := store.StoredCreditBalance(ctx, account, "credits", "")
			if err != nil {
				t.Fatal(err)
			}
			wantBalance := credit.Balance{Available: 10, Held: 20, Consumed: int64(retired)}
			if balance != wantBalance {
				t.Fatalf("stored balance=%+v, want %+v", balance, wantBalance)
			}

			if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
				lots, err := tx.LiveLots()
				if err != nil {
					return err
				}
				if len(lots) != 3 {
					return fmt.Errorf("live lots=%d, want 3: %+v", len(lots), lots)
				}
				want := []struct {
					id, state                string
					available, held, pending int64
				}{
					{id: "live-available", state: "available", available: 10},
					{id: "live-held", state: "held", held: 10},
					{id: "live-pending", state: "pending", held: 10, pending: 2},
				}
				for i, expected := range want {
					if lots[i].ID != expected.id || lots[i].Available != expected.available || lots[i].Held != expected.held || lots[i].PendingRevocation != expected.pending {
						return fmt.Errorf("live lot %d=%+v, want %s available=%d held=%d pending=%d", i, lots[i], expected.state, expected.available, expected.held, expected.pending)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := db.ExecContext(ctx, `ANALYZE billing_lots`); err != nil {
				t.Fatal(err)
			}
			plan := explainLiveLots(t, db, account)
			if retired >= 1000 && plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" {
				t.Fatalf("retired=%d live-lots plan=%s, want index scan", retired, plan.NodeType)
			}
			if retired >= 1000 && plan.IndexName != "billing_lots_credit_balance_live_idx" {
				t.Fatalf("retired=%d live-lots index=%q, want billing_lots_credit_balance_live_idx", retired, plan.IndexName)
			}
			if plan.ActualRows != 3 {
				t.Fatalf("retired=%d live-lots actual rows=%d, want 3", retired, plan.ActualRows)
			}
			if retired >= 1000 && plan.RowsRemovedByFilter != 0 {
				t.Fatalf("retired=%d live-lots rows removed by filter=%d, want 0", retired, plan.RowsRemovedByFilter)
			}
			t.Logf("retired=%d plan=%s index=%s rows=%d rows_removed=%d buffers(hit=%d read=%d) planning_ms=%.3f execution_ms=%.3f", retired, plan.NodeType, plan.IndexName, plan.ActualRows, plan.RowsRemovedByFilter, plan.SharedHitBlocks, plan.SharedReadBlocks, plan.PlanningTime, plan.ExecutionTime)
		})
	}
}

func TestEligibleLotsRetiredRowScalability(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	for _, retired := range []int{10, 1000, 100000} {
		t.Run(fmt.Sprintf("retired-%d", retired), func(t *testing.T) {
			store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
			ctx := t.Context()
			account := billing.AccountID(fmt.Sprintf("eligible-query-plan-%d", retired))
			if err := store.CreateAccount(ctx, account, string(account)+"-subject"); err != nil {
				t.Fatal(err)
			}
			markProjectionUnready(t, db, string(account))
			if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
				t.Fatal(err)
			}
			insertLiveQueryFixture(t, db, string(account), retired)
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			for {
				done, err := store.BuildCreditBalanceProjection(ctx, account, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
				lots, err := tx.EligibleLots("credits", "", at, 1001)
				if err != nil {
					return err
				}
				if len(lots) != 1 || lots[0].ID != "live-available" || lots[0].Available != 10 {
					return fmt.Errorf("eligible lots=%+v, want live-available", lots)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `ANALYZE billing_lots`); err != nil {
				t.Fatal(err)
			}
			plan := explainEligibleLots(t, db, string(account), at)
			if retired >= 1000 && plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" {
				t.Fatalf("retired=%d eligible-lots plan=%s, want index scan", retired, plan.NodeType)
			}
			if retired >= 1000 && plan.IndexName != "billing_lots_fefo_work_idx" {
				t.Fatalf("retired=%d eligible-lots index=%q, want billing_lots_fefo_work_idx", retired, plan.IndexName)
			}
			if plan.ActualRows != 1 {
				t.Fatalf("retired=%d eligible-lots actual rows=%d, want 1", retired, plan.ActualRows)
			}
			t.Logf("retired=%d plan=%s index=%s rows=%d buffers(hit=%d read=%d) execution_ms=%.3f", retired, plan.NodeType, plan.IndexName, plan.ActualRows, plan.SharedHitBlocks, plan.SharedReadBlocks, plan.ExecutionTime)
		})
	}
}

func TestStoredBalanceAtRetiredRowScalability(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	for _, retired := range []int{10, 1000, 100000} {
		t.Run(fmt.Sprintf("retired-%d", retired), func(t *testing.T) {
			store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
			ctx := t.Context()
			account := billing.AccountID(fmt.Sprintf("stored-balance-plan-%d", retired))
			if err := store.CreateAccount(ctx, account, string(account)+"-subject"); err != nil {
				t.Fatal(err)
			}
			markProjectionUnready(t, db, string(account))
			if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
				t.Fatal(err)
			}
			insertLiveQueryFixture(t, db, string(account), retired)
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			for {
				done, err := store.BuildCreditBalanceProjection(ctx, account, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			got, err := credit.New(store, func() time.Time { return at }).Balance(ctx, account, "credits", "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Available != 10 || got.Held != 20 || got.Consumed != int64(retired) {
				t.Fatalf("engine balance=%+v, want available=10 held=20 consumed=%d", got, retired)
			}
			if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
				stored, err := tx.StoredBalance("credits", "", at)
				if err != nil {
					return err
				}
				if stored != got {
					return fmt.Errorf("stored balance=%+v, want engine %+v", stored, got)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `ANALYZE billing_lots`); err != nil {
				t.Fatal(err)
			}
			plan := explainSpendableAvailable(t, db, string(account), at)
			if retired >= 1000 && plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" && plan.NodeType != "Aggregate" {
				t.Fatalf("retired=%d spendable plan=%s, want index or aggregate over index", retired, plan.NodeType)
			}
			if retired >= 1000 && plan.IndexName != "" && plan.IndexName != "billing_lots_fefo_work_idx" {
				t.Fatalf("retired=%d spendable index=%q, want billing_lots_fefo_work_idx", retired, plan.IndexName)
			}
			if retired >= 1000 && plan.IndexName == "" {
				t.Fatalf("retired=%d spendable plan=%s used no index", retired, plan.NodeType)
			}
			t.Logf("retired=%d plan=%s index=%s rows=%d buffers(hit=%d read=%d) execution_ms=%.3f", retired, plan.NodeType, plan.IndexName, plan.ActualRows, plan.SharedHitBlocks, plan.SharedReadBlocks, plan.ExecutionTime)
		})
	}
}

func explainSpendableAvailable(t *testing.T, db *sql.DB, account string, at time.Time) liveLotsPlan {
	t.Helper()
	const query = `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
		SELECT COALESCE(SUM(available),0) FROM billing_lots
		WHERE account_id = $1 AND unit_code=$2 AND available>0 AND valid_from <= $3
		  AND (expires_at IS NULL OR expires_at > $3) AND revoked_at IS NULL`
	var raw []byte
	if err := db.QueryRowContext(t.Context(), query, account, "credits", at).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var report []struct {
		Plan          liveLotsPlanNode `json:"Plan"`
		PlanningTime  float64          `json:"Planning Time"`
		ExecutionTime float64          `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("EXPLAIN report entries=%d, want 1", len(report))
	}
	plan := report[0].Plan
	index := plan
	for len(index.Plans) > 0 && index.IndexName == "" {
		index = index.Plans[0]
	}
	return liveLotsPlan{NodeType: plan.NodeType, IndexName: index.IndexName, ActualRows: index.ActualRows, RowsRemovedByFilter: index.RowsRemovedByFilter, SharedHitBlocks: index.SharedHitBlocks, SharedReadBlocks: index.SharedReadBlocks, PlanningTime: report[0].PlanningTime, ExecutionTime: report[0].ExecutionTime}
}

func explainEligibleLots(t *testing.T, db *sql.DB, account string, at time.Time) liveLotsPlan {
	t.Helper()
	const query = `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
		SELECT lot_id FROM billing_lots
		WHERE account_id = $1 AND unit_code=$2 AND available > 0 AND valid_from <= $3
		  AND (expires_at IS NULL OR expires_at > $3) AND revoked_at IS NULL
		ORDER BY expires_at ASC NULLS LAST, granted_at, lot_id
		LIMIT 1001`
	var raw []byte
	if err := db.QueryRowContext(t.Context(), query, account, "credits", at).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var report []struct {
		Plan          liveLotsPlanNode `json:"Plan"`
		PlanningTime  float64          `json:"Planning Time"`
		ExecutionTime float64          `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("EXPLAIN report entries=%d, want 1", len(report))
	}
	plan := report[0].Plan
	for len(plan.Plans) > 0 && plan.IndexName == "" {
		plan = plan.Plans[0]
	}
	return liveLotsPlan{NodeType: plan.NodeType, IndexName: plan.IndexName, ActualRows: plan.ActualRows, RowsRemovedByFilter: plan.RowsRemovedByFilter, SharedHitBlocks: plan.SharedHitBlocks, SharedReadBlocks: plan.SharedReadBlocks, PlanningTime: report[0].PlanningTime, ExecutionTime: report[0].ExecutionTime}
}

func insertLiveQueryFixture(t *testing.T, db *sql.DB, account string, retired int) {
	t.Helper()
	// Retired rows have no live quantity. Their consumed quantity keeps each
	// row valid under the lot conservation constraint without claiming a ledger
	// history for this query-only fixture.
	const query = `
	INSERT INTO billing_lots (
		account_id, lot_id, unit_code, unit_scale, scope, source, source_ref,
		valid_from, expires_at, granted_at, initial, available, held, consumed,
		expired, revoked, pending_revocation
	)
	SELECT $1, 'retired-' || lpad(i::text, 6, '0'), 'credits', 1, '', 'query-fixture',
	       'retired-' || i::text, $2::timestamptz, $3::timestamptz, $2::timestamptz, 1, 0, 0, 1, 0, 0, 0
	FROM generate_series(1, $4::int) AS rows(i)
	UNION ALL SELECT $1, 'live-available', 'credits', 1, '', 'query-fixture', 'live-available', $2::timestamptz, $3::timestamptz, $2::timestamptz, 10, 10, 0, 0, 0, 0, 0
	UNION ALL SELECT $1, 'live-held', 'credits', 1, '', 'query-fixture', 'live-held', $2::timestamptz, $3::timestamptz, $2::timestamptz, 10, 0, 10, 0, 0, 0, 0
	UNION ALL SELECT $1, 'live-pending', 'credits', 1, '', 'query-fixture', 'live-pending', $2::timestamptz, $3::timestamptz, $2::timestamptz, 10, 0, 10, 0, 0, 0, 2`
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(t.Context(), query, account, now, now.Add(time.Hour), retired); err != nil {
		t.Fatal(err)
	}
}

type liveLotsPlan struct {
	NodeType            string
	IndexName           string
	ActualRows          int
	RowsRemovedByFilter int
	SharedHitBlocks     int
	SharedReadBlocks    int
	PlanningTime        float64
	ExecutionTime       float64
}

func explainLiveLots(t *testing.T, db *sql.DB, account string) liveLotsPlan {
	t.Helper()
	const query = `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
		SELECT lot_id, unit_code, unit_scale, scope, source, source_ref,
		       valid_from, expires_at, granted_at, revoked_at, initial,
		       available, held, consumed, expired, revoked, pending_revocation
		FROM billing_lots WHERE account_id = $1
		  AND (available > 0 OR held > 0 OR pending_revocation > 0)
		ORDER BY lot_id`
	var raw []byte
	if err := db.QueryRowContext(t.Context(), query, account).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var report []struct {
		Plan          liveLotsPlanNode `json:"Plan"`
		PlanningTime  float64          `json:"Planning Time"`
		ExecutionTime float64          `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("EXPLAIN report entries=%d, want 1", len(report))
	}
	plan := report[0].Plan
	for len(plan.Plans) > 0 && plan.IndexName == "" {
		plan = plan.Plans[0]
	}
	return liveLotsPlan{NodeType: plan.NodeType, IndexName: plan.IndexName, ActualRows: plan.ActualRows, RowsRemovedByFilter: plan.RowsRemovedByFilter, SharedHitBlocks: plan.SharedHitBlocks, SharedReadBlocks: plan.SharedReadBlocks, PlanningTime: report[0].PlanningTime, ExecutionTime: report[0].ExecutionTime}
}

type liveLotsPlanNode struct {
	NodeType            string             `json:"Node Type"`
	IndexName           string             `json:"Index Name"`
	ActualRows          int                `json:"Actual Rows"`
	RowsRemovedByFilter int                `json:"Rows Removed by Filter"`
	SharedHitBlocks     int                `json:"Shared Hit Blocks"`
	SharedReadBlocks    int                `json:"Shared Read Blocks"`
	Plans               []liveLotsPlanNode `json:"Plans"`
}
