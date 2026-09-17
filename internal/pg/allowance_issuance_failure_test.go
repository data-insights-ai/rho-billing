package pg

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"testing"

	"github.com/data-insights-ai/rho-billing/credit"
)

func TestAllowanceIssuanceStorageFailureRollsBackGrant(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "issuance-failure")
	ctx := t.Context()
	// Fail after the credit engine has written its grant but before the issuance
	// audit record can be committed. This exercises the service/storage boundary.
	if _, err := f.store.db.ExecContext(ctx, `CREATE FUNCTION fail_allowance_issuance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected issuance failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `CREATE TRIGGER fail_allowance_issuance BEFORE INSERT ON billing_allowance_issuances FOR EACH ROW EXECUTE FUNCTION fail_allowance_issuance()`); err != nil {
		t.Fatal(err)
	}
	service := credit.NewAllowances(f.store.Allowances(), nil)
	request := allowanceIssueRequest{Account: f.account, Now: f.start, Limit: 10, Evidence: []credit.EligibilityObservation{acceptancePaid(f.account, f.schedule.ID, "paid", f.start, f.start, f.start, f.end)}}
	result, err := issueDueForTest(ctx, service, request)
	if err == nil {
		t.Fatal("expected injected storage failure")
	}
	if len(result.Issuances) != 0 || result.HasMore || result.Next != "" {
		t.Fatalf("failed transaction returned committed result: %+v", result)
	}
	for _, table := range []string{"billing_allowance_eligibility_events", "billing_allowance_eligibility_evidence", "billing_journal", "billing_lots", "billing_operations", "billing_allowance_issuances", "billing_allowance_high_water"} {
		var count int
		if err := f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, string(f.account)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d effects after failure", table, count)
		}
	}
	if _, err := f.store.db.ExecContext(ctx, `DROP TRIGGER fail_allowance_issuance ON billing_allowance_issuances`); err != nil {
		t.Fatal(err)
	}
	result, err = issueDueForTest(ctx, service, request)
	if err != nil || len(result.Issuances) != 1 || result.Issuances[0].Amount != 10 {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	result, err = issueDueForTest(ctx, service, request)
	if err != nil || len(result.Issuances) != 0 {
		t.Fatalf("replay result=%+v err=%v", result, err)
	}
}

func TestAllowanceEligibilityRejectsCorruptStoredFingerprint(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "eligibility-fingerprint")
	service := credit.NewAllowances(f.store.Allowances(), nil)
	in := acceptancePaid(f.account, f.schedule.ID, "paid", f.start, f.start, f.start, f.end)
	if err := service.RecordEligibility(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(t.Context(), `UPDATE billing_allowance_eligibility_events SET fingerprint='corrupt' WHERE account_id=$1`, string(f.account)); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordEligibility(t.Context(), in); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("source replay with corrupt fingerprint: %v", err)
	}
	in.SourceID = "same-version"
	if err := service.RecordEligibility(t.Context(), in); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("version tie with corrupt fingerprint: %v", err)
	}
	var count int
	if err := f.store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_allowance_eligibility_events WHERE account_id=$1`, string(f.account)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rejected input inserted %d events", count)
	}
}
