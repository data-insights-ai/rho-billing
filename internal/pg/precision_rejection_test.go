package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPrepaidRejectionSurvivesOuterTransaction(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	now := testTime().Add(999 * time.Nanosecond)
	clock := func() time.Time { return now }
	config := usage.RuleConfig{Version: "rejection-rule", Kind: usage.KindFixed, Target: usage.Target{CreditUnit: "credits"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := store.Rating(ctx, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	engine := credit.New(store, clock)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "test", SourceRef: "source", ValidFrom: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "reservation", Actor: "actor", Unit: "credits", Amount: 2, Deadline: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	o := usage.Observation{Account: "acct-a", ID: "usage", Source: "job", Actor: "actor", OccurredAt: now, Funding: usage.Prepaid, ReservationID: "reservation", Input: usage.RateInput{ActionCount: 3}}
	_, _, err = integration.SettlePrepaid(ctx, store, o, rule, "settle", clock)
	if !credit.IsRejection(err) || !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("expected recorded overrun rejection: %v", err)
	}
	if _, err := engine.Extend(ctx, credit.ExtendInput{Account: "acct-a", Operation: "extend", ReservationID: "reservation", Additional: 2}); err != nil {
		t.Fatal(err)
	}
	_, _, err = integration.SettlePrepaid(ctx, store, o, rule, "settle", clock)
	if !credit.IsRejection(err) || !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("retry changed terminal outcome: %v", err)
	}
	r, err := engine.Reservation(ctx, "acct-a", "reservation")
	if err != nil || r.State != "held" || r.Authorized != 4 {
		t.Fatalf("rejected retry mutated reservation: %+v %v", r, err)
	}
	result, record, err := integration.SettlePrepaid(ctx, store, o, rule, "settle-new", clock)
	if err != nil || result.Consumed != 3 {
		t.Fatalf("new operation could not settle: %+v %v", result, err)
	}
	if !record.Observation.OccurredAt.Equal(billing.CanonicalTime(now)) {
		t.Fatalf("noncanonical stored time %v", record.Observation.OccurredAt)
	}
}

func TestExpiredOutboxCompletionPersistsUnknown(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "fail"}[fail], func(t *testing.T) {
			store, db := queueStore(t)
			ctx := t.Context()
			if err := store.CreateAccount(ctx, "acct-expired", "subject-expired"); err != nil {
				t.Fatal(err)
			}
			msg := queueMessage("acct-expired", "command", integration.Outbound, "payload")
			if err := store.Atomic(ctx, msg.Account, func(s integration.Session) error { return s.Enqueue(ctx, msg) }); err != nil {
				t.Fatal(err)
			}
			claim, ok, err := store.Claim(ctx, integration.Outbound, "worker", time.Now(), time.Minute)
			if err != nil || !ok {
				t.Fatalf("claim %v %v", ok, err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE billing_outbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE account_id=$1`, string(msg.Account)); err != nil {
				t.Fatal(err)
			}
			if fail {
				err = store.Fail(ctx, claim, "timeout", time.Time{}, 3)
			} else {
				err = store.FinishOutbox(ctx, claim, integration.OutboxResult{ObservationID: "finish", State: integration.OutboxCompleted, ProviderReference: "provider-result", Evidence: "provider response"}, func(integration.Session) error { return nil })
			}
			if !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("expired worker accepted: %v", err)
			}
			delivery, err := store.Delivery(ctx, msg)
			if err != nil || delivery.State != "unknown" {
				t.Fatalf("expiry was rolled back: %+v %v", delivery, err)
			}
		})
	}
}
