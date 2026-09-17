// Command prepaid demonstrates monthly allowances backed by confirmed annual
// payment evidence, a purchased credit top-up, FEFO reservation, prepaid
// settlement, and a restart-safe allowance worker. It uses only local facts;
// no provider SDK or network request is made.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type auditOutput struct {
	Account             string `json:"account"`
	Subscription        string `json:"subscription"`
	AnnualPaymentEvent  string `json:"annual_payment_event"`
	TopUpGrantedCredits int64  `json:"topup_granted_credits"`
	InitialIssuances    int    `json:"initial_issuances"`
	SettledCredits      int64  `json:"settled_credits"`
	RestartIssuances    int    `json:"restart_issuances"`
	BoundaryIssuances   int    `json:"boundary_issuances"`
	BalanceAvailable    int64  `json:"balance_available"`
	BalanceExpired      int64  `json:"balance_expired"`
	LedgerEntries       int64  `json:"ledger_entries"`
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("BILLING_DATABASE_URL")
	if dsn == "" {
		return errors.New("BILLING_DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	schema := "rho_prepaid_example_" + suffix
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		return fmt.Errorf("create isolated schema: %w", err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO "`+schema+`"`); err != nil {
		return fmt.Errorf("set isolated schema: %w", err)
	}

	current := time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }
	store := postgres.NewWithClock(db, clock)
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	account := billing.AccountID("prepaid-example-" + suffix)
	if err := store.CreateAccount(ctx, account, "prepaid-example-subject-"+suffix); err != nil {
		return fmt.Errorf("create account: %w", err)
	}

	plan := catalog.PlanVersion{
		ID: "prepaid-example-plan-v1", PlanID: "prepaid-example-plan", Version: 1, PublishedAt: current,
		Allowances: []catalog.AllowanceDefinition{{
			ID: "monthly-credits", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 20,
			Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD",
		}},
	}
	if err := store.Catalog().PublishPlan(ctx, plan); err != nil {
		return fmt.Errorf("publish plan: %w", err)
	}

	ref := billing.Reference{Scope: billing.Scope{Provider: "local", Merchant: "example-merchant", Environment: "sandbox"}, ID: "subscription-" + suffix}
	assignment := catalog.PlanAssignment{ID: "assignment-" + suffix, PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: current, End: time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "subscription-event-" + suffix, OccurredAt: current}); err != nil {
		return fmt.Errorf("store subscription assignment: %w", err)
	}
	schedule := credit.Schedule{Account: account, ID: "allowance-schedule-" + suffix, Subscription: ref, Assignment: assignment, Anchor: current, State: credit.ScheduleActive, StateEffectiveAt: current, SourceID: "schedule-event-" + suffix}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		return fmt.Errorf("store allowance schedule: %w", err)
	}

	annualCoverage := billing.Period{Start: current, End: time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC)}
	annualEvent := "annual-payment-event-" + suffix
	eligibility := credit.EligibilityObservation{
		Account: account, ScheduleID: schedule.ID, SourceID: annualEvent, EffectiveAt: current, ObservedAt: current,
		Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "annual-invoice-" + suffix, Covered: annualCoverage}}},
	}

	purchaseService := purchase.New(store.Purchases(), clock)
	topUpScope := billing.Scope{Provider: "local", Merchant: "example-merchant", Environment: "sandbox"}
	topUpOffer, err := purchaseService.PublishOffer(ctx, purchase.Offer{Account: account, Revision: purchase.Revision{ID: "topup-offer-" + suffix, Version: 1}, Name: "Credit top-up", Effects: []purchase.Effect{{Key: "topup-credit", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 60}}}})
	if err != nil {
		return fmt.Errorf("publish top-up offer: %w", err)
	}
	topUpPrice, err := purchaseService.PublishPrice(ctx, purchase.Price{Account: account, Revision: purchase.Revision{ID: "topup-price-" + suffix, Version: 1}, Offer: topUpOffer.Revision, Currency: "USD", UnitAmount: 1000, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		return fmt.Errorf("publish top-up price: %w", err)
	}
	topUpQuote, err := purchaseService.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "topup-quote-" + suffix, ValidUntil: current.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "topup-line", Price: topUpPrice.Revision, Quantity: 1}}})
	if err != nil {
		return fmt.Errorf("create top-up quote: %w", err)
	}
	topUpIntent, err := purchaseService.CreateIntent(ctx, purchase.IntentInput{Account: account, ID: "topup-intent-" + suffix, Operation: "topup-operation-" + suffix, QuoteID: topUpQuote.ID, QuoteFingerprint: topUpQuote.Fingerprint(), Scope: topUpScope, Actor: "prepaid-example", Reason: "annual top-up", ExpiresAt: topUpQuote.ValidUntil})
	if err != nil {
		return fmt.Errorf("create top-up intent: %w", err)
	}
	confirmed, err := purchaseService.ApplyPayment(ctx, purchase.PaymentFact{Account: account, EventID: annualEvent, TransactionID: "annual-transaction-" + suffix, IntentID: topUpIntent.ID, Scope: topUpScope, Status: purchase.FactPaid, Currency: topUpQuote.Currency, Gross: topUpQuote.Amount, Lines: []purchase.PaidLine{{LineID: "topup-line", Gross: topUpQuote.Amount}}, OccurredAt: current, CollectedAt: current, Payload: []byte("local confirmed paid fact")})
	if err != nil {
		return fmt.Errorf("apply local paid fact: %w", err)
	}
	if !confirmed.Applied {
		return fmt.Errorf("confirmed payment was rejected: %+v", confirmed)
	}
	if _, err := purchaseService.Fulfill(ctx, account, topUpIntent.ID); err != nil {
		return fmt.Errorf("fulfill top-up: %w", err)
	}
	topUpEffects, err := purchaseService.Fulfillments(ctx, account, topUpIntent.ID)
	if err != nil {
		return fmt.Errorf("read top-up effects: %w", err)
	}
	if len(topUpEffects) != 1 || topUpEffects[0].Effect.Credit == nil || topUpEffects[0].State != purchase.FulfillmentComplete {
		return fmt.Errorf("top-up effect is not complete")
	}
	var topUpGrantedCredits int64
	if err := store.Credits().WithinAccount(ctx, account, func(tx credit.Tx) error {
		lot, found, err := tx.Lot(topUpEffects[0].ID)
		if err != nil {
			return err
		}
		if !found || lot.SourceRef != topUpEffects[0].ID || lot.Initial != 60 {
			return fmt.Errorf("top-up grant evidence mismatch")
		}
		topUpGrantedCredits = lot.Initial
		return nil
	}); err != nil {
		return fmt.Errorf("read top-up grant: %w", err)
	}

	first, err := credit.NewAllowances(store.Allowances(), clock).AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "monthly-allowances", Now: current, Limit: 10, MaxIssuances: 10, Evidence: []credit.EligibilityObservation{eligibility}})
	if err != nil {
		return fmt.Errorf("issue initial allowance: %w", err)
	}
	if len(first.Issuances) != 1 || first.Issuances[0].Amount != 20 {
		return fmt.Errorf("unexpected initial allowance result: %+v", first.Issuances)
	}

	ratingConfig := usage.RuleConfig{Version: "prepaid-example-rating-v1", Kind: usage.KindFixed, Target: usage.Target{CreditUnit: "credits"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.Ratings().PublishRating(ctx, ratingConfig); err != nil {
		return fmt.Errorf("publish prepaid rating: %w", err)
	}
	rule, err := store.Ratings().Rating(ctx, ratingConfig.Version)
	if err != nil {
		return fmt.Errorf("load prepaid rating: %w", err)
	}
	engine := credit.New(store.Credits(), clock)
	reservationID := "reservation-" + suffix
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: billing.OperationID("reserve-" + suffix), ReservationID: reservationID, Actor: "example-user", Unit: "credits", Scope: "AI_STANDARD", Amount: 12, Deadline: current.Add(time.Hour)}); err != nil {
		return fmt.Errorf("reserve credits FEFO: %w", err)
	}
	settled, _, err := integration.SettlePrepaid(ctx, store, usage.Observation{Account: account, ID: "usage-" + suffix, Source: "local-example-meter", Actor: "example-user", OccurredAt: current.Add(5 * time.Minute), Funding: usage.Prepaid, ReservationID: reservationID, Input: usage.RateInput{ActionCount: 7}}, rule, billing.OperationID("settle-"+suffix), clock)
	if err != nil {
		return fmt.Errorf("settle prepaid usage: %w", err)
	}
	if settled.Consumed != 7 {
		return fmt.Errorf("unexpected settled credits: %+v", settled)
	}
	if _, err := engine.VerifyLedger(ctx, account); err != nil {
		return fmt.Errorf("verify ledger after settlement: %w", err)
	}

	// The injected clock advances to the exact monthly boundary. The original
	// anchor includes the noon time, so the old lot expires and the next period
	// starts at noon on February 28.
	current = time.Date(2026, time.February, 28, 12, 0, 0, 0, time.UTC)
	restarted := postgres.NewWithClock(db, clock)
	if err := restarted.Migrate(ctx); err != nil {
		return fmt.Errorf("restart migration check: %w", err)
	}
	boundary, err := credit.NewAllowances(restarted.Allowances(), clock).AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "monthly-allowances", Now: current, Limit: 10, MaxIssuances: 10})
	if err != nil {
		return fmt.Errorf("issue boundary allowance after restart: %w", err)
	}
	if len(boundary.Issuances) != 1 || boundary.Issuances[0].Amount != 20 {
		return fmt.Errorf("unexpected boundary allowance result: %+v", boundary.Issuances)
	}
	replay, err := credit.NewAllowances(restarted.Allowances(), clock).AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "monthly-allowances", Now: current, Limit: 10, MaxIssuances: 10})
	if err != nil {
		return fmt.Errorf("replay allowance worker: %w", err)
	}
	if len(replay.Issuances) != 0 {
		return fmt.Errorf("restart replay minted duplicate grants: %+v", replay.Issuances)
	}
	audit, err := credit.New(restarted.Credits(), clock).VerifyLedger(ctx, account)
	if err != nil {
		return fmt.Errorf("verify ledger after restart: %w", err)
	}
	balance, err := credit.New(restarted.Credits(), clock).Balance(ctx, account, "credits", "AI_STANDARD")
	if err != nil {
		return fmt.Errorf("read final balance: %w", err)
	}
	out := auditOutput{Account: string(account), Subscription: ref.ID, AnnualPaymentEvent: annualEvent, TopUpGrantedCredits: topUpGrantedCredits, InitialIssuances: len(first.Issuances), SettledCredits: settled.Consumed, RestartIssuances: len(replay.Issuances), BoundaryIssuances: len(boundary.Issuances), BalanceAvailable: balance.Available, BalanceExpired: balance.Expired, LedgerEntries: audit.Entries}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("format audit output: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
