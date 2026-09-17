package pg

import (
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestUsageRootAndBoundDeferredCommitFailure(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "usage-commit", "usage-commit-tenant"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "usage-commit-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, FixedRate: "2"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	// The INSERT succeeds; PostgreSQL rejects only the COMMIT. This exercises
	// the real root transaction boundary rather than a callback-error stand-in.
	if _, err := db.ExecContext(ctx, `
CREATE FUNCTION reject_usage_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'deliberate usage commit failure'; END $$;
CREATE CONSTRAINT TRIGGER usage_commit_failure AFTER INSERT ON billing_usage
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_usage_commit();`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	observation := usage.Observation{Account: "usage-commit", ID: "root", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}
	service := usage.New(store.UsageRepository(), func() time.Time { return now })
	result, err := service.RateAndRecord(ctx, observation, config.Version)
	if err == nil || result.Observation.ID != "" || result.Fingerprint != "" {
		t.Fatalf("root commit failure returned result=%+v error=%v", result, err)
	}
	observation.ID = "bound"
	var provisional usage.Record
	var callbackReturned bool
	err = store.Atomic(ctx, observation.Account, func(session integration.Session) error {
		var err error
		provisional, err = usage.New(session.Usage(), func() time.Time { return now }).RateAndRecord(ctx, observation, config.Version)
		callbackReturned = err == nil
		return err
	})
	if err == nil || !callbackReturned || provisional.Observation.ID != "bound" {
		t.Fatalf("bound commit result=%+v callbackReturned=%v error=%v", provisional, callbackReturned, err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_usage WHERE account_id='usage-commit'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed commits published %d usage rows: %v", rows, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER usage_commit_failure ON billing_usage`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"root", "bound"} {
		observation.ID = id
		if _, err := service.RateAndRecord(ctx, observation, config.Version); err != nil {
			t.Fatalf("retry after failed commit for %s: %v", id, err)
		}
	}
}
