package pg

import (
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"time"
)

// Baseline selection rules retained as test oracles for the indexed SQL path.
func scheduleStateAt(history []credit.Schedule, _ credit.Schedule, at time.Time) credit.Schedule {
	var selected credit.Schedule
	for _, state := range history {
		if !state.StateEffectiveAt.After(at) && (selected.ID == "" || state.StateEffectiveAt.After(selected.StateEffectiveAt) || state.StateEffectiveAt.Equal(selected.StateEffectiveAt) && state.Revision > selected.Revision) {
			selected = state
		}
	}
	return selected
}

func scheduleStateForPeriod(history []credit.Schedule, schedule credit.Schedule, period billing.Period) credit.Schedule {
	selected := scheduleStateAt(history, schedule, period.Start)
	if selected.ID != "" {
		return selected
	}
	// A chained assignment can begin inside a rooted period. Select the first
	// state effective during that period, rather than using the latest schedule
	// state, so a later pause does not erase the earlier active interval.
	for _, state := range history {
		if state.StateEffectiveAt.Before(period.Start) || !state.StateEffectiveAt.Before(period.End) {
			continue
		}
		if selected.ID == "" || state.StateEffectiveAt.Before(selected.StateEffectiveAt) || state.StateEffectiveAt.Equal(selected.StateEffectiveAt) && state.Revision < selected.Revision {
			selected = state
		}
	}
	return selected
}

type storedEligibility struct {
	SourceID    string
	EffectiveAt time.Time
	ObservedAt  time.Time
	Eligibility credit.Eligibility
}

func eligibleForPeriod(events []storedEligibility, period billing.Period) (credit.Eligibility, string, bool) {
	for _, event := range events {
		if event.EffectiveAt.After(period.Start) {
			continue
		}
		// The newest effective status owns the boundary. If it is delinquent,
		// paused, or lacks coverage, older paid evidence must not resurrect it.
		return event.Eligibility, event.SourceID, event.Eligibility.Covers(period) == nil
	}
	return credit.Eligibility{}, "", false
}
