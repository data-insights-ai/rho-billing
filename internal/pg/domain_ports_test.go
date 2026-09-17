package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresDomainPortsShareOneConnection(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("domain-ports")
	if err := store.CreateAccount(ctx, account, "domain-ports-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store.Credits(), func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "ports-grant", LotID: "ports-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "test", SourceRef: "ports", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	bal, err := engine.Balance(ctx, account, "credits", "")
	if err != nil || bal.Available != 10 {
		t.Fatalf("credit port balance=%+v err=%v", bal, err)
	}
	purchases := purchase.New(store.Purchases(), func() time.Time { return now })
	if _, err := purchases.Quote(ctx, account, "missing-quote"); err == nil {
		t.Fatal("purchase port accepted a missing quote")
	}
	settlements := usage.NewSettlement(store.Settlements(), func() time.Time { return now })
	if _, err := settlements.Attempt(ctx, account, "missing-attempt"); err == nil {
		t.Fatal("settlement port accepted a missing attempt")
	}
	queue := store.Queue()
	msg := queueMessage(account, "ports-inbox", integration.Inbound, "payload")
	if err := queue.Receive(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, err := queue.Delivery(ctx, msg)
	if err != nil || got.Message.ID != msg.ID {
		t.Fatalf("queue port delivery=%+v err=%v", got, err)
	}
}

var (
	_ credit.Repository          = (*Store)(nil)
	_ integration.Repository     = (*Store)(nil)
	_ purchase.Repository        = (*Store)(nil).Purchases()
	_ usage.SettlementRepository = (*Store)(nil).Settlements()
)
