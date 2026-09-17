package pg

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresUsageServiceRejectsForgedRatingAndCorruptReads(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("usage-service-forged")
	if err := store.CreateAccount(ctx, account, "usage-service-forged-subject"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "usage-service-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "3"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	now := usageServiceTestTime()
	record, err := usage.Prepare(usage.Observation{Account: account, ID: "forged-usage", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	forged := record
	forged.Rating.RoundedAmount++
	if _, err := usage.New(store.UsageRepository(), func() time.Time { return now }).Record(ctx, forged); !errors.Is(err, billing.ErrInvalid) && !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("forged rating error=%v, want invalid or conflict", err)
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return now })
	if _, err := service.Record(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_usage SET record='{}'::jsonb WHERE account_id=$1 AND usage_id=$2`, account, record.Observation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Usage(ctx, account, record.Observation.ID); !errors.Is(err, billing.ErrState) {
		t.Fatalf("corrupt read error=%v, want state error", err)
	}
}

func TestPostgresUsageServiceBoundRollbackAndScopeValidation(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("usage-service-rollback")
	if err := store.CreateAccount(ctx, account, "usage-service-rollback-subject"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "usage-service-rollback-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	now := usageServiceTestTime()
	record, err := usage.Prepare(usage.Observation{Account: account, ID: "rollback-usage", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("rollback usage")
	if err := store.Atomic(ctx, account, func(session integration.Session) error {
		if _, err := usage.New(session.Usage(), func() time.Time { return now }).Record(ctx, record); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("bound rollback error=%v, want sentinel", err)
	}
	if _, err := usage.New(store.UsageRepository(), nil).Usage(ctx, account, record.Observation.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rolled back usage read error=%v, want not found", err)
	}

	scoped := record
	scoped.Observation.ID = "foreign-scope"
	scoped.Observation.Scope = usage.BillingScope{Subscription: providerReferenceForUsage(), ItemID: "item"}
	scoped.Fingerprint = usage.Identity(scoped)
	if _, err := usage.New(store.UsageRepository(), nil).Record(ctx, scoped); !errors.Is(err, billing.ErrNotFound) && !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("foreign scope error=%v, want account-scoped rejection", err)
	}
}

func TestPostgresPrepaidEvidenceMetricsAndRootRollback(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("usage-service-prepaid")
	if err := store.CreateAccount(ctx, account, "usage-service-prepaid-subject"); err != nil {
		t.Fatal(err)
	}
	now := usageServiceTestTime()
	nowFn := func() time.Time { return now }
	config := usage.RuleConfig{Version: "usage-service-prepaid-rule", Kind: usage.KindFixed, Target: usage.Target{CreditUnit: "credits"}, Rounding: usage.RoundDown, FixedRate: "2"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	engine := credit.New(store, nowFn)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: "prepaid-grant", LotID: "prepaid-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "test", SourceRef: "prepaid-source", ValidFrom: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "prepaid-reserve", ReservationID: "prepaid-reservation", Actor: "actor", Unit: "credits", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	observation := usage.Observation{Account: account, ID: "prepaid-metrics-usage", Source: "meter", Actor: "actor", OccurredAt: now, Funding: usage.Prepaid, ReservationID: "prepaid-reservation", Input: usage.RateInput{ActionCount: 1}}
	prepared, err := usage.Prepare(observation, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "prepaid-settle", ReservationID: observation.ReservationID, Actual: prepared.Rating.RoundedAmount, Evidence: credit.Evidence{UsageID: observation.ID, RatingVersion: config.Version, Metrics: []credit.Metric{{Name: "actions", Quantity: 1}}}}); err != nil {
		t.Fatal(err)
	}
	// Keep the amount, usage ID, and rating version valid while corrupting only
	// the durable measurement evidence. The usage service must reject it.
	if _, err := db.ExecContext(ctx, `UPDATE billing_reservations SET evidence=$1::jsonb WHERE account_id=$2 AND reservation_id=$3`, `{"UsageID":"prepaid-metrics-usage","RatingVersion":"usage-service-prepaid-rule","Metrics":[{"Name":"wrong-metric","Quantity":1}]}`, account, observation.ReservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.New(store.UsageRepository(), nowFn).Record(ctx, prepared); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("same amount with mismatched prepaid metrics error=%v, want conflict", err)
	}
	if _, err := usage.New(store.UsageRepository(), nil).Usage(ctx, account, observation.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rejected prepaid usage read error=%v, want not found", err)
	}

	// A failure after both domain writes must roll back the root transaction.
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "rollback-reserve", ReservationID: "rollback-reservation", Actor: "actor", Unit: "credits", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rollbackObservation := observation
	rollbackObservation.ID = "rollback-prepaid-usage"
	rollbackObservation.ReservationID = "rollback-reservation"
	rollbackPrepared, err := usage.Prepare(rollbackObservation, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("root callback failed after prepaid commit")
	if err := store.Atomic(ctx, account, func(session integration.Session) error {
		if _, err := credit.New(session.Credits(), nowFn).Settle(ctx, credit.SettleInput{Account: account, Operation: "rollback-settle", ReservationID: rollbackObservation.ReservationID, Actual: rollbackPrepared.Rating.RoundedAmount, Evidence: credit.Evidence{UsageID: rollbackObservation.ID, RatingVersion: config.Version, Metrics: []credit.Metric{{Name: "actions", Quantity: 1}}}}); err != nil {
			return err
		}
		if _, err := usage.New(session.Usage(), nowFn).Record(ctx, rollbackPrepared); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("root callback error=%v, want sentinel", err)
	}
	reservation, err := engine.Reservation(ctx, account, "rollback-reservation")
	if err != nil || reservation.State != "held" || reservation.Consumed != 0 {
		t.Fatalf("rolled-back reservation=%+v error=%v", reservation, err)
	}
	if _, err := usage.New(store.UsageRepository(), nil).Usage(ctx, account, rollbackObservation.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rolled-back prepaid usage read error=%v, want not found", err)
	}
}

func TestPostgresUsageServiceOverlapAndDuplicateConcurrency(t *testing.T) {
	store, db := testStore(t)
	second := New(secondQueueDB(t, db))
	ctx := t.Context()
	account := billing.AccountID("usage-service-concurrent")
	if err := store.CreateAccount(ctx, account, "usage-service-concurrent-subject"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "usage-service-concurrent-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	now := usageServiceTestTime()
	base := usage.Observation{Account: account, Source: "meter", OccurredAt: now, Interval: billing.Period{Start: now.Add(-time.Minute), End: now.Add(time.Minute)}, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}
	firstObservation := base
	firstObservation.ID = "concurrent-usage-a"
	secondObservation := base
	secondObservation.ID = "concurrent-usage-b"
	record, err := usage.Prepare(firstObservation, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := usage.Prepare(secondObservation, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() {
		_, err := usage.New(store.UsageRepository(), func() time.Time { return now }).Record(ctx, record)
		errs <- err
	})
	wg.Go(func() {
		_, err := usage.New(second.UsageRepository(), func() time.Time { return now }).Record(ctx, secondRecord)
		errs <- err
	})
	wg.Wait()
	close(errs)
	var success, conflict int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, billing.ErrConflict) {
			conflict++
		} else {
			t.Fatalf("concurrent overlapping usage error=%v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent overlap outcomes success=%d conflict=%d", success, conflict)
	}
	page, err := usage.New(store.UsageRepository(), nil).UsagePage(ctx, account, billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, "", 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("committed overlapping usage rows=%d error=%v, want one", len(page), err)
	}

	duplicateAccount := billing.AccountID("usage-service-duplicate")
	if err := store.CreateAccount(ctx, duplicateAccount, "usage-service-duplicate-subject"); err != nil {
		t.Fatal(err)
	}
	duplicateObservation := base
	duplicateObservation.Account = duplicateAccount
	duplicateObservation.ID = "duplicate-usage"
	duplicateRecord, err := usage.Prepare(duplicateObservation, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	duplicateErrors := make(chan error, 2)
	var duplicateWG sync.WaitGroup
	duplicateWG.Go(func() {
		<-start
		_, err := usage.New(store.UsageRepository(), func() time.Time { return now }).Record(ctx, duplicateRecord)
		duplicateErrors <- err
	})
	duplicateWG.Go(func() {
		<-start
		_, err := usage.New(second.UsageRepository(), func() time.Time { return now }).Record(ctx, duplicateRecord)
		duplicateErrors <- err
	})
	close(start)
	duplicateWG.Wait()
	close(duplicateErrors)
	for err := range duplicateErrors {
		if err != nil {
			t.Fatalf("concurrent identical usage error=%v", err)
		}
	}
	duplicatePage, err := usage.New(store.UsageRepository(), nil).UsagePage(ctx, duplicateAccount, billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, "", 10)
	if err != nil || len(duplicatePage) != 1 {
		t.Fatalf("committed duplicate usage rows=%d error=%v, want one", len(duplicatePage), err)
	}
}

func usageServiceTestTime() time.Time {
	return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
}

func providerReferenceForUsage() billing.Reference {
	return billing.Reference{Scope: billing.Scope{Provider: "foreign", Merchant: "merchant", Environment: "sandbox"}, ID: "foreign-subscription"}
}
