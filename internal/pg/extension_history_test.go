package pg

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestExtensionVersusSettlementIsSerializable(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var first, second credit.Repository
			if backend == "memory" {
				first = credit.NewMemoryRepository("acct-a")
				second = first
			} else {
				store, db := testStore(t)
				createTestAccounts(t, store)
				first, second = store, New(secondQueueDB(t, db))
			}
			ctx := t.Context()
			e := credit.New(first, testTime)
			if _, err := e.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "test", SourceRef: "grant", ValidFrom: testTime()}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 6, Deadline: testTime().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			extend := credit.ExtendInput{Account: "acct-a", Operation: "extend", ReservationID: "hold", Additional: 4}
			settle := credit.SettleInput{Account: "acct-a", Operation: "settle", ReservationID: "hold", Actual: 6, Evidence: credit.Evidence{UsageID: "usage", RatingVersion: "rule", Metrics: []credit.Metric{{Name: "actions", Quantity: 6}}}}
			start := make(chan struct{})
			var extensionErr, settlementErr error
			var wg sync.WaitGroup
			wg.Go(func() { <-start; _, extensionErr = e.Extend(ctx, extend) })
			wg.Go(func() { <-start; _, settlementErr = credit.New(second, testTime).Settle(ctx, settle) })
			close(start)
			wg.Wait()
			if settlementErr != nil || (extensionErr != nil && !errors.Is(extensionErr, billing.ErrState)) {
				t.Fatalf("extend=%v settle=%v", extensionErr, settlementErr)
			}
			if _, err := e.Extend(ctx, extend); (extensionErr == nil && err != nil) || (extensionErr != nil && !errors.Is(err, billing.ErrState)) {
				t.Fatalf("extension outcome changed: %v -> %v", extensionErr, err)
			}
			if _, err := e.Settle(ctx, settle); err != nil {
				t.Fatal(err)
			}
			settle.Actual = 5
			if _, err := e.Settle(ctx, settle); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("changed completion accepted: %v", err)
			}
			balance, err := e.Balance(ctx, "acct-a", "credits", "")
			if err != nil || balance.Available != 4 || balance.Consumed != 6 || balance.Held != 0 {
				t.Fatalf("invalid serialized result %+v %v", balance, err)
			}
			if _, err := e.VerifyLedger(ctx, "acct-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJournalCursorSurvivesNewWrites(t *testing.T) {
	store, _ := testStore(t)
	createTestAccounts(t, store)
	ctx := t.Context()
	e := credit.New(store, testTime)
	grant := func(id string) {
		t.Helper()
		if _, err := e.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: billing.OperationID(id), LotID: id, Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1, Source: "test", SourceRef: id, ValidFrom: testTime()}); err != nil {
			t.Fatal(err)
		}
	}
	grant("one")
	grant("two")
	page, err := store.History(ctx, "acct-a", 0, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("first page %v %v", page, err)
	}
	first := page[0]
	grant("three")
	rest, err := store.History(ctx, "acct-a", first.Sequence, 10)
	if err != nil || len(rest) != 2 || rest[0].Sequence != first.Sequence+1 || rest[1].Sequence != first.Sequence+2 {
		t.Fatalf("unstable continuation %v %v", rest, err)
	}
	again, err := store.History(ctx, "acct-a", 0, 1)
	if err != nil || len(again) != 1 || again[0] != first {
		t.Fatalf("history changed %v %v", again, err)
	}
	foreign, err := store.History(ctx, "acct-b", 0, 10)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign history leaked %v %v", foreign, err)
	}
}
