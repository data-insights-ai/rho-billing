// Command postpaid demonstrates a complete postpaid close and recovery flow.
// The provider call is a deterministic in-process simulator; no provider SDK
// or network request is used.
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
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/usage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type auditOutput struct {
	Provider       string `json:"provider"`
	Account        string `json:"account"`
	BatchID        string `json:"batch_id"`
	Currency       string `json:"currency"`
	Total          int64  `json:"total_minor_units"`
	LineCount      int64  `json:"line_count"`
	FinalState     string `json:"final_state"`
	FinalRevision  int64  `json:"final_revision"`
	AttemptID      string `json:"attempt_id"`
	UnknownState   string `json:"unknown_state"`
	UnknownRev     int64  `json:"unknown_revision"`
	ConfirmedState string `json:"confirmed_state"`
	ConfirmedRev   int64  `json:"confirmed_revision"`
	ProviderRef    string `json:"provider_reference"`
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
	schema := "rho_postpaid_example_" + suffix
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		return fmt.Errorf("create isolated schema: %w", err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO "`+schema+`"`); err != nil {
		return fmt.Errorf("set isolated schema: %w", err)
	}

	store := postgres.New(db)
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	operation := func(prefix string) billing.OperationID { return billing.OperationID(prefix + "-" + suffix) }
	account := billing.AccountID("postpaid-example-" + suffix)
	if err := store.CreateAccount(ctx, account, "postpaid-example-subject-"+suffix); err != nil {
		return fmt.Errorf("create account: %w", err)
	}

	ruleConfig := usage.RuleConfig{Version: "postpaid-example-v1", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundHalfUp, FixedRate: "125"}
	if err := store.Ratings().PublishRating(ctx, ruleConfig); err != nil {
		return fmt.Errorf("publish rating: %w", err)
	}
	rule, err := store.Ratings().Rating(ctx, ruleConfig.Version)
	if err != nil {
		return fmt.Errorf("load rating: %w", err)
	}
	period := usage.BillingPeriod{Start: now.Add(-time.Hour), End: now.Add(time.Hour), Cutoff: now}
	record, err := usage.Prepare(usage.Observation{Account: account, ID: "usage-" + suffix, Source: "postpaid-example-meter", OccurredAt: now.Add(-30 * time.Minute), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 3}}, rule, now)
	if err != nil {
		return fmt.Errorf("prepare usage: %w", err)
	}
	if _, err := usage.New(store.Usage(), nil).Record(ctx, record); err != nil {
		return fmt.Errorf("store usage: %w", err)
	}

	service := usage.NewSettlement(store.Settlements(), func() time.Time { return now })
	job, err := service.StartClose(ctx, usage.CloseInput{Account: account, Operation: operation("close"), BatchID: "batch-" + suffix, Period: period, Currency: "USD", CreatedAt: now})
	if err != nil {
		return fmt.Errorf("start usage close: %w", err)
	}
	// Each advance commits its cursor and staged lines together. A restarted
	// host reads the same job and resumes from its persisted revision.
	for job.State == usage.ClosePreparing {
		job, err = service.AdvanceClose(ctx, account, job.BatchID, job.Revision, 100)
		if err != nil {
			return fmt.Errorf("advance usage close: %w", err)
		}
	}
	if _, err := service.PublishClose(ctx, account, job.BatchID, job.Revision); err != nil {
		return fmt.Errorf("publish usage close: %w", err)
	}
	batch, err := service.BatchSummary(ctx, account, job.BatchID)
	if err != nil {
		return fmt.Errorf("read published batch: %w", err)
	}
	attempt, err := service.BeginSubmission(ctx, usage.BeginSubmissionInput{Account: account, Operation: operation("begin"), BatchID: batch.ID, AttemptID: "attempt-" + suffix, Provider: "billingtest", IdempotencyKey: "postpaid-" + suffix, Capability: usage.SettlementCapability{SupportsIdempotency: true, SupportsLookup: true}, CreatedAt: now})
	if err != nil {
		return fmt.Errorf("begin submission: %w", err)
	}

	simulator := billingtest.NewSimulator(billingtest.AcceptThenTimeout)
	_, submitErr := simulator.Submit(ctx, billingtest.SubmitRequest{BatchID: batch.ID, AttemptID: attempt.ID, IdempotencyKey: attempt.IdempotencyKey, Currency: batch.Currency, Amount: batch.Total})
	if !errors.Is(submitErr, billingtest.ErrAcceptedThenTimeout) {
		return fmt.Errorf("simulated submission: expected accepted timeout, got %v", submitErr)
	}
	unknown, err := service.CompleteSubmission(ctx, usage.CompleteSubmissionInput{Account: account, Operation: operation("complete-unknown"), BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: usage.SubmissionUnknown, Reason: "provider accepted request but the response timed out", CompletedAt: now})
	if err != nil {
		return fmt.Errorf("record unknown submission: %w", err)
	}

	lookup, err := simulator.Lookup(ctx, attempt.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("authoritative provider lookup: %w", err)
	}
	if lookup.Status != usage.SubmissionConfirmed || lookup.ProviderReference == "" {
		return fmt.Errorf("provider lookup did not confirm the accepted request: %+v", lookup)
	}
	confirmed, err := service.ReconcileUnknown(ctx, usage.ReconcileUnknownInput{Account: account, Operation: operation("reconcile"), BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: unknown.Revision, Status: usage.SubmissionConfirmed, ProviderReference: lookup.ProviderReference, Evidence: "billingtest authoritative lookup", ReconciledAt: now})
	if err != nil {
		return fmt.Errorf("reconcile unknown submission: %w", err)
	}

	out := auditOutput{Provider: "billingtest.AcceptThenTimeout", Account: string(account), BatchID: batch.ID, Currency: batch.Currency, Total: batch.Total, LineCount: batch.LineCount, FinalState: string(batch.State), FinalRevision: batch.Revision, AttemptID: attempt.ID, UnknownState: string(unknown.State), UnknownRev: unknown.Revision, ConfirmedState: string(confirmed.State), ConfirmedRev: confirmed.Revision, ProviderRef: lookup.ProviderReference}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("format audit output: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
