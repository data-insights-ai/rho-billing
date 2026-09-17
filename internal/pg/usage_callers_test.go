package pg

import (
	"context"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func recordUsage(ctx context.Context, store *Store, record usage.Record) (usage.Record, error) {
	return usage.New(store.UsageRepository(), nil).Record(ctx, record)
}

func readUsage(ctx context.Context, store *Store, account billing.AccountID, id string) (usage.Record, error) {
	return usage.New(store.UsageRepository(), nil).Usage(ctx, account, id)
}

func readUsagePage(ctx context.Context, store *Store, account billing.AccountID, period billing.Period, after string, limit int) ([]usage.Record, error) {
	return usage.New(store.UsageRepository(), nil).UsagePage(ctx, account, period, after, limit)
}
