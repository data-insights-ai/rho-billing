package credit

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

func issueDueForTest(t *testing.T, service *AllowanceService, request issueRequest) (issueResult, error) {
	t.Helper()
	out, err := service.AdvanceCheckpoint(t.Context(), CheckpointRequest{Account: request.Account, ID: "test-worker", Revision: 0, Now: request.Now, Limit: request.Limit, MaxIssuances: request.MaxIssuances, MaxPeriods: request.MaxPeriods, Evidence: request.Evidence})
	return issueResult{Issuances: out.Issuances, Next: out.NextSchedule, NextPeriod: out.NextPeriod, NextDefinition: out.NextDefinition, HasMore: out.HasMore}, err
}

func TestServiceIssueDueIssuesAtExactPeriodBoundary(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	if err := f.service.RecordEligibility(t.Context(), fixtureEligibility(f, "payment-source")); err != nil {
		t.Fatal(err)
	}
	result, err := issueDueForTest(t, f.service, issueRequest{Account: f.schedule.Account, Now: utcBoundary(f), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issuances) != 1 || result.Issuances[0].Amount != 10 || result.Issuances[0].Status != IssuanceGranted {
		t.Fatalf("issuances = %+v, want one granted boundary issuance", result.Issuances)
	}
	if got := referenceCreditBalance(t, f); got != 10 {
		t.Fatalf("credit balance = %d, want 10", got)
	}
}

func TestServiceIssueDueReplayIsExactlyOnce(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	request := issueRequest{Account: f.schedule.Account, Now: utcBoundary(f), Limit: 1, Evidence: []EligibilityObservation{fixtureEligibility(f, "payment-source")}}
	first, err := issueDueForTest(t, f.service, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := issueDueForTest(t, f.service, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Issuances) != 1 || len(second.Issuances) != 0 {
		t.Fatalf("replay issuance counts = %d and %d, want 1 and 0", len(first.Issuances), len(second.Issuances))
	}
	got := referenceCreditBalance(t, f)
	if len(f.repo.issuances) != 1 || got != 10 {
		t.Fatalf("replay state = issuances %d balance %d, want 1 and 10", len(f.repo.issuances), got)
	}
}

func TestServiceIssueDueRejectsMemberAllowanceWithoutPublishingEvidence(t *testing.T) {
	f := newServiceFixture()
	plan := f.repo.plans[f.schedule.Assignment.PlanVersionID]
	plan.Allowances[0].Scope = catalog.AllowanceMember
	f.repo.plans[plan.ID] = plan
	putFixtureSchedule(t, f)
	result, err := issueDueForTest(t, f.service, issueRequest{Account: f.schedule.Account, Now: utcBoundary(f), Limit: 1, Evidence: []EligibilityObservation{fixtureEligibility(f, "payment-source")}})
	if !errors.Is(err, ErrMemberScope) || len(result.Issuances) != 0 {
		t.Fatalf("member allowance result=%+v error=%v", result, err)
	}
	if len(f.repo.eligibilityBySource) != 0 || len(f.repo.issuances) != 0 || referenceCreditBalance(t, f) != 0 {
		t.Fatal("unsupported member allowance published transactional effects")
	}
}

func TestServiceIssueDuePeriodCursorResumesInclusiveSchedule(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	if err := f.service.RecordEligibility(t.Context(), fixtureEligibility(f, "payment-source")); err != nil {
		t.Fatal(err)
	}
	request := issueRequest{Account: f.schedule.Account, Now: f.schedule.Assignment.Effective.End, Limit: 1, MaxPeriods: 1}
	first, err := issueDueForTest(t, f.service, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Issuances) != 1 || !first.HasMore || first.Next != f.schedule.ID || first.NextDefinition == "" || first.NextPeriod.IsZero() {
		t.Fatalf("first page = %+v, want one issuance and a period cursor", first)
	}
	request.After, request.AfterPeriod, request.AfterDefinition = first.Next, first.NextPeriod, first.NextDefinition
	second, err := issueDueForTest(t, f.service, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Issuances) != 1 || second.Issuances[0].Period.Start.Equal(first.Issuances[0].Period.Start) {
		t.Fatalf("second page = %+v, want the next period", second)
	}
}

func TestServiceIssueDueFailureRollsBackEvidenceIssuanceHighWaterAndCredits(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	sentinel := errors.New("commit failed")
	f.repo.failAfter = sentinel
	request := issueRequest{Account: f.schedule.Account, Now: utcBoundary(f), Limit: 1, Evidence: []EligibilityObservation{fixtureEligibility(f, "payment-source")}}
	if _, err := issueDueForTest(t, f.service, request); !errors.Is(err, sentinel) {
		t.Fatalf("failed issue error = %v, want sentinel", err)
	}
	if len(f.repo.eligibilityBySource) != 0 || len(f.repo.eligibilityByVersion) != 0 || len(f.repo.issuances) != 0 || len(f.repo.lineageByIssuance) != 0 {
		t.Fatalf("failed allowance transaction published state: evidence=%d/%d issuance=%d highwater=%d", len(f.repo.eligibilityBySource), len(f.repo.eligibilityByVersion), len(f.repo.issuances), len(f.repo.lineageByIssuance))
	}
	if got := referenceCreditBalance(t, f); got != 0 {
		t.Fatalf("failed transaction credit balance = %d, want 0", got)
	}

	result, err := issueDueForTest(t, f.service, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issuances) != 1 || len(f.repo.eligibilityBySource) != 1 || len(f.repo.issuances) != 1 || referenceCreditBalance(t, f) != 10 {
		t.Fatalf("retry state = result=%d evidence=%d issuance=%d balance=%d, want 1/1/1/10", len(result.Issuances), len(f.repo.eligibilityBySource), len(f.repo.issuances), referenceCreditBalance(t, f))
	}
}

func TestCheckpointMaxPeriodsIsGlobalAcrossSchedules(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	second := f.schedule
	second.ID = "a-service-schedule"
	second.SourceID = "second-service-event"
	if _, err := f.service.PutSchedule(t.Context(), second, 0); err != nil {
		t.Fatal(err)
	}
	firstEvidence := fixtureEligibility(f, "first-payment")
	secondEvidence := firstEvidence
	secondEvidence.ScheduleID = second.ID
	secondEvidence.SourceID = "second-payment"
	if err := f.service.RecordEligibility(t.Context(), firstEvidence); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecordEligibility(t.Context(), secondEvidence); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.AdvanceCheckpoint(t.Context(), CheckpointRequest{Account: f.schedule.Account, ID: "global-period-worker", Now: f.schedule.Assignment.Effective.End, Limit: 10, MaxPeriods: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issuances) != 1 || !result.HasMore {
		t.Fatalf("global period page=%+v, want one issuance and more work", result)
	}
	if result.NextSchedule != second.ID {
		t.Fatalf("global period cursor=%q, want %q", result.NextSchedule, second.ID)
	}
}

func utcBoundary(f serviceFixture) time.Time {
	return billing.CanonicalTime(f.schedule.Anchor)
}

func referenceCreditBalance(t *testing.T, f serviceFixture) int64 {
	t.Helper()
	var balance int64
	err := f.repo.credits.WithinAccount(t.Context(), f.schedule.Account, func(tx Tx) error {
		lots, err := tx.LiveLots()
		if err != nil {
			return err
		}
		for _, lot := range lots {
			if lot.Unit.Code == "credits" {
				balance += lot.Available
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

var _ Repository = testBoundCreditRepository{}
var _ AllowanceRepository = (*referenceRepository)(nil)
