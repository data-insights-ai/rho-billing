package pg

import (
	"context"
	"encoding/json"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func (s *session) allowancePeriodFacts(ctx context.Context, scheduleID string, periods []billing.Period) ([]credit.PeriodFact, error) {
	if len(periods) == 0 {
		return nil, nil
	}
	if len(periods) > 10000 {
		return nil, billing.ErrInvalid
	}
	starts, ends := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		if !p.Valid() {
			return nil, billing.ErrInvalid
		}
		starts[i], ends[i] = databaseTime(p.Start), databaseTime(p.End)
	}
	rows, err := s.tx.QueryContext(ctx, `
 SELECT COALESCE(prior.snapshot,initial.snapshot,'{}'::jsonb),
 COALESCE(e.eligibility,'{}'::jsonb),COALESCE(e.source_id,'')
 FROM unnest($3::timestamptz[],$4::timestamptz[]) WITH ORDINALITY p(start_at,end_at,position)
 LEFT JOIN LATERAL (
 SELECT snapshot FROM billing_allowance_schedule_history
 WHERE account_id=$1 AND schedule_id=$2 AND state_effective_at<=p.start_at
 ORDER BY state_effective_at DESC,revision DESC LIMIT 1
 ) prior ON true
 LEFT JOIN LATERAL (
 SELECT snapshot FROM billing_allowance_schedule_history
 WHERE account_id=$1 AND schedule_id=$2 AND prior.snapshot IS NULL
 AND state_effective_at>=p.start_at AND state_effective_at<p.end_at
 ORDER BY state_effective_at ASC,revision ASC LIMIT 1
 ) initial ON true
 LEFT JOIN LATERAL (
 SELECT eligibility,source_id FROM billing_allowance_eligibility_events
 WHERE account_id=$1 AND schedule_id=$2 AND effective_at<=p.start_at
 ORDER BY effective_at DESC,observed_at DESC,source_id DESC LIMIT 1
 ) e ON true
 ORDER BY p.position`, string(s.account), scheduleID, starts, ends)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make([]credit.PeriodFact, 0, len(periods))
	for rows.Next() {
		var state, eligibility []byte
		var fact credit.PeriodFact
		if err := rows.Scan(&state, &eligibility, &fact.SourceID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(state, &fact.State); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(eligibility, &fact.Eligibility); err != nil {
			return nil, err
		}
		fact.State = normalizeStoredSchedule(fact.State)
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}
