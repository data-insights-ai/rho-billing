package pg

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestPostgresConcurrentNewUnitRegistrationOpposingOrders(t *testing.T) {
	store, db := testStore(t)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	var schema string
	if err := db.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// testStore normally uses one connection. Prewarm every connection used
	// by this concurrent test with the per-test schema search path.
	connections := make([]*sql.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(t.Context(), `SET search_path TO `+schema); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	createTestAccounts(t, store)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	now := testTime
	type attempt struct {
		account billing.AccountID
		units   []string
	}
	attempts := []attempt{
		{account: "acct-a", units: []string{"unit-order-a", "unit-order-b"}},
		{account: "acct-b", units: []string{"unit-order-b", "unit-order-a"}},
	}
	start := make(chan struct{})
	results := make(chan error, len(attempts))
	var wg sync.WaitGroup
	for _, attempt := range attempts {
		attempt := attempt
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.Atomic(ctx, attempt.account, func(session integration.Session) error {
				engine := credit.New(session.Credits(), now)
				for index, code := range attempt.units {
					if _, err := engine.Grant(ctx, credit.GrantInput{
						Account: attempt.account, Operation: billing.OperationID(string(attempt.account) + "-unit-order-" + string(rune('a'+index))),
						LotID: string(attempt.account) + "-" + code, Unit: billing.Unit{Code: code, Scale: 1}, Amount: 1,
						Source: "unit-order", SourceRef: string(attempt.account) + "-" + code, ValidFrom: now(),
					}); err != nil {
						return err
					}
				}
				return nil
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("opposing-order unit transaction: %v", err)
		}
	}

	for _, attempt := range attempts {
		engine := credit.New(store, now)
		for _, code := range attempt.units {
			balance, err := engine.Balance(ctx, attempt.account, code, "")
			if err != nil || balance.Available != 1 {
				t.Fatalf("account %s unit %s balance = %#v, err %v", attempt.account, code, balance, err)
			}
		}
	}
}
