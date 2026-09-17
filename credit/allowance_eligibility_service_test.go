package credit

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestServiceRecordEligibilityCanonicalReplay(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	observation := fixtureEligibility(f, "payment-source")
	observation.EffectiveAt = observation.EffectiveAt.Add(789 * time.Nanosecond)
	observation.ObservedAt = observation.ObservedAt.Add(789 * time.Nanosecond)
	if err := f.service.RecordEligibility(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecordEligibility(t.Context(), observation); err != nil {
		t.Fatalf("canonical source replay = %v", err)
	}
	if len(f.repo.eligibilityBySource) != 1 || len(f.repo.eligibilityByVersion) != 1 {
		t.Fatalf("replay inserted duplicate evidence: source=%d version=%d", len(f.repo.eligibilityBySource), len(f.repo.eligibilityByVersion))
	}
	stored := f.repo.eligibilityBySource[eligibilitySourceKey{scheduleID: f.schedule.ID, sourceID: observation.SourceID}]
	if !stored.EffectiveAt.Equal(billing.CanonicalTime(observation.EffectiveAt)) || !stored.ObservedAt.Equal(billing.CanonicalTime(observation.ObservedAt)) {
		t.Fatalf("stored observation = %+v, want canonical timestamps", stored)
	}
}

func TestServiceRecordEligibilityRejectsChangedSourceContent(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	observation := fixtureEligibility(f, "payment-source")
	if err := f.service.RecordEligibility(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	changed := observation
	changed.Eligibility.Status = EligibilityTrial
	changed.Eligibility.Evidence = []EligibilityEvidence{{Kind: EvidenceTrial, Reference: "trial", Policy: "policy", Covered: observationCoverage(f)}}
	if err := f.service.RecordEligibility(t.Context(), changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed source content error = %v, want conflict", err)
	}
}

func TestServiceRecordEligibilityRejectsContradictoryVersionAcrossSources(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	first := fixtureEligibility(f, "payment-a")
	if err := f.service.RecordEligibility(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceID = "payment-b"
	second.Eligibility.Evidence = []EligibilityEvidence{{Kind: EvidencePayment, Reference: "different", Covered: observationCoverage(f)}}
	if err := f.service.RecordEligibility(t.Context(), second); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("contradictory version error = %v, want conflict", err)
	}
}

func TestServiceRecordEligibilityAcceptsExactVersionReplayAcrossSources(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	first := fixtureEligibility(f, "payment-a")
	if err := f.service.RecordEligibility(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceID = "payment-b"
	if err := f.service.RecordEligibility(t.Context(), second); err != nil {
		t.Fatalf("exact version replay = %v", err)
	}
	if len(f.repo.eligibilityBySource) != 2 || len(f.repo.eligibilityByVersion) != 1 {
		t.Fatalf("evidence indexes = source %d version %d, want 2 and 1", len(f.repo.eligibilityBySource), len(f.repo.eligibilityByVersion))
	}
}

func TestServiceRecordEligibilityRollsBackAfterTransactionFailure(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	sentinel := errors.New("commit failed")
	f.repo.failAfter = sentinel
	if err := f.service.RecordEligibility(t.Context(), fixtureEligibility(f, "payment-source")); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want commit failure", err)
	}
	if len(f.repo.eligibilityBySource) != 0 || len(f.repo.eligibilityByVersion) != 0 {
		t.Fatalf("failed transaction published evidence: source=%d version=%d", len(f.repo.eligibilityBySource), len(f.repo.eligibilityByVersion))
	}
}

func TestServiceRecordEligibilityNormalizesNilAndEmptyEvidence(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	first := fixtureEligibility(f, "canceled-source")
	first.Eligibility = Eligibility{Status: EligibilityCanceled}
	if err := f.service.RecordEligibility(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceID = "canceled-empty-source"
	second.Eligibility.Evidence = []EligibilityEvidence{}
	if err := f.service.RecordEligibility(t.Context(), second); err != nil {
		t.Fatalf("empty evidence normalization = %v", err)
	}
	if f.repo.eligibilityBySource[eligibilitySourceKey{scheduleID: f.schedule.ID, sourceID: first.SourceID}].Eligibility.Evidence != nil || f.repo.eligibilityBySource[eligibilitySourceKey{scheduleID: f.schedule.ID, sourceID: second.SourceID}].Eligibility.Evidence != nil {
		t.Fatal("normalized empty evidence should be nil")
	}
}

func TestServiceRecordEligibilityRejectsWrongAccount(t *testing.T) {
	f := newServiceFixture()
	putFixtureSchedule(t, f)
	observation := fixtureEligibility(f, "payment-source")
	observation.Account = "other-account"
	if err := f.service.RecordEligibility(t.Context(), observation); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("wrong account error = %v, want not found", err)
	}
}

func putFixtureSchedule(t *testing.T, f serviceFixture) {
	t.Helper()
	if _, err := f.service.PutSchedule(t.Context(), f.schedule, 0); err != nil {
		t.Fatal(err)
	}
}

func fixtureEligibility(f serviceFixture, source string) EligibilityObservation {
	return EligibilityObservation{
		Account: f.schedule.Account, ScheduleID: f.schedule.ID, SourceID: source,
		EffectiveAt: f.schedule.Anchor, ObservedAt: f.now,
		Eligibility: Eligibility{Status: EligibilityPaid, Evidence: []EligibilityEvidence{{Kind: EvidencePayment, Reference: source, Covered: observationCoverage(f)}}},
	}
}

func observationCoverage(f serviceFixture) billing.Period {
	return billing.Period{Start: f.schedule.Anchor, End: f.schedule.Assignment.Effective.End}
}
