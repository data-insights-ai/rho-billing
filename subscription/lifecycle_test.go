package subscription

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestFreeTrialAndPaidAccessStayIndependentOfCollection(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	free, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-free", ID: "life-free", Operation: "activate-free", Quantity: 1, Trial: true,
		Items:    []Item{lifecycleItem("seat", 1, start, start.AddDate(0, 0, 14))},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionNone, Proration: ProrationNone, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: start.AddDate(0, 0, 14)}, At: start,
	})
	if err != nil || free.Access != AccessTrial || free.Collection != CollectionIdle || free.Ref.ID != "" {
		t.Fatalf("free trial=%+v err=%v", free, err)
	}
	if _, err := svc.RecordCollection(t.Context(), CollectionInput{Account: "acct-free", ID: "life-free", Operation: "invoice-free", State: CollectionInvoiced, At: start}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("free collection=%v", err)
	}

	paid, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-paid", ID: "life-paid", Operation: "activate-paid", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessPaid, Collection: CollectionAutomatic, Proration: ProrationNone, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil || paid.Access != AccessNone || paid.Collection != CollectionPending {
		t.Fatalf("paid activate=%+v err=%v", paid, err)
	}
	invoiced, err := svc.RecordCollection(t.Context(), CollectionInput{Account: "acct-paid", ID: "life-paid", Operation: "invoice-1", State: CollectionInvoiced, At: start.Add(time.Minute)})
	if err != nil || invoiced.Access != AccessNone || invoiced.Collection != CollectionInvoiced {
		t.Fatalf("invoice is not payment: %+v err=%v", invoiced, err)
	}
	collected, err := svc.RecordCollection(t.Context(), CollectionInput{Account: "acct-paid", ID: "life-paid", Operation: "paid-1", State: CollectionPaid, At: start.Add(2 * time.Minute)})
	if err != nil || collected.Access != AccessActive || collected.Collection != CollectionPaid {
		t.Fatalf("paid collection=%+v err=%v", collected, err)
	}
}

func TestAbsoluteQuantityRevisionRejectsSupersededAndLateConfirm(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if _, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-seats", ID: "life-seats", Operation: "activate-seats", Quantity: 2,
		Items:    []Item{lifecycleItem("seat", 2, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
		Ref: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "m", Environment: "sandbox"}, ID: "sub-seats"},
	}); err != nil {
		t.Fatal(err)
	}
	newer, err := svc.SetDesiredQuantity(t.Context(), QuantityInput{Account: "acct-seats", ID: "life-seats", Operation: "qty-2", Quantity: 5, Revision: 2, At: start.Add(time.Minute)})
	if err != nil || newer.DesiredQuantity != 5 || newer.QuantityRevision != 2 {
		t.Fatalf("desired=%+v err=%v", newer, err)
	}
	if _, err := svc.SetDesiredQuantity(t.Context(), QuantityInput{Account: "acct-seats", ID: "life-seats", Operation: "qty-1", Quantity: 3, Revision: 1, At: start.Add(2 * time.Minute)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("superseded revision=%v", err)
	}
	replay, err := svc.SetDesiredQuantity(t.Context(), QuantityInput{Account: "acct-seats", ID: "life-seats", Operation: "qty-2", Quantity: 5, Revision: 2, At: start.Add(time.Minute)})
	if err != nil || replay.DesiredQuantity != 5 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	late := Snapshot{
		Account:    "acct-seats",
		Ref:        billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "m", Environment: "sandbox"}, ID: "sub-seats"},
		Status:     "active",
		Items:      []Item{lifecycleItem("seat", 2, start, end)},
		SourceTime: start.Add(3 * time.Minute),
	}
	confirmed, err := svc.Confirm(t.Context(), "acct-seats", "life-seats", late)
	if err != nil || confirmed.DesiredQuantity != 5 || confirmed.ConfirmedQuantity != 2 || confirmed.QuantityRevision != 2 {
		t.Fatalf("late confirm overwrote desired: %+v err=%v", confirmed, err)
	}
}

func TestScheduledPauseResumeCancelAndUpgradeKeepEffectiveTimes(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	life, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-change", ID: "life-change", Operation: "activate-change", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationNextPeriod, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewChange(ChangeInput{Kind: ChangeUpgrade, At: start.Add(time.Hour)}, life)
	if err != nil || !preview.EffectiveAt.Equal(end) {
		t.Fatalf("next-period preview=%+v err=%v", preview, err)
	}
	// A pause scheduled for later records the schedule without taking access
	// away now: the customer has paid through the current period.
	paused, err := svc.RequestChange(t.Context(), ChangeInput{Account: "acct-change", ID: "life-change", Operation: "pause-1", Kind: ChangePause, EffectiveAt: start.Add(24 * time.Hour), At: start.Add(time.Hour)})
	if err != nil || paused.Access != AccessActive || !paused.Change.EffectiveAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("pause=%+v err=%v", paused, err)
	}
	// Resume withdraws the scheduled pause.
	resumed, err := svc.RequestChange(t.Context(), ChangeInput{Account: "acct-change", ID: "life-change", Operation: "resume-1", Kind: ChangeResume, At: start.Add(2 * time.Hour)})
	if err != nil || resumed.Access != AccessActive {
		t.Fatalf("resume=%+v err=%v", resumed, err)
	}
	upgraded, err := svc.RequestChange(t.Context(), ChangeInput{
		Account: "acct-change", ID: "life-change", Operation: "upgrade-1", Kind: ChangeUpgrade,
		Items:    []Item{lifecycleItem("seat", 1, start, end), lifecycleItem("addon", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		At:       start.Add(3 * time.Hour),
	})
	if err != nil || len(upgraded.Items) != 2 || upgraded.Change.Kind != string(ChangeUpgrade) {
		t.Fatalf("upgrade=%+v err=%v", upgraded, err)
	}
	// Cancel-at-period-end keeps access until the period actually ends.
	canceled, err := svc.RequestChange(t.Context(), ChangeInput{Account: "acct-change", ID: "life-change", Operation: "cancel-1", Kind: ChangeCancel, EffectiveAt: end, At: start.Add(4 * time.Hour)})
	if err != nil || canceled.Access != AccessActive || !canceled.Change.EffectiveAt.Equal(end) {
		t.Fatalf("cancel=%+v err=%v", canceled, err)
	}
	// An immediate cancel still revokes access now.
	now, err := svc.RequestChange(t.Context(), ChangeInput{Account: "acct-change", ID: "life-change", Operation: "cancel-now", Kind: ChangeCancel, EffectiveAt: start.Add(5 * time.Hour), At: start.Add(5 * time.Hour)})
	if err != nil || now.Access != AccessCanceled {
		t.Fatalf("immediate cancel=%+v err=%v", now, err)
	}
}

func TestKeepPeriodUpgradeDoesNotMoveAllowanceAnchor(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	life, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-allow", ID: "life-allow", Operation: "activate-allow", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionNone, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDay := life.AllowanceDay
	upgraded, err := svc.RequestChange(t.Context(), ChangeInput{
		Account: "acct-allow", ID: "life-allow", Operation: "upgrade-keep", Kind: ChangeUpgrade,
		Items: []Item{lifecycleItem("seat", 2, start.AddDate(0, 1, 0), end)}, Quantity: 2, QuantityRevision: 2,
		Policies: Policies{Access: AccessImmediate, Collection: CollectionNone, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		At:       start.AddDate(0, 1, 0),
	})
	if err != nil || upgraded.AllowanceDay != firstDay || !upgraded.AllowanceAnchor.Equal(start) {
		t.Fatalf("keep period moved anchor=%+v err=%v", upgraded, err)
	}
	again, err := svc.RequestChange(t.Context(), ChangeInput{
		Account: "acct-allow", ID: "life-allow", Operation: "upgrade-keep-2", Kind: ChangeUpgrade,
		Items: []Item{lifecycleItem("seat", 3, start.AddDate(0, 2, 0), end)}, Quantity: 3, QuantityRevision: 3,
		Policies: Policies{Access: AccessImmediate, Collection: CollectionNone, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		At:       start.AddDate(0, 2, 0),
	})
	if err != nil || again.AllowanceDay != firstDay {
		t.Fatalf("second upgrade refilled calendar=%+v err=%v", again, err)
	}
}

func TestAnnualCoveragePinsMonthEndAnchor(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2029, 1, 31, 0, 0, 0, 0, time.UTC)
	life, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-annual", ID: "life-annual", Operation: "activate-annual", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationNone, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil || life.AllowanceDay != 31 || !life.AllowanceAnchor.Equal(start) || !life.Coverage.End.Equal(end) {
		t.Fatalf("annual coverage=%+v err=%v", life, err)
	}
}

func TestManualInvoiceIsNotPaidAndFailedPaymentUsesGrace(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	life, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-manual", ID: "life-manual", Operation: "activate-manual", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessGrace, Collection: CollectionManual, Proration: ProrationNone, Allowance: AllowanceKeepPeriod, UsageDuringGrace: false},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil || life.Access != AccessActive || life.Collection != CollectionPending {
		t.Fatalf("manual activate=%+v err=%v", life, err)
	}
	invoiced, err := svc.RecordCollection(t.Context(), CollectionInput{Account: "acct-manual", ID: "life-manual", Operation: "invoice-manual", State: CollectionInvoiced, At: start.Add(time.Minute)})
	if err != nil || invoiced.Access != AccessActive || invoiced.Collection != CollectionInvoiced {
		t.Fatalf("invoice granted payment: %+v err=%v", invoiced, err)
	}
	failed, err := svc.RecordCollection(t.Context(), CollectionInput{Account: "acct-manual", ID: "life-manual", Operation: "fail-manual", State: CollectionFailed, At: start.Add(2 * time.Minute)})
	if err != nil || failed.Access != AccessInGrace || failed.Collection != CollectionFailed || failed.Policies.UsageDuringGrace {
		t.Fatalf("failed payment=%+v err=%v", failed, err)
	}
}

func lifecycleItem(id string, qty int64, start, end time.Time) Item {
	return Item{ID: id, PlanVersion: "plan-1", Quantity: qty, Period: billing.Period{Start: start, End: end}}
}

func TestConfirmRejectsSnapshotWithoutObservationFence(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "m", Environment: "sandbox"}, ID: "sub-fence"}
	if _, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-fence", ID: "life-fence", Operation: "activate-fence", Quantity: 1,
		Items:    []Item{lifecycleItem("seat", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start, Ref: ref,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Account: "acct-fence", Ref: ref, Status: "active", Items: []Item{lifecycleItem("seat", 1, start, end)}}
	if _, err := svc.Confirm(t.Context(), "acct-fence", "life-fence", snapshot); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("zero source time error=%v, want invalid", err)
	}
	snapshot.SourceTime = start.Add(2 * time.Minute)
	confirmed, err := svc.Confirm(t.Context(), "acct-fence", "life-fence", snapshot)
	if err != nil || !confirmed.UpdatedAt.Equal(billing.CanonicalTime(snapshot.SourceTime)) {
		t.Fatalf("confirmed=%+v err=%v, want UpdatedAt from the observation fence", confirmed, err)
	}
	// A reordered redelivery must not regress the confirmed state.
	stale := snapshot
	stale.SourceTime = start.Add(time.Minute)
	stale.Items = []Item{lifecycleItem("seat", 99, start, end)}
	unchanged, err := svc.Confirm(t.Context(), "acct-fence", "life-fence", stale)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != confirmed.Revision || unchanged.ConfirmedQuantity != confirmed.ConfirmedQuantity || !unchanged.UpdatedAt.Equal(confirmed.UpdatedAt) {
		t.Fatalf("stale snapshot applied: %+v, want %+v", unchanged, confirmed)
	}
}

// Coverage is either wholly unset or a valid period. A half-set one used to
// slip through because IsZero() == IsZero() is false when exactly one end is
// set, so the guard short-circuited and the partial period reached the
// allowance anchor.
func TestLifecycleRejectsHalfSetCoverage(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	base := Lifecycle{
		Account: "acct-coverage", ID: "life-coverage", Revision: 1,
		DesiredQuantity: 1, QuantityRevision: 1,
		Items:     []Item{lifecycleItem("seat", 1, start, end)},
		Policies:  Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		Access:    AccessActive,
		CreatedAt: start, UpdatedAt: start,
	}
	whole := base
	whole.Coverage = billing.Period{Start: start, End: end}
	if err := whole.Validate(); err != nil {
		t.Fatalf("complete coverage rejected: %v", err)
	}
	unset := base
	if err := unset.Validate(); err != nil {
		t.Fatalf("absent coverage rejected: %v", err)
	}
	for name, coverage := range map[string]billing.Period{
		"only start": {Start: start},
		"only end":   {End: end},
	} {
		half := base
		half.Coverage = coverage
		if err := half.Validate(); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("%s coverage accepted: err=%v", name, err)
		}
	}
}

// A subscription that changes plan must carry the new plan's billing
// period, not the one it was activated with. Activation refuses a
// subscription that already exists, so the provider's confirmation is the
// only place the new period can enter the model; before it did, an
// organization that moved from a monthly plan to a yearly one kept a
// month-long window for ever. The window is what a change scheduled for
// "next period" lands on, so a stale one puts it in the past.
func TestConfirmAdoptsTheProvidersBillingPeriod(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 9, 19, 7, 0, 0, 0, time.UTC)
	monthEnd := start.AddDate(0, 1, 0)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "m", Environment: "sandbox"}, ID: "sub-term"}
	if _, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-term", ID: "life-term", Operation: "activate-term", Quantity: 1,
		Items:    []Item{lifecycleItem("pro", 1, start, monthEnd)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationNextPeriod, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: monthEnd}, At: start, Ref: ref,
	}); err != nil {
		t.Fatal(err)
	}

	// The same day, the organization moves to a yearly plan.
	upgraded := start.Add(12 * time.Hour)
	yearEnd := upgraded.AddDate(1, 0, 0)
	confirmed, err := svc.Confirm(t.Context(), "acct-term", "life-term", Snapshot{
		Account: "acct-term", Ref: ref, Status: "active", SourceTime: upgraded,
		Items: []Item{lifecycleItem("enterprise", 1, upgraded, yearEnd)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed.Items) != 1 || confirmed.Items[0].ID != "enterprise" {
		t.Fatalf("items = %+v, want the plan the subscription now holds", confirmed.Items)
	}
	if !confirmed.Items[0].Period.End.Equal(yearEnd) {
		t.Fatalf("item period ends %v, want %v", confirmed.Items[0].Period.End, yearEnd)
	}
	if !confirmed.Coverage.End.Equal(yearEnd) {
		t.Fatalf("coverage ends %v, want %v: a change scheduled for the next period would land in the past", confirmed.Coverage.End, yearEnd)
	}
	if err := confirmed.Validate(); err != nil {
		t.Fatalf("confirmed lifecycle invalid: %v", err)
	}
}

// A snapshot with no items says nothing about the term and must not
// replace a window we do know with nothing. One with items is already
// required by Validate to carry a period on each, so there is no third
// case to test.
func TestConfirmKeepsThePeriodWhenTheSnapshotHasNoItems(t *testing.T) {
	svc := New(NewMemoryRepository())
	start := time.Date(2026, 9, 19, 7, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "m", Environment: "sandbox"}, ID: "sub-bare"}
	if _, err := svc.Activate(t.Context(), ActivateInput{
		Account: "acct-bare", ID: "life-bare", Operation: "activate-bare", Quantity: 1,
		Items:    []Item{lifecycleItem("pro", 1, start, end)},
		Policies: Policies{Access: AccessImmediate, Collection: CollectionAutomatic, Proration: ProrationImmediate, Allowance: AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start, Ref: ref,
	}); err != nil {
		t.Fatal(err)
	}
	confirmed, err := svc.Confirm(t.Context(), "acct-bare", "life-bare", Snapshot{
		Account: "acct-bare", Ref: ref, Status: "active", SourceTime: start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed.Coverage.End.Equal(end) || len(confirmed.Items) != 1 || !confirmed.Items[0].Period.End.Equal(end) {
		t.Fatalf("a snapshot with no items replaced a known term: coverage %v items %+v", confirmed.Coverage, confirmed.Items)
	}

	// And an item without a period never reaches Confirm at all.
	if err := Validate(Snapshot{
		Account: "acct-bare", Ref: ref, Status: "active",
		Items: []Item{{ID: "pro", PlanVersion: "plan-1", Quantity: 1}},
	}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("an item without a period was accepted: %v", err)
	}
}
