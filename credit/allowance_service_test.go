package credit

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"

	"github.com/data-insights-ai/rho-billing/subscription"
)

// referenceRepository is deliberately test-only. WithinAccount works like a
// database transaction: it gives the callback a private copy and publishes
// that copy only when the callback and the transaction both succeed.
type referenceRepository struct {
	mu                   sync.Mutex
	plans                map[string]catalog.PlanVersion
	subscriptions        map[billing.Reference]subscription.Snapshot
	schedules            map[string]Schedule
	recordedAt           map[string]time.Time
	eligibilityBySource  map[eligibilitySourceKey]EligibilityObservation
	eligibilityByVersion map[eligibilityVersionKey]EligibilityObservation
	issuances            map[issuanceKey]Issuance
	lineageByIssuance    map[issuanceKey]string
	checkpoints          map[string]Checkpoint
	changes              []AllowanceChange
	credits              Repository
	failAfter            error
}

func newReferenceRepository(plan catalog.PlanVersion, snap subscription.Snapshot) *referenceRepository {
	return &referenceRepository{
		plans:                map[string]catalog.PlanVersion{plan.ID: clonePlan(plan)},
		subscriptions:        map[billing.Reference]subscription.Snapshot{snap.Ref: subscription.Copy(snap)},
		schedules:            make(map[string]Schedule),
		recordedAt:           make(map[string]time.Time),
		eligibilityBySource:  make(map[eligibilitySourceKey]EligibilityObservation),
		eligibilityByVersion: make(map[eligibilityVersionKey]EligibilityObservation),
		issuances:            make(map[issuanceKey]Issuance),
		lineageByIssuance:    make(map[issuanceKey]string),
		checkpoints:          make(map[string]Checkpoint),
		credits:              NewMemoryRepository(snap.Account),
	}
}

func (r *referenceRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(AllowanceTx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	working := &referenceTx{
		account:              account,
		plans:                clonePlans(r.plans),
		subscriptions:        cloneSubscriptions(r.subscriptions),
		schedules:            cloneSchedules(r.schedules),
		recordedAt:           cloneTimes(r.recordedAt),
		eligibilityBySource:  cloneEligibility(r.eligibilityBySource),
		eligibilityByVersion: cloneEligibilityVersions(r.eligibilityByVersion),
		issuances:            maps.Clone(r.issuances),
		lineageByIssuance:    maps.Clone(r.lineageByIssuance),
		checkpoints:          maps.Clone(r.checkpoints),
		changes:              slices.Clone(r.changes),
	}
	err := r.credits.WithinAccount(ctx, account, func(creditTx Tx) error {
		working.credits = testBoundCreditRepository{account: account, tx: creditTx}
		if err := fn(working); err != nil {
			return err
		}
		if r.failAfter != nil {
			err := r.failAfter
			r.failAfter = nil
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.plans, r.subscriptions, r.schedules, r.recordedAt = working.plans, working.subscriptions, working.schedules, working.recordedAt
	r.eligibilityBySource, r.eligibilityByVersion = working.eligibilityBySource, working.eligibilityByVersion
	r.issuances, r.lineageByIssuance = working.issuances, working.lineageByIssuance
	r.checkpoints = working.checkpoints
	r.changes = working.changes
	return nil
}

type referenceTx struct {
	account              billing.AccountID
	plans                map[string]catalog.PlanVersion
	subscriptions        map[billing.Reference]subscription.Snapshot
	schedules            map[string]Schedule
	recordedAt           map[string]time.Time
	eligibilityBySource  map[eligibilitySourceKey]EligibilityObservation
	eligibilityByVersion map[eligibilityVersionKey]EligibilityObservation
	issuances            map[issuanceKey]Issuance
	lineageByIssuance    map[issuanceKey]string
	checkpoints          map[string]Checkpoint
	changes              []AllowanceChange
	credits              Repository
}

type eligibilitySourceKey struct {
	scheduleID string
	sourceID   string
}

type eligibilityVersionKey struct {
	scheduleID  string
	effectiveAt time.Time
	observedAt  time.Time
}

type issuanceKey struct {
	scheduleID, definitionID string
	periodStart              time.Time
}

type testBoundCreditRepository struct {
	account billing.AccountID
	tx      Tx
}

func (r testBoundCreditRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	if account != r.account {
		return billing.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(r.tx)
}

func (testBoundCreditRepository) History(context.Context, billing.AccountID, int64, int) ([]Entry, error) {
	return nil, billing.ErrState
}

func (tx *referenceTx) Plan(ctx context.Context, id string) (catalog.PlanVersion, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PlanVersion{}, err
	}
	plan, ok := tx.plans[id]
	if !ok {
		return catalog.PlanVersion{}, billing.ErrNotFound
	}
	return clonePlan(plan), nil
}

func (tx *referenceTx) Subscription(ctx context.Context, ref billing.Reference) (subscription.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return subscription.Snapshot{}, err
	}
	snap, ok := tx.subscriptions[ref]
	if !ok || snap.Account != tx.account {
		return subscription.Snapshot{}, billing.ErrNotFound
	}
	return subscription.Copy(snap), nil
}

func (tx *referenceTx) Schedule(ctx context.Context, id string) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	schedule, ok := tx.schedules[id]
	if !ok || schedule.Account != tx.account {
		return Schedule{}, billing.ErrNotFound
	}
	return schedule, nil
}

func (tx *referenceTx) TransitionAnchor(ctx context.Context, id string) (time.Time, error) {
	schedule, err := tx.Schedule(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Anchor, nil
}

func (tx *referenceTx) SaveSchedule(ctx context.Context, schedule Schedule, expectedRevision int64, recordedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if schedule.Account != tx.account {
		return billing.ErrNotFound
	}
	current, exists := tx.schedules[schedule.ID]
	if (!exists && expectedRevision != 0) || (exists && (current.Revision != expectedRevision || expectedRevision == 0)) {
		return billing.ErrConflict
	}
	tx.schedules[schedule.ID] = schedule
	tx.recordedAt[schedule.ID] = billing.CanonicalTime(recordedAt)
	tx.changes = append(tx.changes, AllowanceChange{Sequence: int64(len(tx.changes) + 1), ScheduleID: schedule.ID, EffectiveAt: schedule.StateEffectiveAt, ChangedAt: time.Now()})
	return nil
}

func (tx *referenceTx) EligibilityBySource(ctx context.Context, scheduleID, sourceID string) (EligibilityObservation, error) {
	if err := ctx.Err(); err != nil {
		return EligibilityObservation{}, err
	}
	observation, ok := tx.eligibilityBySource[eligibilitySourceKey{scheduleID: scheduleID, sourceID: sourceID}]
	if !ok || observation.Account != tx.account {
		return EligibilityObservation{}, billing.ErrNotFound
	}
	return cloneEligibilityObservation(observation), nil
}

func (tx *referenceTx) EligibilityAtVersion(ctx context.Context, scheduleID string, effectiveAt, observedAt time.Time) (EligibilityObservation, error) {
	if err := ctx.Err(); err != nil {
		return EligibilityObservation{}, err
	}
	key := eligibilityVersionKey{scheduleID: scheduleID, effectiveAt: billing.CanonicalTime(effectiveAt), observedAt: billing.CanonicalTime(observedAt)}
	observation, ok := tx.eligibilityByVersion[key]
	if !ok || observation.Account != tx.account {
		return EligibilityObservation{}, billing.ErrNotFound
	}
	return cloneEligibilityObservation(observation), nil
}

func (tx *referenceTx) InsertEligibility(ctx context.Context, observation EligibilityObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if observation.Account != tx.account {
		return billing.ErrNotFound
	}
	observation = cloneEligibilityObservation(observation)
	observation.EffectiveAt = billing.CanonicalTime(observation.EffectiveAt)
	observation.ObservedAt = billing.CanonicalTime(observation.ObservedAt)
	tx.eligibilityBySource[eligibilitySourceKey{scheduleID: observation.ScheduleID, sourceID: observation.SourceID}] = observation
	tx.eligibilityByVersion[eligibilityVersionKey{scheduleID: observation.ScheduleID, effectiveAt: observation.EffectiveAt, observedAt: observation.ObservedAt}] = observation
	tx.changes = append(tx.changes, AllowanceChange{Sequence: int64(len(tx.changes) + 1), ScheduleID: observation.ScheduleID, EffectiveAt: observation.EffectiveAt, ChangedAt: time.Now()})
	return nil
}

// These fixture reads exercise the actual service and bound credit engine.
// Historical schedule revisions are covered by the PostgreSQL conformance tests.
func (tx *referenceTx) Schedules(ctx context.Context, after string, inclusive bool, limit int) ([]Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	result := make([]Schedule, 0, len(tx.schedules))
	for _, schedule := range tx.schedules {
		if schedule.Account == tx.account && (schedule.ID > after || (inclusive && schedule.ID == after)) {
			result = append(result, schedule)
		}
	}
	slices.SortFunc(result, func(a, b Schedule) int { return cmp.Compare(a.ID, b.ID) })
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
func (tx *referenceTx) SuccessorAt(ctx context.Context, id string) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	var successor time.Time
	for _, schedule := range tx.schedules {
		if schedule.Account == tx.account && schedule.PreviousScheduleID == id && (successor.IsZero() || schedule.Assignment.Effective.Start.Before(successor)) {
			successor = schedule.Assignment.Effective.Start
		}
	}
	return successor, nil
}
func (tx *referenceTx) Lineage(ctx context.Context, id string) (Lineage, error) {
	if err := ctx.Err(); err != nil {
		return Lineage{}, err
	}
	current, ok := tx.schedules[id]
	if !ok || current.Account != tx.account {
		return Lineage{}, billing.ErrNotFound
	}
	seen := make(map[string]struct{})
	for current.PreviousScheduleID != "" {
		if _, ok := seen[current.ID]; ok {
			return Lineage{}, billing.ErrConflict
		}
		seen[current.ID] = struct{}{}
		previous, ok := tx.schedules[current.PreviousScheduleID]
		if !ok || previous.Account != tx.account {
			return Lineage{}, billing.ErrNotFound
		}
		current = previous
	}
	return Lineage{RootAssignment: current.Assignment.ID, RootAnchor: current.Anchor, TransitionAnchor: current.Anchor}, nil
}
func (tx *referenceTx) PeriodFacts(ctx context.Context, scheduleID string, periods []billing.Period) ([]PeriodFact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	schedule, ok := tx.schedules[scheduleID]
	if !ok || schedule.Account != tx.account {
		return nil, billing.ErrNotFound
	}
	facts := make([]PeriodFact, len(periods))
	for i, period := range periods {
		if !period.Valid() {
			return nil, billing.ErrInvalid
		}
		// The reference fixture stores the current schedule only. PostgreSQL
		// supplies historical schedule revisions through its history table.
		state := schedule
		fact := PeriodFact{State: state}
		var latestEffective, latestObserved time.Time
		var latestSource string
		for _, observation := range tx.eligibilityBySource {
			if observation.Account == tx.account && observation.ScheduleID == scheduleID && !observation.EffectiveAt.After(period.Start) && (fact.SourceID == "" || observation.EffectiveAt.After(latestEffective) || (observation.EffectiveAt.Equal(latestEffective) && (observation.ObservedAt.After(latestObserved) || (observation.ObservedAt.Equal(latestObserved) && observation.SourceID > latestSource)))) {
				fact.Eligibility, fact.SourceID = observation.Eligibility, observation.SourceID
				latestEffective = observation.EffectiveAt
				latestObserved = observation.ObservedAt
				latestSource = observation.SourceID
			}
		}
		facts[i] = fact
	}
	return facts, nil
}
func (tx *referenceTx) Issuance(ctx context.Context, scheduleID, definitionID string, periodStart time.Time) (Issuance, error) {
	if err := ctx.Err(); err != nil {
		return Issuance{}, err
	}
	issuance, ok := tx.issuances[issuanceKey{scheduleID: scheduleID, definitionID: definitionID, periodStart: billing.CanonicalTime(periodStart)}]
	if !ok || issuance.Account != tx.account {
		return Issuance{}, billing.ErrNotFound
	}
	return issuance, nil
}
func (tx *referenceTx) HighWater(ctx context.Context, lineage, definitionID string, periodStart time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var highWater int64
	for key, issuance := range tx.issuances {
		if key.definitionID == definitionID && key.periodStart.Equal(billing.CanonicalTime(periodStart)) && tx.lineageByIssuance[key] == lineage && issuance.EntitledAmount > highWater {
			highWater = issuance.EntitledAmount
		}
	}
	return highWater, nil
}
func (tx *referenceTx) SaveIssuance(ctx context.Context, issuance Issuance, lineage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if issuance.Account != tx.account {
		return billing.ErrNotFound
	}
	key := issuanceKey{scheduleID: issuance.ScheduleID, definitionID: issuance.DefinitionID, periodStart: billing.CanonicalTime(issuance.Period.Start)}
	if existing, ok := tx.issuances[key]; ok {
		if existing != issuance || tx.lineageByIssuance[key] != lineage {
			return billing.ErrConflict
		}
		return nil
	}
	tx.issuances[key] = issuance
	tx.lineageByIssuance[key] = lineage
	return nil
}
func (tx *referenceTx) Credits() Repository { return tx.credits }

func (tx *referenceTx) Checkpoint(ctx context.Context, id string) (Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	checkpoint, ok := tx.checkpoints[id]
	if !ok || checkpoint.Account != tx.account {
		return Checkpoint{}, billing.ErrNotFound
	}
	return checkpoint, nil
}

func (tx *referenceTx) SaveCheckpoint(ctx context.Context, checkpoint Checkpoint, expectedRevision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkpoint.Account != tx.account || checkpoint.ID == "" || checkpoint.Revision != expectedRevision+1 {
		return billing.ErrConflict
	}
	if current, ok := tx.checkpoints[checkpoint.ID]; ok && current.Revision != expectedRevision {
		return billing.ErrConflict
	}
	tx.checkpoints[checkpoint.ID] = checkpoint
	return nil
}

func (tx *referenceTx) AllowanceChanges(ctx context.Context, after int64, limit int) ([]AllowanceChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 256 {
		return nil, billing.ErrInvalid
	}
	result := make([]AllowanceChange, 0, limit)
	for _, change := range tx.changes {
		if change.Sequence > after {
			result = append(result, change)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (tx *referenceTx) AllowanceChangeWatermark(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return int64(len(tx.changes)), nil
}

func clonePlan(plan catalog.PlanVersion) catalog.PlanVersion {
	plan.Entitlements = slices.Clone(plan.Entitlements)
	plan.Allowances = slices.Clone(plan.Allowances)
	return plan
}

func clonePlans(in map[string]catalog.PlanVersion) map[string]catalog.PlanVersion {
	out := make(map[string]catalog.PlanVersion, len(in))
	for id, plan := range in {
		out[id] = clonePlan(plan)
	}
	return out
}

func cloneSubscriptions(in map[billing.Reference]subscription.Snapshot) map[billing.Reference]subscription.Snapshot {
	out := make(map[billing.Reference]subscription.Snapshot, len(in))
	for ref, snap := range in {
		out[ref] = subscription.Copy(snap)
	}
	return out
}

func cloneSchedules(in map[string]Schedule) map[string]Schedule {
	return maps.Clone(in)
}

func cloneTimes(in map[string]time.Time) map[string]time.Time {
	return maps.Clone(in)
}

func cloneEligibilityObservation(observation EligibilityObservation) EligibilityObservation {
	observation.Eligibility.Evidence = slices.Clone(observation.Eligibility.Evidence)
	return observation
}

func cloneEligibility(in map[eligibilitySourceKey]EligibilityObservation) map[eligibilitySourceKey]EligibilityObservation {
	out := make(map[eligibilitySourceKey]EligibilityObservation, len(in))
	for key, observation := range in {
		out[key] = cloneEligibilityObservation(observation)
	}
	return out
}

func cloneEligibilityVersions(in map[eligibilityVersionKey]EligibilityObservation) map[eligibilityVersionKey]EligibilityObservation {
	out := make(map[eligibilityVersionKey]EligibilityObservation, len(in))
	for key, observation := range in {
		out[key] = cloneEligibilityObservation(observation)
	}
	return out
}

type serviceFixture struct {
	repo       *referenceRepository
	service    *AllowanceService
	schedule   Schedule
	assignment catalog.PlanAssignment
	now        time.Time
}

func newServiceFixture() serviceFixture {
	account := billing.AccountID("service-account")
	start := time.Date(2026, time.January, 1, 0, 0, 0, 123456789, time.FixedZone("fixture", 3600))
	end := start.AddDate(0, 3, 0)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "service-merchant", Environment: "sandbox"}, ID: "service-subscription"}
	plan := catalog.PlanVersion{ID: "service-plan-v1", PlanID: "service-plan", Version: 1, PublishedAt: start, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "account"}}}
	assignment := catalog.PlanAssignment{ID: "service-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	snap := subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}
	now := time.Date(2026, time.January, 2, 3, 4, 5, 987654321, time.FixedZone("clock", -3600))
	repo := newReferenceRepository(plan, snap)
	schedule := Schedule{Account: account, ID: "service-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: ScheduleActive, StateEffectiveAt: start, SourceID: "service-event"}
	return serviceFixture{repo: repo, service: NewAllowances(repo, func() time.Time { return now }), schedule: schedule, assignment: assignment, now: now}
}

func TestServicePutScheduleCreatesCanonicalRevision(t *testing.T) {
	f := newServiceFixture()
	got, err := f.service.PutSchedule(t.Context(), f.schedule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || !got.StateEffectiveAt.Equal(billing.CanonicalTime(f.schedule.StateEffectiveAt)) {
		t.Fatalf("schedule = %+v, want revision 1 and canonical state time", got)
	}
	if recorded := f.repo.recordedAt[f.schedule.ID]; !recorded.Equal(billing.CanonicalTime(f.now)) {
		t.Fatalf("recordedAt = %v, want %v", recorded, billing.CanonicalTime(f.now))
	}
}

func TestServicePutScheduleUsesCompareAndSwapRevision(t *testing.T) {
	f := newServiceFixture()
	if _, err := f.service.PutSchedule(t.Context(), f.schedule, 0); err != nil {
		t.Fatal(err)
	}
	updated := f.schedule
	updated.State = SchedulePaused
	updated.StateEffectiveAt = updated.StateEffectiveAt.Add(time.Hour)
	got, err := f.service.PutSchedule(t.Context(), updated, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 {
		t.Fatalf("revision = %d, want 2", got.Revision)
	}
	if _, err := f.service.PutSchedule(t.Context(), updated, 1); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale update error = %v, want conflict", err)
	}
}

func TestServicePutScheduleRejectsImmutableIdentityChanges(t *testing.T) {
	for name, mutate := range map[string]func(*Schedule){
		"anchor":        func(s *Schedule) { s.Anchor = s.Anchor.Add(time.Hour) },
		"assignment id": func(s *Schedule) { s.Assignment.ID = "other-assignment" },
		"previous":      func(s *Schedule) { s.PreviousScheduleID = "previous-schedule" },
		"subscription":  func(s *Schedule) { s.Subscription.ID = "other-subscription" },
	} {
		t.Run(name, func(t *testing.T) {
			base := newServiceFixture()
			if _, err := base.service.PutSchedule(t.Context(), base.schedule, 0); err != nil {
				t.Fatal(err)
			}
			candidate := base.schedule
			mutate(&candidate)
			if name == "assignment id" {
				snap := base.repo.subscriptions[base.schedule.Subscription]
				snap.Assignments[0].ID = candidate.Assignment.ID
				base.repo.subscriptions[base.schedule.Subscription] = snap
			}
			if name == "subscription" {
				snap := base.repo.subscriptions[base.schedule.Subscription]
				delete(base.repo.subscriptions, base.schedule.Subscription)
				snap.Ref = candidate.Subscription
				base.repo.subscriptions[candidate.Subscription] = snap
			}
			if _, err := base.service.PutSchedule(t.Context(), candidate, 1); !errors.Is(err, ErrAdjustment) {
				t.Fatalf("error = %v, want adjustment", err)
			}
		})
	}
}

func TestServicePutScheduleRejectsWrongAssignmentSource(t *testing.T) {
	f := newServiceFixture()
	f.repo.subscriptions[f.schedule.Subscription] = subscription.Snapshot{Account: f.schedule.Account, Ref: f.schedule.Subscription, Status: "active", Assignments: []catalog.PlanAssignment{{ID: f.assignment.ID, PlanVersionID: f.assignment.PlanVersionID, Quantity: f.assignment.Quantity, Effective: f.assignment.Effective, Source: catalog.SourceAddOn}}}
	if _, err := f.service.PutSchedule(t.Context(), f.schedule, 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("wrong source error = %v, want conflict", err)
	}
}

func TestServicePutScheduleRollsBackWhenTransactionFailsAfterCallback(t *testing.T) {
	f := newServiceFixture()
	sentinel := errors.New("commit failed")
	f.repo.failAfter = sentinel
	got, err := f.service.PutSchedule(t.Context(), f.schedule, 0)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if got != (Schedule{}) {
		t.Fatalf("result = %+v, want zero result", got)
	}
	if _, err := f.repo.schedules[f.schedule.ID]; err || len(f.repo.schedules) != 0 || len(f.repo.recordedAt) != 0 {
		t.Fatalf("failed transaction published state: schedules=%v recorded=%v", f.repo.schedules, f.repo.recordedAt)
	}
}

func TestServicePutScheduleRejectsRevisionOverflow(t *testing.T) {
	f := newServiceFixture()
	f.repo.schedules[f.schedule.ID] = Schedule{Account: f.schedule.Account, ID: f.schedule.ID, Subscription: f.schedule.Subscription, Assignment: f.schedule.Assignment, Anchor: f.schedule.Anchor, State: ScheduleActive, StateEffectiveAt: f.schedule.StateEffectiveAt, Revision: math.MaxInt64, SourceID: f.schedule.SourceID}
	f.schedule.State = SchedulePaused
	if _, err := f.service.PutSchedule(t.Context(), f.schedule, math.MaxInt64); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow error = %v, want overflow", err)
	}
}

func TestServicePutScheduleHonorsCanceledContext(t *testing.T) {
	f := newServiceFixture()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := f.service.PutSchedule(ctx, f.schedule, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if got != (Schedule{}) || len(f.repo.schedules) != 0 {
		t.Fatalf("canceled call changed state: result=%+v schedules=%v", got, f.repo.schedules)
	}
}
