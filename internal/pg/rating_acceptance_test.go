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

func TestPostgresRatingAcceptanceFreezesVersionsAndExcludesPrepaid(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "rating-acceptance-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	now := func() time.Time { return period.Cutoff }

	postpaidV1 := usage.RuleConfig{Version: "acceptance-postpaid-v1", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "10"}
	postpaidV2 := usage.RuleConfig{Version: "acceptance-postpaid-v2", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "15"}
	prepaidConfig := usage.RuleConfig{Version: "acceptance-prepaid-v1", Kind: usage.KindFixed, Target: usage.Target{CreditUnit: "credits"}, Rounding: usage.RoundDown, FixedRate: "2"}
	for _, config := range []usage.RuleConfig{postpaidV1, postpaidV2, prepaidConfig} {
		if err := store.PublishRating(ctx, config); err != nil {
			t.Fatal(err)
		}
	}
	ruleV1, err := store.Rating(ctx, postpaidV1.Version)
	if err != nil {
		t.Fatal(err)
	}
	ruleV2, err := store.Rating(ctx, postpaidV2.Version)
	if err != nil {
		t.Fatal(err)
	}
	prepaidRule, err := store.Rating(ctx, prepaidConfig.Version)
	if err != nil {
		t.Fatal(err)
	}

	originalObservation := usage.Observation{
		Account: "acct-a", ID: "acceptance-original", Source: "acceptance-meter",
		OccurredAt: period.Start.Add(time.Hour), Funding: usage.Postpaid,
		Input: usage.RateInput{ActionCount: 2},
	}
	original, err := usage.Prepare(originalObservation, ruleV1, period.Cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, original); err != nil {
		t.Fatal(err)
	}

	// A caller cannot alter persisted raw evidence while retaining the old
	// fingerprint/rating evidence. The durable record must remain unchanged.
	altered := original
	altered.Observation.Input.ActionCount++
	if _, err := recordUsage(ctx, store, altered); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("altered persisted observation accepted: %v", err)
	}
	storedOriginal, err := readUsage(ctx, store, "acct-a", originalObservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOriginal.Fingerprint != original.Fingerprint || storedOriginal.Observation.Input.ActionCount != originalObservation.Input.ActionCount || storedOriginal.Rating.RuleVersion != postpaidV1.Version {
		t.Fatalf("altered persisted observation changed durable evidence: %+v", storedOriginal)
	}

	// A real prepaid-funded record is settled through the credit reservation
	// boundary and is present in billing_usage, but must not enter postpaid
	// finalization.
	engine := credit.New(store, now)
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "acceptance-grant", LotID: "acceptance-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "acceptance-payment", ValidFrom: period.Start}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "acceptance-reserve", ReservationID: "acceptance-reservation", Actor: "acceptance-worker", Unit: "credits", Scope: "ACCEPTANCE", Amount: 4, Deadline: period.Cutoff.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	prepaidObservation := usage.Observation{
		Account: "acct-a", ID: "acceptance-prepaid", Source: "prepaid-meter",
		OccurredAt: period.Start.Add(2 * time.Hour), Funding: usage.Prepaid,
		ReservationID: "acceptance-reservation", Input: usage.RateInput{ActionCount: 2},
	}
	if _, _, err := integration.SettlePrepaid(ctx, store, prepaidObservation, prepaidRule, "acceptance-settle-prepaid", now); err != nil {
		t.Fatal(err)
	}

	pending, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: estimateTestPeriod(), Currency: "USD", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != usage.StatusEstimated || pending.Total != original.Rating.RoundedAmount || len(pending.Lines) != 1 || pending.Lines[0].UsageID != originalObservation.ID {
		t.Fatalf("pending estimate mixed prepaid usage: %+v", pending)
	}

	// The caller supplies no snapshot. Finalization reads all eligible stored
	// postpaid records and keeps the original v1 evidence frozen.
	originalBatch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "acct-a", Operation: "acceptance-close", BatchID: "acceptance-batch", Period: period, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if originalBatch.Total != original.Rating.RoundedAmount || len(originalBatch.Lines) != 1 || originalBatch.Lines[0].RuleVersion != postpaidV1.Version {
		t.Fatalf("finalized original changed rating evidence: %+v", originalBatch)
	}

	reproduced, err := usage.Prepare(storedOriginal.Observation, ruleV1, storedOriginal.ReceivedAt)
	if err != nil {
		t.Fatal(err)
	}
	if reproduced.Fingerprint != storedOriginal.Fingerprint || reproduced.Rating.RuleVersion != postpaidV1.Version || reproduced.Rating.RoundedAmount != storedOriginal.Rating.RoundedAmount {
		t.Fatalf("v1 evidence was not reproducible: %+v vs %+v", reproduced, storedOriginal)
	}

	// Re-rate the same raw observation under v2 as a new immutable usage record,
	// then link it to the frozen original with a signed adjustment in a late
	// correction. The original batch remains independently reproducible.
	reratedObservation := originalObservation
	reratedObservation.ID = "acceptance-rerated-v2"
	rerated, err := usage.Prepare(reratedObservation, ruleV2, period.Cutoff.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rerated.Observation.Source != storedOriginal.Observation.Source || rerated.Observation.OccurredAt != storedOriginal.Observation.OccurredAt || rerated.Observation.Input.ActionCount != storedOriginal.Observation.Input.ActionCount || rerated.Rating.RoundedAmount == storedOriginal.Rating.RoundedAmount {
		t.Fatalf("v2 rerating did not preserve raw evidence/change price: original=%+v rerated=%+v", storedOriginal, rerated)
	}
	if _, err := recordUsage(ctx, store, rerated); err != nil {
		t.Fatal(err)
	}
	correction, err := usage.NewSettlement(store.Settlements(), now).Correct(ctx, usage.CorrectionInput{
		Account: "acct-a", Operation: "acceptance-correction", BatchID: "acceptance-correction-batch", OriginalBatchID: originalBatch.ID,
		Period: period, Currency: "USD", UsageIDs: []string{rerated.Observation.ID},
		Adjustments: []usage.Adjustment{{ID: "acceptance-v1-reversal", OriginalUsageID: originalObservation.ID, Amount: -storedOriginal.Rating.RoundedAmount, Currency: "USD", Reason: "replace v1 rating with signed v2 rerating"}},
		CreatedAt:   period.Cutoff.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	correctionBatch, err := readPagedSettlementBatch(ctx, store.Settlements(), now, "acct-a", correction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if correctionBatch.OriginalBatchID != originalBatch.ID || correction.Total != rerated.Rating.RoundedAmount-original.Rating.RoundedAmount || len(correctionBatch.Lines) != 2 {
		t.Fatalf("linked rerating correction = %+v", correction)
	}
	var sawRerated, sawAdjustment bool
	for _, line := range correctionBatch.Lines {
		switch line.Kind {
		case "usage":
			sawRerated = line.UsageID == reratedObservation.ID && line.RuleVersion == postpaidV2.Version && line.Amount == rerated.Rating.RoundedAmount
		case "adjustment":
			sawAdjustment = line.OriginalUsageID == originalObservation.ID && line.Amount == -storedOriginal.Rating.RoundedAmount
		}
	}
	if !sawRerated || !sawAdjustment {
		t.Fatalf("correction did not retain signed linked lines: %+v", correctionBatch.Lines)
	}

	frozen, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", Period: estimateTestPeriod(), Currency: "USD", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Status != usage.StatusFinalized || frozen.BatchID != originalBatch.ID || frozen.Total != originalBatch.Total || len(frozen.Lines) != 1 || frozen.Lines[0].RuleVersion != postpaidV1.Version {
		t.Fatalf("original frozen estimate changed after correction: %+v", frozen)
	}
	correctionEstimate, err := store.EstimateUsage(ctx, usage.EstimateInput{Account: "acct-a", BatchID: correction.ID, Period: estimateTestPeriod(), Currency: "USD", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if correctionEstimate.Status != usage.StatusFinalized || correctionEstimate.BatchID != correction.ID || correctionEstimate.Total != correction.Total || len(correctionEstimate.Lines) != 2 {
		t.Fatalf("correction estimate = %+v", correctionEstimate)
	}
}
