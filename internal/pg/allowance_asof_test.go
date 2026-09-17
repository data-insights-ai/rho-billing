package pg

import (
	"encoding/json"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

func allowanceAsOfFacts(t *testing.T, f allowanceAcceptanceFixture, periods []billing.Period) ([]credit.PeriodFact, error) {
	t.Helper()
	var facts []credit.PeriodFact
	err := f.store.Atomic(t.Context(), f.account, func(scope integration.Session) error {
		return scope.Allowances().WithinAccount(t.Context(), f.account, func(tx credit.AllowanceTx) error {
			var err error
			facts, err = tx.PeriodFacts(t.Context(), f.schedule.ID, periods)
			return err
		})
	})
	return facts, err
}

func TestPostgresAllowanceAsOfLookupHandlesHistoryBeyondOneThousandRows(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "asof-history")
	ctx := t.Context()
	var snapshot []byte
	if err := f.store.db.QueryRowContext(ctx, `SELECT snapshot FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2 AND revision=1`, f.account, f.schedule.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	for revision := int64(2); revision <= 1002; revision++ {
		if _, err := f.store.db.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, f.account, f.schedule.ID, revision, snapshot, "asof-history-"+time.Duration(revision).String(), f.start); err != nil {
			t.Fatal(err)
		}
	}
	periods := []billing.Period{{Start: f.start, End: f.start.AddDate(0, 1, 0)}}
	facts, err := allowanceAsOfFacts(t, f, periods)
	if err != nil {
		t.Fatalf("as-of lookup failed with 1002 history rows: %v", err)
	}
	if len(facts) != 1 || facts[0].State.State != credit.ScheduleActive {
		t.Fatalf("as-of facts = %#v, want one active fact", facts)
	}
}

func TestPostgresAllowanceAsOfLookupDoesNotResurrectEarlierPaidEvidence(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "asof-denial")
	ctx := t.Context()
	period := billing.Period{Start: f.start, End: f.start.AddDate(0, 1, 0)}
	paid := acceptancePaid(f.account, f.schedule.ID, "asof-paid", f.start.Add(-time.Hour), f.start.Add(-time.Hour), f.start, f.end)
	denied := credit.EligibilityObservation{Account: f.account, ScheduleID: f.schedule.ID, SourceID: "asof-denied", EffectiveAt: f.start, ObservedAt: f.start, Eligibility: credit.Eligibility{Status: credit.EligibilityDelinquent}}
	if err := credit.NewAllowances(f.store.Allowances(), nil).RecordEligibility(ctx, paid); err != nil {
		t.Fatal(err)
	}
	if err := credit.NewAllowances(f.store.Allowances(), nil).RecordEligibility(ctx, denied); err != nil {
		t.Fatal(err)
	}
	facts, err := allowanceAsOfFacts(t, f, []billing.Period{period})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Eligibility.Status != credit.EligibilityDelinquent || facts[0].SourceID != denied.SourceID {
		t.Fatalf("as-of eligibility = %#v, want authoritative denial from %q", facts, denied.SourceID)
	}
}

func TestPostgresAllowanceAsOfLookupSelectsFirstStateInsidePeriod(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "asof-midperiod")
	ctx := t.Context()
	period := billing.Period{Start: f.start, End: f.start.AddDate(0, 1, 0)}
	var snapshot []byte
	if err := f.store.db.QueryRowContext(ctx, `SELECT snapshot FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2 AND revision=1`, f.account, f.schedule.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var state credit.Schedule
	if err := json.Unmarshal(snapshot, &state); err != nil {
		t.Fatal(err)
	}
	// Move the initial active state strictly inside the period so this actually
	// exercises the fallback query, not the ordinary at-or-before lookup.
	state.StateEffectiveAt = f.start.Add(time.Hour)
	initialState := state
	initial, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `UPDATE billing_allowance_schedule_history SET snapshot=$3 WHERE account_id=$1 AND schedule_id=$2 AND revision=1`, f.account, f.schedule.ID, initial); err != nil {
		t.Fatal(err)
	}
	state.State = credit.SchedulePaused
	state.StateEffectiveAt = f.start.Add(time.Hour)
	state.Revision = 2
	paused, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, f.account, f.schedule.ID, 2, paused, "asof-midperiod-pause", f.start); err != nil {
		t.Fatal(err)
	}
	periods := []billing.Period{
		{Start: f.start, End: f.start.Add(time.Hour)},
		period,
		{Start: f.start.Add(time.Hour), End: period.End},
	}
	facts, err := allowanceAsOfFacts(t, f, periods)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != len(periods) {
		t.Fatalf("facts=%d periods=%d", len(facts), len(periods))
	}
	for i, p := range periods {
		want := scheduleStateForPeriod([]credit.Schedule{initialState, state}, f.schedule, p)
		got := facts[i].State
		if got.ID != want.ID || got.State != want.State || got.Revision != want.Revision || !got.StateEffectiveAt.Equal(want.StateEffectiveAt) {
			t.Fatalf("period %d state=%+v want %+v", i, got, want)
		}
	}

}

func TestPostgresAllowanceAsOfLookupUsesHighestRevisionForEqualStateEffectiveAt(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "asof-revision-tie")
	ctx := t.Context()
	period := billing.Period{Start: f.start, End: f.start.AddDate(0, 1, 0)}
	var snapshot []byte
	if err := f.store.db.QueryRowContext(ctx, `SELECT snapshot FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2 AND revision=1`, f.account, f.schedule.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var state credit.Schedule
	if err := json.Unmarshal(snapshot, &state); err != nil {
		t.Fatal(err)
	}
	state.StateEffectiveAt = f.start
	state.State = credit.SchedulePaused
	state.Revision = 2
	paused, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, f.account, f.schedule.ID, 2, paused, "asof-tie-low", f.start); err != nil {
		t.Fatal(err)
	}
	state.State = credit.ScheduleCanceled
	state.Revision = 3
	canceled, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, f.account, f.schedule.ID, 3, canceled, "asof-tie-high", f.start); err != nil {
		t.Fatal(err)
	}
	facts, err := allowanceAsOfFacts(t, f, []billing.Period{period})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].State.State != credit.ScheduleCanceled || facts[0].State.Revision != 3 {
		t.Fatalf("as-of tie state = %#v, want canceled revision 3", facts)
	}
}
