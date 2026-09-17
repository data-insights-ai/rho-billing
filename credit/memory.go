package credit

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type memoryRepository struct {
	mu       sync.Mutex
	unitsMu  sync.Mutex
	accounts map[billing.AccountID]*memoryAccount
	units    map[string]int64
}
type memoryAccount struct {
	mu    sync.Mutex
	state memoryState
}
type lifetimeKey struct{ unit, scope string }

type memoryState struct {
	lots            map[string]Lot
	liveAvailable   map[string]struct{}
	accountLifetime map[string]Balance
	scopeLifetime   map[lifetimeKey]Balance
	reservations    map[string]Reservation
	limits          []Limit
	outcomes        map[billing.OperationID]Outcome
	entries         []Entry
}

func NewMemoryRepository(accounts ...billing.AccountID) Repository {
	r := &memoryRepository{accounts: make(map[billing.AccountID]*memoryAccount), units: make(map[string]int64)}
	for _, a := range accounts {
		if !billing.ValidID(string(a)) {
			panic("credit: invalid test account")
		}
		r.accounts[a] = &memoryAccount{state: newMemoryState()}
	}
	return r
}
func newMemoryState() memoryState {
	return memoryState{
		lots:            map[string]Lot{},
		liveAvailable:   map[string]struct{}{},
		accountLifetime: map[string]Balance{},
		scopeLifetime:   map[lifetimeKey]Balance{},
		reservations:    map[string]Reservation{},
		outcomes:        map[billing.OperationID]Outcome{},
	}
}
func cloneReservation(r Reservation) Reservation {
	r.Allocations = slices.Clone(r.Allocations)
	r.Evidence.Metrics = slices.Clone(r.Evidence.Metrics)
	return r
}
func (s memoryState) clone() memoryState {
	out := newMemoryState()
	for k, v := range s.lots {
		out.lots[k] = v
	}
	maps.Copy(out.liveAvailable, s.liveAvailable)
	maps.Copy(out.accountLifetime, s.accountLifetime)
	maps.Copy(out.scopeLifetime, s.scopeLifetime)
	for k, v := range s.reservations {
		out.reservations[k] = cloneReservation(v)
	}
	for k, v := range s.outcomes {
		out.outcomes[k] = v
	}
	out.limits = slices.Clone(s.limits)
	out.entries = slices.Clone(s.entries)
	return out
}
func (r *memoryRepository) account(id billing.AccountID) (*memoryAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accounts[id]
	if !ok {
		return nil, billing.ErrNotFound
	}
	return a, nil
}
func (r *memoryRepository) WithinAccount(ctx context.Context, id billing.AccountID, fn func(Tx) error) error {
	a, err := r.account(id)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// The unit registry is global. Hold its mutex across the callback and
	// commit so a rejected concurrent scale cannot lose its terminal outcome
	// after observing a stale registry snapshot.
	r.unitsMu.Lock()
	defer r.unitsMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	copy := a.state.clone()
	r.mu.Lock()
	units := make(map[string]int64, len(r.units))
	for code, scale := range r.units {
		units[code] = scale
	}
	r.mu.Unlock()
	tx := &memoryTx{s: &copy, units: units}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	for code, scale := range tx.units {
		if old, ok := r.units[code]; ok && old != scale {
			r.mu.Unlock()
			return billing.ErrConflict
		}
	}
	for code, scale := range tx.units {
		r.units[code] = scale
	}
	r.mu.Unlock()
	a.state = copy
	return nil
}
func (r *memoryRepository) History(ctx context.Context, id billing.AccountID, after int64, limit int) ([]Entry, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	a, err := r.account(id)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range a.state.entries {
		if e.Sequence > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

type memoryTx struct {
	s     *memoryState
	units map[string]int64
}

func (t *memoryTx) Lots() ([]Lot, error) {
	var out []Lot
	for _, v := range t.s.lots {
		out = append(out, v)
	}
	return out, nil
}
func (t *memoryTx) LiveLots() ([]Lot, error) {
	var out []Lot
	for _, l := range t.s.lots {
		if l.Available > 0 || l.Held > 0 || l.PendingRevocation > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}
func (t *memoryTx) Lot(id string) (Lot, bool, error) { l, ok := t.s.lots[id]; return l, ok, nil }
func (t *memoryTx) LotsByID(ids []string) ([]Lot, error) {
	if len(ids) > reservationSweepLimit {
		return nil, ErrAllocationBudget
	}
	out := make([]Lot, 0, len(ids))
	for _, id := range ids {
		if l, ok := t.s.lots[id]; ok {
			out = append(out, l)
		}
	}
	return out, nil
}
func (t *memoryTx) LotBySource(unit, source, reference string) (Lot, bool, error) {
	for _, l := range t.s.lots {
		if l.Unit.Code == unit && l.Source == source && l.SourceRef == reference {
			return l, true, nil
		}
	}
	return Lot{}, false, nil
}
func (t *memoryTx) StoredBalance(unit, scope string, at time.Time) (Balance, error) {
	if at.IsZero() {
		return Balance{}, billing.ErrInvalid
	}
	b, err := t.lifetimeBalance(unit, scope)
	if err != nil {
		return Balance{}, err
	}
	for id := range t.s.liveAvailable {
		l := t.s.lots[id]
		if l.Unit.Code != unit || (scope != "" && l.Scope != "" && l.Scope != scope) {
			continue
		}
		if at.Before(l.ValidFrom) || expired(l, at) || !l.RevokedAt.IsZero() {
			continue
		}
		b.Available, err = checked.Add(b.Available, l.Available)
		if err != nil {
			return Balance{}, err
		}
	}
	return b, nil
}

func (t *memoryTx) lifetimeBalance(unit, scope string) (Balance, error) {
	if scope == "" {
		b := t.s.accountLifetime[unit]
		b.Available = 0
		return b, nil
	}
	unscoped := t.s.scopeLifetime[lifetimeKey{unit: unit, scope: ""}]
	scoped := t.s.scopeLifetime[lifetimeKey{unit: unit, scope: scope}]
	var b Balance
	var err error
	b.Held, err = checked.Add(unscoped.Held, scoped.Held)
	if err != nil {
		return Balance{}, err
	}
	b.Consumed, err = checked.Add(unscoped.Consumed, scoped.Consumed)
	if err != nil {
		return Balance{}, err
	}
	b.Expired, err = checked.Add(unscoped.Expired, scoped.Expired)
	if err != nil {
		return Balance{}, err
	}
	b.Revoked, err = checked.Add(unscoped.Revoked, scoped.Revoked)
	if err != nil {
		return Balance{}, err
	}
	return b, nil
}

func (t *memoryTx) DueLots(at time.Time, limit int) ([]Lot, error) {
	if at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	rows := make([]Lot, 0)
	for _, l := range t.s.lots {
		if l.Available > 0 && expired(l, at) {
			rows = append(rows, l)
		}
	}
	slices.SortFunc(rows, func(a, b Lot) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows[:min(limit, len(rows))], nil
}

func (t *memoryTx) EligibleLots(unit, scope string, at time.Time, limit int) ([]Lot, error) {
	if unit == "" || at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	rows := make([]Lot, 0)
	for _, l := range t.s.lots {
		if l.Unit.Code == unit && (scope == "" || l.Scope == "" || l.Scope == scope) && l.Available > 0 && !at.Before(l.ValidFrom) && !expired(l, at) && l.RevokedAt.IsZero() {
			rows = append(rows, l)
		}
	}
	slices.SortFunc(rows, func(a, b Lot) int {
		if a.ExpiresAt.IsZero() != b.ExpiresAt.IsZero() {
			if a.ExpiresAt.IsZero() {
				return 1
			}
			return -1
		}
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		return cmp.Or(a.GrantedAt.Compare(b.GrantedAt), cmp.Compare(a.ID, b.ID))
	})
	return rows[:min(limit, len(rows))], nil
}

func (t *memoryTx) ActiveReservations() ([]Reservation, error) {
	var out []Reservation
	for _, v := range t.s.reservations {
		if v.State == "held" {
			out = append(out, cloneReservation(v))
		}
	}
	return out, nil
}
func (t *memoryTx) EnsureUnit(unit billing.Unit) error {
	if !unit.Valid() {
		return billing.ErrInvalid
	}
	if old, ok := t.units[unit.Code]; ok && old != unit.Scale {
		return billing.ErrConflict
	}
	t.units[unit.Code] = unit.Scale
	return nil
}
func (t *memoryTx) PutLot(v Lot) error {
	if err := t.EnsureUnit(v.Unit); err != nil {
		return err
	}
	if old, ok := t.s.lots[v.ID]; ok {
		if err := t.applyLotLifetime(old, -1); err != nil {
			return err
		}
	}
	t.s.lots[v.ID] = v
	if v.Available > 0 {
		t.s.liveAvailable[v.ID] = struct{}{}
	} else {
		delete(t.s.liveAvailable, v.ID)
	}
	return t.applyLotLifetime(v, 1)
}

func (t *memoryTx) applyLotLifetime(lot Lot, sign int64) error {
	if err := applyLifetime(&t.s.accountLifetime, lot.Unit.Code, lot, sign); err != nil {
		return err
	}
	return applyLifetimeKey(&t.s.scopeLifetime, lifetimeKey{unit: lot.Unit.Code, scope: lot.Scope}, lot, sign)
}

func applyLifetime(totals *map[string]Balance, unit string, lot Lot, sign int64) error {
	next, err := signedLifetime((*totals)[unit], lot, sign)
	if err != nil {
		return err
	}
	(*totals)[unit] = next
	return nil
}

func applyLifetimeKey(totals *map[lifetimeKey]Balance, key lifetimeKey, lot Lot, sign int64) error {
	next, err := signedLifetime((*totals)[key], lot, sign)
	if err != nil {
		return err
	}
	(*totals)[key] = next
	return nil
}

func signedLifetime(b Balance, lot Lot, sign int64) (Balance, error) {
	b.Available = 0
	for _, field := range []struct {
		dst    *int64
		amount int64
	}{{&b.Held, lot.Held}, {&b.Consumed, lot.Consumed}, {&b.Expired, lot.Expired}, {&b.Revoked, lot.Revoked}} {
		if sign < 0 {
			if *field.dst < field.amount {
				return Balance{}, billing.ErrState
			}
			*field.dst -= field.amount
			continue
		}
		value, err := checked.Add(*field.dst, field.amount)
		if err != nil {
			return Balance{}, err
		}
		*field.dst = value
	}
	return b, nil
}
func (t *memoryTx) Reservations() ([]Reservation, error) {
	var out []Reservation
	for _, v := range t.s.reservations {
		out = append(out, cloneReservation(v))
	}
	return out, nil
}
func (t *memoryTx) Reservation(id string) (Reservation, bool, error) {
	v, ok := t.s.reservations[id]
	if !ok {
		return Reservation{}, false, nil
	}
	return cloneReservation(v), true, nil
}
func (t *memoryTx) PutReservation(v Reservation) error {
	t.s.reservations[v.ID] = cloneReservation(v)
	return nil
}
func (t *memoryTx) HasUsage(usageID string) (bool, error) {
	for _, reservation := range t.s.reservations {
		if reservation.Evidence.UsageID == usageID {
			return true, nil
		}
	}
	return false, nil
}
func (t *memoryTx) SettledForLimit(actor, unit string, period billing.Period) (int64, error) {
	var total int64
	for _, reservation := range t.s.reservations {
		if reservation.State != "settled" || reservation.Actor != actor || reservation.Unit != unit || !period.Contains(reservation.CreatedAt) {
			continue
		}
		var err error
		total, err = checked.Add(total, reservation.Consumed)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
func (t *memoryTx) Limits() ([]Limit, error) { return slices.Clone(t.s.limits), nil }
func (t *memoryTx) LimitAt(actor, unit string, at time.Time) (Limit, bool, error) {
	var found Limit
	for _, l := range t.s.limits {
		if l.Actor == actor && l.Unit == unit && l.Period.Contains(at) {
			if found.Actor != "" {
				return Limit{}, false, billing.ErrConflict
			}
			found = l
		}
	}
	return found, found.Actor != "", nil
}
func (t *memoryTx) LimitsOverlapping(actor, unit string, period billing.Period) ([]Limit, error) {
	rows := make([]Limit, 0, 2)
	for _, l := range t.s.limits {
		if l.Actor == actor && l.Unit == unit && l.Period.Start.Before(period.End) && period.Start.Before(l.Period.End) {
			rows = append(rows, l)
			if len(rows) > 2 {
				return nil, billing.ErrConflict
			}
		}
	}
	return rows, nil
}
func (t *memoryTx) PutLimit(v Limit) error {
	for i, old := range t.s.limits {
		if old.Actor == v.Actor && old.Unit == v.Unit && old.Period.Equal(v.Period) {
			t.s.limits[i] = v
			return nil
		}
	}
	t.s.limits = append(t.s.limits, v)
	return nil
}
func (t *memoryTx) Outcome(id billing.OperationID) (Outcome, bool, error) {
	v, ok := t.s.outcomes[id]
	return v, ok, nil
}
func (t *memoryTx) PutOutcome(id billing.OperationID, v Outcome) error {
	if _, ok := t.s.outcomes[id]; ok {
		return billing.ErrConflict
	}
	t.s.outcomes[id] = v
	return nil
}
func (t *memoryTx) Append(e Entry) error { return t.AppendEntries([]Entry{e}) }

func (t *memoryTx) AppendEntries(entries []Entry) error {
	for i, e := range entries {
		if !billing.ValidID(string(e.OperationID)) || e.Kind == "" {
			return billing.ErrInvalid
		}
		sequence, err := checked.Add(int64(len(t.s.entries)), int64(i)+1)
		if err != nil {
			return err
		}
		if e.Sequence != 0 && e.Sequence != sequence {
			return billing.ErrConflict
		}
	}
	for _, e := range entries {
		e.Sequence = int64(len(t.s.entries)) + 1
		e.RecordedAt = billing.CanonicalTime(e.RecordedAt)
		e.EffectiveAt = billing.CanonicalTime(e.EffectiveAt)
		t.s.entries = append(t.s.entries, e)
	}
	return nil
}

func (t *memoryTx) History(after int64, limit int) ([]Entry, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	out := make([]Entry, 0, limit)
	for _, entry := range t.s.entries {
		if entry.Sequence > after {
			out = append(out, entry)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (t *memoryTx) DueReservations(at time.Time, limit int) ([]Reservation, error) {
	if at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	var rows []Reservation
	for _, r := range t.s.reservations {
		if r.State == "held" && !at.Before(r.Deadline) {
			rows = append(rows, cloneReservation(r))
		}
	}
	slices.SortFunc(rows, func(a, b Reservation) int {
		if c := a.Deadline.Compare(b.Deadline); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows[:min(limit, len(rows))], nil
}
func (t *memoryTx) CommittedForLimit(actor, unit string, period billing.Period, at time.Time) (int64, error) {
	var total int64
	for _, r := range t.s.reservations {
		if r.Actor != actor || r.Unit != unit || !period.Contains(r.CreatedAt) {
			continue
		}
		var err error
		total, err = checked.Add(total, reservationExposure(r, at))
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
