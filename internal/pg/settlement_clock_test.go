package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresSettlementFinalizeUsesInjectedServiceClock(t *testing.T) {
	_, db := testStore(t)
	fixed := time.Date(2026, 9, 15, 14, 30, 0, 123456789, time.UTC)
	store := NewWithClock(db, func() time.Time { return fixed })
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-clock-default", "settlement-clock-default"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, "acct-clock-default", "usage-clock-default", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 2)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}

	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{
		Account: "acct-clock-default", Operation: "close-clock-default", BatchID: "batch-clock-default",
		Period: period, Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := batch.CreatedAt, billing.CanonicalTime(fixed); !got.Equal(want) {
		t.Fatalf("default CreatedAt = %s, want store clock %s", got, want)
	}

	if err := store.CreateAccount(ctx, "acct-clock-explicit", "settlement-clock-explicit"); err != nil {
		t.Fatal(err)
	}
	explicit := time.Date(2026, 9, 15, 15, 45, 0, 987654321, time.UTC)
	record = settlementTestRecord(t, store, "acct-clock-explicit", "usage-clock-explicit", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 3)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	batch, err = finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{
		Account: "acct-clock-explicit", Operation: "close-clock-explicit", BatchID: "batch-clock-explicit",
		Period: period, Currency: "USD", CreatedAt: explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := batch.CreatedAt, billing.CanonicalTime(explicit); !got.Equal(want) {
		t.Fatalf("explicit CreatedAt = %s, want input timestamp %s", got, want)
	}
}
