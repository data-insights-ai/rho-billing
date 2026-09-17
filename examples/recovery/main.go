// Command recovery demonstrates durable inbox fencing and outbox recovery.
// Everything runs against one isolated PostgreSQL schema and a deterministic
// in-process provider simulator; no provider SDK or network request is used.
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
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/usage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type auditOutput struct {
	Account                string `json:"account"`
	InboundID              string `json:"inbound_id"`
	ClaimedFence           int64  `json:"claimed_fence"`
	SuccessorFence         int64  `json:"successor_fence"`
	ClaimStateAfterRestart string `json:"claim_state_after_restart"`
	InboundFinalState      string `json:"inbound_final_state"`
	StaleWorkerError       string `json:"stale_worker_error"`
	StaleCallback          bool   `json:"stale_callback_ran"`
	SuccessorCallback      bool   `json:"successor_callback_ran"`
	ReplayCallback         bool   `json:"replay_callback_ran"`
	CreditsAvailable       int64  `json:"credits_available"`
	OutboxID               string `json:"outbox_id"`
	UnknownState           string `json:"outbox_unknown_state"`
	LookupStatus           string `json:"provider_lookup_status"`
	ResolvedState          string `json:"outbox_resolved_state"`
	ProviderReference      string `json:"provider_reference"`
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

	now := billing.CanonicalTime(time.Now())
	suffix := strconv.FormatInt(now.UnixNano(), 10)
	schema := "rho_recovery_example_" + suffix
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		return fmt.Errorf("create isolated schema: %w", err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO "`+schema+`"`); err != nil {
		return fmt.Errorf("set isolated schema: %w", err)
	}

	clock := func() time.Time { return now }
	store := postgres.NewWithClock(db, clock)
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	account := billing.AccountID("recovery-example-" + suffix)
	if err := store.CreateAccount(ctx, account, "recovery-example-subject-"+suffix); err != nil {
		return fmt.Errorf("create account: %w", err)
	}

	inbound := integration.Message{
		Account: account, ID: "inbound-" + suffix,
		Scope: billing.Scope{Provider: "local-provider", Merchant: "local-merchant", Environment: "sandbox"},
		Kind:  "usage.completed", Direction: integration.Inbound, OccurredAt: now, Payload: []byte(`{"usage":3}`),
	}
	if err := store.Queue().Receive(ctx, inbound); err != nil {
		return fmt.Errorf("receive inbound message: %w", err)
	}
	claimNow := billing.CanonicalTime(time.Now()).Add(time.Second)
	oldClaim, ok, err := store.Queue().Claim(ctx, integration.Inbound, "old-worker-"+suffix, claimNow, time.Minute)
	if err != nil {
		return fmt.Errorf("claim inbound message: %w", err)
	}
	if !ok {
		return errors.New("claim inbound message: no pending delivery")
	}

	// Demo-only crash simulation: make the old worker's lease expired without
	// sleeping, so the successor can fence it immediately.
	if _, err := db.ExecContext(ctx, `UPDATE billing_inbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE account_id=$1 AND message_id=$2`, string(account), inbound.ID); err != nil {
		return fmt.Errorf("expire demo inbox lease: %w", err)
	}
	restarted := postgres.NewWithClock(db, clock)
	claimedDelivery, err := restarted.Queue().Delivery(ctx, inbound)
	if err != nil {
		return fmt.Errorf("inspect retained inbox claim after restart: %w", err)
	}
	if claimedDelivery.State != "processing" || claimedDelivery.Fence != oldClaim.Fence {
		return fmt.Errorf("retained inbox claim=%+v", claimedDelivery)
	}
	nextClaim, ok, err := restarted.Queue().Claim(ctx, integration.Inbound, "successor-"+suffix, claimNow, time.Minute)
	if err != nil {
		return fmt.Errorf("claim inbound successor: %w", err)
	}
	if !ok {
		return errors.New("claim inbound successor: no expired delivery")
	}

	staleCallback := false
	staleErr := store.Queue().ProcessInbox(ctx, oldClaim, func(integration.Session) error {
		staleCallback = true
		return nil
	})
	if !errors.Is(staleErr, billing.ErrConflict) || staleCallback {
		return fmt.Errorf("stale worker was not fenced: err=%v callback=%t", staleErr, staleCallback)
	}

	successorCallback := false
	if err := restarted.Queue().ProcessInbox(ctx, nextClaim, func(scope integration.Session) error {
		successorCallback = true
		_, err := credit.New(scope.Credits(), clock).Grant(ctx, credit.GrantInput{
			Account: account, Operation: billing.OperationID("inbox-effect-" + suffix),
			LotID: "inbox-lot-" + suffix, Unit: billing.Unit{Code: "credits", Scale: 1},
			Amount: 3, Source: "recovery-example", SourceRef: inbound.ID, ValidFrom: now,
		})
		return err
	}); err != nil {
		return fmt.Errorf("process successor inbox: %w", err)
	}
	replayCallback := false
	replayErr := restarted.Queue().ProcessInbox(ctx, nextClaim, func(integration.Session) error {
		replayCallback = true
		return nil
	})
	if !errors.Is(replayErr, billing.ErrConflict) || replayCallback {
		return fmt.Errorf("inbox replay was not idempotent: err=%v callback=%t", replayErr, replayCallback)
	}
	finalInbound, err := postgres.NewWithClock(db, clock).Queue().Delivery(ctx, inbound)
	if err != nil {
		return fmt.Errorf("inspect processed inbox after restart: %w", err)
	}
	balance, err := credit.New(restarted.Credits(), clock).Balance(ctx, account, "credits", "")
	if err != nil {
		return fmt.Errorf("inspect successor effect: %w", err)
	}
	if finalInbound.State != "processed" || balance.Available != 3 {
		return fmt.Errorf("unexpected inbox result state=%s balance=%+v", finalInbound.State, balance)
	}

	outbound := integration.Message{
		Account: account, ID: "outbound-" + suffix,
		Scope: billing.Scope{Provider: "billingtest", Merchant: "local-merchant", Environment: "sandbox"},
		Kind:  "charge.submit", Direction: integration.Outbound, OccurredAt: now, Payload: []byte(`{"batch":"recovery-demo"}`),
	}
	if err := restarted.Atomic(ctx, account, func(scope integration.Session) error {
		return scope.Enqueue(ctx, outbound)
	}); err != nil {
		return fmt.Errorf("enqueue outbound command: %w", err)
	}
	outboundClaimNow := billing.CanonicalTime(time.Now()).Add(time.Second)
	outClaim, ok, err := restarted.Queue().Claim(ctx, integration.Outbound, "outbound-worker-"+suffix, outboundClaimNow, time.Minute)
	if err != nil {
		return fmt.Errorf("claim outbound command: %w", err)
	}
	if !ok {
		return errors.New("claim outbound command: no pending delivery")
	}
	send, err := restarted.Queue().BeginOutbox(ctx, outClaim, func(integration.Session) error { return nil })
	if err != nil || !send {
		return fmt.Errorf("begin outbound attempt: send=%v err=%v", send, err)
	}
	simulator := billingtest.NewSimulator(billingtest.AcceptThenTimeout)
	providerResult, submitErr := simulator.Submit(ctx, billingtest.SubmitRequest{
		BatchID: outbound.ID, AttemptID: "attempt-" + suffix, IdempotencyKey: "recovery-" + suffix,
		Currency: "USD", Amount: 3,
	})
	if !errors.Is(submitErr, billingtest.ErrAcceptedThenTimeout) || providerResult.Status != usage.SubmissionUnknown {
		return fmt.Errorf("simulated outbound timeout: result=%+v err=%v", providerResult, submitErr)
	}
	if err := restarted.Queue().FinishOutbox(ctx, outClaim, integration.OutboxResult{ObservationID: "timeout-" + suffix, State: integration.OutboxUnknown, Evidence: "provider response timed out"}, func(integration.Session) error { return nil }); err != nil {
		return fmt.Errorf("record unknown outbound result: %w", err)
	}
	unknownStore := postgres.NewWithClock(db, clock)
	unknownDelivery, err := unknownStore.Queue().Delivery(ctx, outbound)
	if err != nil {
		return fmt.Errorf("inspect unknown outbound result after restart: %w", err)
	}
	if unknownDelivery.State != "unknown" {
		return fmt.Errorf("outbound state=%s, want unknown before lookup", unknownDelivery.State)
	}
	lookup, err := simulator.Lookup(ctx, "recovery-"+suffix)
	if err != nil || lookup.Status != usage.SubmissionConfirmed || lookup.ProviderReference == "" {
		return fmt.Errorf("authoritative provider lookup: result=%+v err=%v", lookup, err)
	}
	if err := unknownStore.Queue().ResolveOutbox(ctx, outClaim, integration.OutboxResult{ObservationID: "lookup-" + suffix, ExpectedPrevious: "timeout-" + suffix, State: integration.OutboxCompleted, ProviderReference: lookup.ProviderReference, Evidence: "billingtest authoritative lookup"}, func(integration.Session) error { return nil }); err != nil {
		return fmt.Errorf("resolve unknown outbound result: %w", err)
	}
	resolvedStore := postgres.NewWithClock(db, clock)
	resolvedDelivery, err := resolvedStore.Queue().Delivery(ctx, outbound)
	if err != nil {
		return fmt.Errorf("inspect resolved outbound result: %w", err)
	}
	if resolvedDelivery.State != "completed" || resolvedDelivery.Message.ID != outbound.ID {
		return fmt.Errorf("unexpected resolved outbound result=%+v", resolvedDelivery)
	}

	out := auditOutput{
		Account: string(account), InboundID: inbound.ID, ClaimedFence: oldClaim.Fence,
		SuccessorFence: nextClaim.Fence, ClaimStateAfterRestart: string(claimedDelivery.State),
		InboundFinalState: string(finalInbound.State), StaleWorkerError: staleErr.Error(),
		StaleCallback: staleCallback, SuccessorCallback: successorCallback,
		ReplayCallback: replayCallback, CreditsAvailable: balance.Available,
		OutboxID: outbound.ID, UnknownState: string(unknownDelivery.State),
		LookupStatus: string(lookup.Status), ResolvedState: string(resolvedDelivery.State),
		ProviderReference: lookup.ProviderReference,
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("format audit output: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
