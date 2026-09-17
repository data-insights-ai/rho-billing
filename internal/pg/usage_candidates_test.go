package pg

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestUsageCandidatesPageWithoutLosingLaterOverlap(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "candidate-account", "candidate-tenant"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "candidate-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, Weights: map[string]string{"other": "1", "target": "1"}}
	rule, err := usage.NewRule(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	const count = 1005
	// This fixture measures the persistence candidate page. Individual events
	// can share an instant; only the last has the aggregate's target metric.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range count {
		metric := "other"
		if i == count-1 {
			metric = "target"
		}
		record, err := usage.Prepare(usage.Observation{Account: "candidate-account", ID: fmt.Sprintf("event-%04d", i), Source: "meter", OccurredAt: start, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: metric, Quantity: 1}}}}, rule, start)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_usage(account_id,usage_id,source,occurred_at,funding,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7)`, record.Observation.Account, record.Observation.ID, record.Observation.Source, start, string(record.Observation.Funding), record.Fingerprint, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	observation := usage.Observation{Account: "candidate-account", ID: "aggregate", Source: "meter", OccurredAt: start, Interval: billing.Period{Start: start, End: start.Add(time.Hour)}, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "target", Quantity: 1}}}}
	if err := store.UsageRepository().WithinAccount(ctx, observation.Account, func(tx usage.Tx) error {
		individual := observation
		individual.Interval = billing.Period{}
		unrelated, err := tx.Candidates(ctx, individual, "", 1000)
		if err != nil {
			return err
		}
		if len(unrelated) != 0 {
			return fmt.Errorf("individual event scanned %d unrelated individual events", len(unrelated))
		}
		var cursor string
		var seen, pages int
		for {
			page, err := tx.Candidates(ctx, observation, cursor, 1000)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			if len(page) > 1000 {
				return fmt.Errorf("candidate page exceeds bound: %d", len(page))
			}
			pages++
			for _, record := range page {
				if strings.Compare(record.Observation.ID, cursor) <= 0 {
					return fmt.Errorf("cursor did not advance: %q after %q", record.Observation.ID, cursor)
				}
				cursor = record.Observation.ID
				seen++
			}
		}
		if seen != count || pages != 2 {
			return fmt.Errorf("candidate scan seen=%d pages=%d", seen, pages)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return start })
	if _, err := service.RateAndRecord(ctx, observation, config.Version); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second-page overlap error=%v, want conflict", err)
	}
	observation.Source = "independent-meter"
	if _, err := service.RateAndRecord(ctx, observation, config.Version); err != nil {
		t.Fatalf("independent source rejected: %v", err)
	}
}
