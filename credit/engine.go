package credit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type Engine struct {
	repo Repository
	now  func() time.Time
}

func New(repo Repository, now func() time.Time) *Engine {
	if repo == nil {
		panic("credit: nil repository")
	}
	if now == nil {
		now = time.Now
	}
	return &Engine{repo, now}
}

type state struct {
	lots                 map[string]Lot
	originalLots         map[string]Lot
	lotLookup            func(string) (Lot, bool, error)
	lotsByID             func([]string) ([]Lot, error)
	sourceLookup         func(string, string, string) (Lot, bool, error)
	storedBalance        func(string, string) (Balance, error)
	eligibleLots         func(string, string, time.Time, int) ([]Lot, error)
	originalReservations map[string]Reservation
	maintenanceMore      bool
	reservations         map[string]Reservation
	limits               []Limit
	entries              []Entry
	lotWrites            map[string]Lot
	reservationWrites    map[string]Reservation
	limitWrites          []Limit
	now                  time.Time
	op                   billing.OperationID
	reservationLookup    func(string) (Reservation, bool, error)
	usageExists          func(string) (bool, error)
	committedForLimit    func(string, string, billing.Period, time.Time) (int64, error)
	limitAt              func(string, string, time.Time) (Limit, bool, error)
	limitsOverlapping    func(string, string, billing.Period) ([]Limit, error)
	ensureUnit           func(billing.Unit) error
}

func load(tx Tx, now time.Time, op billing.OperationID) (*state, error) {
	s, err := loadMaintenance(tx, now, op)
	if err != nil {
		return nil, err
	}
	if s.maintenanceMore {
		return nil, ErrMaintenanceRequired
	}
	return s, nil
}

func loadMaintenance(tx Tx, now time.Time, op billing.OperationID) (*state, error) {
	s := &state{lots: map[string]Lot{}, originalLots: map[string]Lot{}, lotLookup: tx.Lot, lotsByID: tx.LotsByID, sourceLookup: tx.LotBySource, storedBalance: func(unit, scope string) (Balance, error) { return tx.StoredBalance(unit, scope, now) }, eligibleLots: tx.EligibleLots, limitAt: tx.LimitAt, limitsOverlapping: tx.LimitsOverlapping, reservations: map[string]Reservation{}, originalReservations: map[string]Reservation{}, lotWrites: map[string]Lot{}, reservationWrites: map[string]Reservation{}, now: now, op: op, reservationLookup: tx.Reservation, usageExists: tx.HasUsage, committedForLimit: tx.CommittedForLimit, ensureUnit: tx.EnsureUnit}
	lots, err := tx.DueLots(now, reservationSweepLimit+1)
	if err != nil {
		return nil, err
	}
	if len(lots) > reservationSweepLimit {
		s.maintenanceMore = true
		lots = lots[:reservationSweepLimit]
	}
	for _, l := range lots {
		s.lots[l.ID] = l
		s.originalLots[l.ID] = l
	}
	rs, err := tx.DueReservations(now, reservationSweepLimit+1)
	if err != nil {
		return nil, err
	}
	if len(rs) > reservationSweepLimit {
		s.maintenanceMore = true
		rs = rs[:reservationSweepLimit]
	}
	for _, r := range rs {
		s.reservations[r.ID] = r
		s.originalReservations[r.ID] = cloneReservation(r)
	}
	return s, err
}
func (s *state) copy() *state {
	c := *s
	c.originalLots = maps.Clone(s.originalLots)
	c.originalReservations = maps.Clone(s.originalReservations)
	c.lots = map[string]Lot{}
	for k, v := range s.lots {
		c.lots[k] = v
	}
	c.reservations = map[string]Reservation{}
	for k, v := range s.reservations {
		c.reservations[k] = cloneReservation(v)
	}
	c.lotWrites = map[string]Lot{}
	for k, v := range s.lotWrites {
		c.lotWrites[k] = v
	}
	c.reservationWrites = map[string]Reservation{}
	for k, v := range s.reservationWrites {
		c.reservationWrites[k] = cloneReservation(v)
	}
	c.limits = slices.Clone(s.limits)
	c.limitWrites = slices.Clone(s.limitWrites)
	c.entries = slices.Clone(s.entries)
	return &c
}
func (s *state) lot(id string) (Lot, bool, error) {
	if l, ok := s.lots[id]; ok {
		return l, true, nil
	}
	l, ok, err := s.lotLookup(id)
	if err == nil && ok {
		s.lots[id] = l
		s.originalLots[id] = l
	}
	return l, ok, err
}
func (s *state) saveLot(l Lot) { s.lots[l.ID] = l; s.lotWrites[l.ID] = l }
func (s *state) saveReservation(r Reservation) {
	s.reservations[r.ID] = r
	s.reservationWrites[r.ID] = r
}
func (s *state) reservation(id string) (Reservation, bool, error) {
	if reservation, ok := s.reservations[id]; ok {
		if err := s.loadReservationLots(reservation); err != nil {
			return Reservation{}, false, err
		}
		return cloneReservation(reservation), true, nil
	}
	if s.reservationLookup == nil {
		return Reservation{}, false, nil
	}
	r, ok, err := s.reservationLookup(id)
	if err == nil && ok {
		s.reservations[id] = cloneReservation(r)
		s.originalReservations[id] = cloneReservation(r)
		if loadErr := s.loadReservationLots(r); loadErr != nil {
			return Reservation{}, false, loadErr
		}
	}
	return r, ok, err
}

func (s *state) loadReservationLots(r Reservation) error {
	ids := make([]string, 0, len(r.Allocations))
	seen := make(map[string]struct{}, len(r.Allocations))
	for _, allocation := range r.Allocations {
		if _, loaded := s.lots[allocation.LotID]; loaded {
			continue
		}
		if _, ok := seen[allocation.LotID]; ok {
			continue
		}
		seen[allocation.LotID] = struct{}{}
		ids = append(ids, allocation.LotID)
	}
	for len(ids) > 0 {
		pageSize := min(len(ids), reservationSweepLimit)
		page, err := s.lotsByID(ids[:pageSize])
		if err != nil {
			return err
		}
		if len(page) != pageSize {
			return billing.ErrNotFound
		}
		for _, lot := range page {
			s.lots[lot.ID] = lot
			s.originalLots[lot.ID] = lot
		}
		ids = ids[pageSize:]
	}
	return nil
}
func (s *state) flush(tx Tx) error {
	for _, id := range sortedKeys(s.lotWrites) {
		if err := tx.PutLot(s.lotWrites[id]); err != nil {
			return err
		}
	}
	for _, id := range sortedKeys(s.reservationWrites) {
		if err := tx.PutReservation(s.reservationWrites[id]); err != nil {
			return err
		}
	}
	for _, v := range s.limitWrites {
		if err := tx.PutLimit(v); err != nil {
			return err
		}
	}
	return tx.AppendEntries(s.entries)
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// A persisted projection read failure is an operational failure, never a
// terminal rejection of the caller's financial command. Keep its underlying
// error available to the host without recording an idempotent rejection.
type projectionReadError struct{ cause error }

func (e *projectionReadError) Error() string {
	return "credit: read balance projection: " + e.cause.Error()
}
func (e *projectionReadError) Unwrap() error { return e.cause }

func errorCode(err error) string {
	if _, ok := errors.AsType[*projectionReadError](err); ok {
		return ""
	}
	for _, item := range []struct {
		err  error
		code string
	}{{billing.ErrInvalid, "invalid"}, {billing.ErrConflict, "conflict"}, {billing.ErrNotFound, "not_found"}, {billing.ErrInsufficient, "insufficient"}, {billing.ErrLimit, "limit"}, {billing.ErrState, "state"}, {billing.ErrExpired, "expired"}, {billing.ErrOverflow, "overflow"}} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return ""
}
func resultError(code string) error {
	switch code {
	case "":
		return nil
	case "invalid":
		return billing.ErrInvalid
	case "conflict":
		return billing.ErrConflict
	case "not_found":
		return billing.ErrNotFound
	case "insufficient":
		return billing.ErrInsufficient
	case "limit":
		return billing.ErrLimit
	case "state":
		return billing.ErrState
	case "expired":
		return billing.ErrExpired
	case "overflow":
		return billing.ErrOverflow
	default:
		return fmt.Errorf("credit: corrupt operation outcome %q", code)
	}
}

func (e *Engine) run(ctx context.Context, account billing.AccountID, op billing.OperationID, fp string, action func(*state) (Result, error)) (Result, error) {
	if !billing.ValidID(string(account)) || !billing.ValidID(string(op)) {
		return Result{}, billing.ErrInvalid
	}
	var out Outcome
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		old, ok, err := tx.Outcome(op)
		if err != nil {
			return err
		}
		if ok {
			if old.Fingerprint != fp {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		s, err := load(tx, billing.CanonicalTime(e.now()), op)
		if err != nil {
			return err
		}
		if err = s.sweep(); err != nil {
			return err
		}
		candidate := s.copy()
		res, actionErr := action(candidate)
		if actionErr == nil {
			s = candidate
		} else if errorCode(actionErr) == "" {
			return actionErr
		}
		if err = s.flush(tx); err != nil {
			return err
		}
		out = Outcome{Fingerprint: fp, Error: errorCode(actionErr), Result: res}
		return tx.PutOutcome(op, out)
	})
	if err != nil {
		return Result{}, err
	}
	if err := resultError(out.Error); err != nil {
		return out.Result, &Rejection{Cause: err}
	}
	return out.Result, nil
}

func (s *state) change(id, kind, reservation, reason string, a, h, c, x, r int64, effective time.Time) error {
	l, ok := s.lots[id]
	if !ok {
		return billing.ErrNotFound
	}
	fields := []*int64{&l.Available, &l.Held, &l.Consumed, &l.Expired, &l.Revoked}
	delta := []int64{a, h, c, x, r}
	var total int64
	for i, p := range fields {
		v, err := checked.Add(*p, delta[i])
		if err != nil || v < 0 {
			return fmt.Errorf("credit: invalid ledger projection for %s", id)
		}
		*p = v
		total, err = checked.Add(total, v)
		if err != nil {
			return err
		}
	}
	if total != l.Initial {
		return fmt.Errorf("credit: conservation violation for %s", id)
	}
	s.saveLot(l)
	s.entries = append(s.entries, Entry{OperationID: s.op, LotID: id, ReservationID: reservation, Kind: kind, Reason: reason, RecordedAt: s.now, EffectiveAt: effective, Available: a, Held: h, Consumed: c, Expired: x, Revoked: r})
	return nil
}
func expired(l Lot, at time.Time) bool { return !l.ExpiresAt.IsZero() && !at.Before(l.ExpiresAt) }
func (s *state) sweep() error {
	for _, id := range sortedKeys(s.reservations) {
		r := s.reservations[id]
		if r.State == "held" && !s.now.Before(r.Deadline) {
			if err := s.finish(r, 0, "timed_out", "authorization deadline", Evidence{}); err != nil {
				return err
			}
		}
	}
	for _, id := range sortedKeys(s.lots) {
		l := s.lots[id]
		if l.Available > 0 && expired(l, s.now) {
			if err := s.change(l.ID, "expiry", "", "lot expired", -l.Available, 0, 0, l.Available, 0, l.ExpiresAt); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *state) balance(unit, scope string) (Balance, error) {
	b, err := s.storedBalance(unit, scope)
	if err != nil {
		return Balance{}, &projectionReadError{cause: err}
	}
	matches := func(l Lot) bool { return l.Unit.Code == unit && (scope == "" || l.Scope == "" || l.Scope == scope) }
	// Subtract every loaded lot's original lifetime contribution before adding
	// current values to avoid transient overflow when quantities move between lots.
	// Retired, unloaded history remains in the persisted projection.
	for _, l := range s.originalLots {
		if !matches(l) {
			continue
		}
		available := l.Available
		if s.now.Before(l.ValidFrom) || expired(l, s.now) || !l.RevokedAt.IsZero() {
			available = 0
		}
		for _, field := range []struct {
			dst    *int64
			amount int64
		}{{&b.Available, available}, {&b.Held, l.Held}, {&b.Consumed, l.Consumed}, {&b.Expired, l.Expired}, {&b.Revoked, l.Revoked}} {
			if field.amount < 0 || *field.dst < field.amount {
				return Balance{}, errors.New("credit: inconsistent stored balance projection")
			}
			*field.dst -= field.amount
		}
	}
	for _, l := range s.lots {
		if !matches(l) {
			continue
		}
		available := l.Available
		if s.now.Before(l.ValidFrom) || expired(l, s.now) || !l.RevokedAt.IsZero() {
			available = 0
		}
		for _, field := range []struct {
			dst    *int64
			amount int64
		}{{&b.Available, available}, {&b.Held, l.Held}, {&b.Consumed, l.Consumed}, {&b.Expired, l.Expired}, {&b.Revoked, l.Revoked}} {
			value, err := checked.Add(*field.dst, field.amount)
			if err != nil {
				return Balance{}, err
			}
			*field.dst = value
		}
	}
	return b, nil
}
func (s *state) result(unit, scope string) (Result, error) {
	b, err := s.balance(unit, scope)
	return Result{Balance: b}, err
}

func (e *Engine) Grant(ctx context.Context, in GrantInput) (Result, error) {
	in.ValidFrom = billing.CanonicalTime(in.ValidFrom)
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	fp := identity.Fingerprint("grant", in.LotID, in.Unit.Code, strconv.FormatInt(in.Unit.Scale, 10), strconv.FormatInt(in.Amount, 10), in.Scope, in.Source, in.SourceRef, identity.Instant(in.ValidFrom), identity.Instant(in.ExpiresAt))
	return e.run(ctx, in.Account, in.Operation, fp, func(s *state) (Result, error) {
		if !billing.ValidID(in.LotID) || !in.Unit.Valid() || in.Amount <= 0 || !billing.ValidID(in.Source) || !billing.ValidID(in.SourceRef) || in.ValidFrom.IsZero() || (!in.ExpiresAt.IsZero() && !in.ExpiresAt.After(in.ValidFrom)) {
			return Result{}, billing.ErrInvalid
		}
		if _, ok, err := s.lot(in.LotID); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		if _, ok, err := s.sourceLookup(in.Unit.Code, in.Source, in.SourceRef); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		if s.ensureUnit != nil {
			if err := s.ensureUnit(in.Unit); err != nil {
				return Result{}, err
			}
		}
		l := Lot{ID: in.LotID, Unit: in.Unit, Scope: in.Scope, Source: in.Source, SourceRef: in.SourceRef, ValidFrom: in.ValidFrom.UTC(), ExpiresAt: in.ExpiresAt.UTC(), GrantedAt: s.now, Initial: in.Amount, Available: in.Amount}
		s.saveLot(l)
		s.entries = append(s.entries, Entry{OperationID: s.op, LotID: l.ID, Kind: "grant", RecordedAt: s.now, EffectiveAt: l.ValidFrom, Available: l.Initial})
		if err := s.sweep(); err != nil {
			return Result{}, err
		}
		out, err := s.result(in.Unit.Code, in.Scope)
		out.LotID = l.ID
		return out, err
	})
}

func (s *state) allocate(unit, scope string, amount int64) ([]Allocation, error) {
	pool, err := s.eligibleLots(unit, scope, s.now, reservationSweepLimit+1)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(pool))
	for _, l := range pool {
		seen[l.ID] = struct{}{}
	}
	for i, l := range pool {
		if i >= reservationSweepLimit {
			break
		}
		if _, loaded := s.lots[l.ID]; !loaded {
			s.lots[l.ID] = l
			s.originalLots[l.ID] = l
		}
	}
	for _, l := range s.lots {
		if _, ok := seen[l.ID]; ok || l.Unit.Code != unit || (scope != "" && l.Scope != "" && l.Scope != scope) || l.Available <= 0 || s.now.Before(l.ValidFrom) || expired(l, s.now) || !l.RevokedAt.IsZero() {
			continue
		}
		pool = append(pool, l)
	}
	slices.SortFunc(pool, func(a, b Lot) int {
		if a.ExpiresAt.IsZero() != b.ExpiresAt.IsZero() {
			if a.ExpiresAt.IsZero() {
				return 1
			}
			return -1
		}
		return cmp.Or(a.ExpiresAt.Compare(b.ExpiresAt), a.GrantedAt.Compare(b.GrantedAt), cmp.Compare(a.ID, b.ID))
	})
	if len(pool) > reservationSweepLimit+1 {
		pool = pool[:reservationSweepLimit+1]
	}
	var out []Allocation
	for i, l := range pool {
		if i >= reservationSweepLimit {
			break
		}
		if current, ok := s.lots[l.ID]; ok {
			l = current
		} else {
			s.lots[l.ID] = l
			s.originalLots[l.ID] = l
		}
		take := min(amount, l.Available)
		if take > 0 {
			out = append(out, Allocation{l.ID, take})
			amount -= take
		}
		if amount == 0 {
			break
		}
	}
	if amount > 0 && len(pool) > reservationSweepLimit {
		return nil, ErrAllocationBudget
	}
	if amount > 0 {
		return nil, billing.ErrInsufficient
	}
	return out, nil
}
func reservationExposure(r Reservation, at time.Time) int64 {
	if r.State == "settled" {
		return r.Consumed
	}
	if r.State == "held" && at.Before(r.Deadline) {
		return r.Authorized
	}
	return 0
}
func (s *state) committed(actor, unit string, period billing.Period) (int64, error) {
	total, err := s.committedForLimit(actor, unit, period, s.now)
	if err != nil {
		return 0, &projectionReadError{cause: err}
	}
	matches := func(r Reservation) bool { return r.Actor == actor && r.Unit == unit && period.Contains(r.CreatedAt) }
	for _, r := range s.originalReservations {
		if matches(r) {
			amount := reservationExposure(r, s.now)
			if amount < 0 || total < amount {
				return 0, errors.New("credit: inconsistent reservation exposure")
			}
			total -= amount
		}
	}
	for _, r := range s.reservations {
		if !matches(r) {
			continue
		}
		total, err = checked.Add(total, reservationExposure(r, s.now))
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
func (s *state) limit(actor, unit string, at time.Time, additional int64) (billing.Period, error) {
	l, ok, err := s.limitAt(actor, unit, at)
	if err != nil {
		return billing.Period{}, &projectionReadError{cause: err}
	}
	if !ok {
		return billing.Period{}, nil
	}
	used, err := s.committed(actor, unit, l.Period)
	if err != nil {
		return billing.Period{}, err
	}
	next, err := checked.Add(used, additional)
	if err != nil {
		return billing.Period{}, err
	}
	if next > l.Amount {
		return billing.Period{}, billing.ErrLimit
	}
	return l.Period, nil
}
func (e *Engine) Reserve(ctx context.Context, in ReserveInput) (Result, error) {
	in.Deadline = billing.CanonicalTime(in.Deadline)
	fp := identity.Fingerprint("reserve", in.ReservationID, in.Actor, in.Unit, in.Scope, strconv.FormatInt(in.Amount, 10), identity.Instant(in.Deadline))
	return e.run(ctx, in.Account, in.Operation, fp, func(s *state) (Result, error) {
		if !billing.ValidID(in.ReservationID) || !billing.ValidID(in.Unit) || in.Amount <= 0 || !in.Deadline.After(s.now) {
			return Result{}, billing.ErrInvalid
		}
		if _, ok := s.reservations[in.ReservationID]; ok {
			return Result{}, billing.ErrConflict
		}
		if _, found, err := s.reservation(in.ReservationID); err != nil {
			return Result{}, err
		} else if found {
			return Result{}, billing.ErrConflict
		}
		period, err := s.limit(in.Actor, in.Unit, s.now, in.Amount)
		if err != nil {
			return Result{}, err
		}
		allocations, err := s.allocate(in.Unit, in.Scope, in.Amount)
		if err != nil {
			return Result{}, err
		}
		r := Reservation{ID: in.ReservationID, Actor: in.Actor, Unit: in.Unit, Scope: in.Scope, CreatedAt: s.now, Deadline: in.Deadline.UTC(), State: "held", Authorized: in.Amount, Allocations: allocations, LimitPeriod: period}
		for _, a := range allocations {
			if err = s.change(a.LotID, "reserve", r.ID, "", -a.Amount, a.Amount, 0, 0, 0, s.now); err != nil {
				return Result{}, err
			}
		}
		s.saveReservation(r)
		out, err := s.result(in.Unit, in.Scope)
		out.ReservationID = r.ID
		return out, err
	})
}
func (e *Engine) Extend(ctx context.Context, in ExtendInput) (Result, error) {
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint("extend", in.ReservationID, strconv.FormatInt(in.Additional, 10)), func(s *state) (Result, error) {
		r, ok, err := s.reservation(in.ReservationID)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if _, active := s.reservations[in.ReservationID]; !active || r.State != "held" {
			return Result{}, billing.ErrState
		}
		if in.Additional <= 0 {
			return Result{}, billing.ErrInvalid
		}
		next, err := checked.Add(r.Authorized, in.Additional)
		if err != nil {
			return Result{}, err
		}
		// The extension is authorized now, so it is bounded by the limit in
		// force now. Evaluating as of r.CreatedAt let a cap introduced after
		// the reservation be bypassed entirely, and the resulting period was
		// discarded so the row could not be reconciled afterwards either.
		period, err := s.limit(r.Actor, r.Unit, s.now, in.Additional)
		if err != nil {
			return Result{}, err
		}
		if period.Valid() {
			r.LimitPeriod = period
		}
		as, err := s.allocate(r.Unit, r.Scope, in.Additional)
		if err != nil {
			return Result{}, err
		}
		if len(r.Allocations)+len(as) > reservationSweepLimit {
			return Result{}, ErrAllocationBudget
		}
		for _, a := range as {
			if err = s.change(a.LotID, "reserve", r.ID, "extend", -a.Amount, a.Amount, 0, 0, 0, s.now); err != nil {
				return Result{}, err
			}
		}
		r.Authorized = next
		r.Allocations = append(r.Allocations, as...)
		s.saveReservation(r)
		out, err := s.result(r.Unit, r.Scope)
		out.ReservationID = r.ID
		return out, err
	})
}

func (s *state) finish(r Reservation, actual int64, next ReservationState, reason string, evidence Evidence) error {
	if err := s.loadReservationLots(r); err != nil {
		return err
	}
	remaining := actual
	for _, a := range r.Allocations {
		l, ok, err := s.lot(a.LotID)
		if err != nil {
			return err
		}
		if !ok {
			return billing.ErrNotFound
		}
		consume := min(remaining, a.Amount)
		release := a.Amount - consume
		remaining -= consume
		if consume > 0 {
			pending := min(l.PendingRevocation, consume)
			l.PendingRevocation -= pending
			s.saveLot(l)
			if err := s.change(a.LotID, "consume", r.ID, reason, 0, -consume, consume, 0, 0, s.now); err != nil {
				return err
			}
			s.entries[len(s.entries)-1].PendingRevocation = -pending
		}
		if release > 0 {
			l, ok, err = s.lot(a.LotID)
			if err != nil {
				return err
			}
			if !ok {
				return billing.ErrNotFound
			}
			pending := min(l.PendingRevocation, release)
			l.PendingRevocation -= pending
			s.saveLot(l)
			var available, expiredAmount, revoked int64
			kind := "release"
			if !l.RevokedAt.IsZero() {
				revoked = release
				kind = "release_revoked"
			} else if expired(l, s.now) {
				revoked = pending
				expiredAmount = release - pending
				kind = "release_expired"
			} else {
				revoked = pending
				available = release - pending
			}
			if err := s.change(a.LotID, kind, r.ID, reason, available, -release, 0, expiredAmount, revoked, s.now); err != nil {
				return err
			}
			s.entries[len(s.entries)-1].PendingRevocation = -pending
		}
	}
	r.State = next
	r.Consumed = actual
	r.Evidence = evidence
	s.saveReservation(r)
	return nil
}

func (e *Engine) Settle(ctx context.Context, in SettleInput) (Result, error) {
	metrics := slices.Clone(in.Evidence.Metrics)
	slices.SortFunc(metrics, func(a, b Metric) int { return cmp.Compare(a.Name, b.Name) })
	fields := []string{"settle", in.ReservationID, strconv.FormatInt(in.Actual, 10), in.Evidence.UsageID, in.Evidence.RatingVersion}
	for _, m := range metrics {
		fields = append(fields, m.Name, strconv.FormatInt(m.Quantity, 10))
	}
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint(fields...), func(s *state) (Result, error) {
		r, ok, err := s.reservation(in.ReservationID)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if r.State == "timed_out" {
			return Result{}, billing.ErrExpired
		}
		if _, active := s.reservations[in.ReservationID]; !active || r.State != "held" {
			return Result{}, billing.ErrState
		}
		if in.Actual < 0 || in.Actual > r.Authorized || !billing.ValidID(in.Evidence.UsageID) || !billing.ValidID(in.Evidence.RatingVersion) || len(metrics) == 0 {
			return Result{}, billing.ErrInvalid
		}
		for i, m := range metrics {
			if !billing.ValidID(m.Name) || m.Quantity < 0 || (i > 0 && metrics[i-1].Name == m.Name) {
				return Result{}, billing.ErrInvalid
			}
		}
		if s.usageExists != nil {
			exists, err := s.usageExists(in.Evidence.UsageID)
			if err != nil {
				return Result{}, err
			}
			if exists {
				return Result{}, billing.ErrConflict
			}
		}
		for _, other := range s.reservations {
			if other.Evidence.UsageID == in.Evidence.UsageID {
				return Result{}, billing.ErrConflict
			}
		}
		ev := in.Evidence
		ev.Metrics = metrics
		if err := s.finish(r, in.Actual, "settled", "", ev); err != nil {
			return Result{}, err
		}
		out, err := s.result(r.Unit, r.Scope)
		out.ReservationID = r.ID
		out.Consumed = in.Actual
		return out, err
	})
}
func (e *Engine) Release(ctx context.Context, in ReleaseInput) (Result, error) {
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint("release", in.ReservationID, in.Reason), func(s *state) (Result, error) {
		r, ok, err := s.reservation(in.ReservationID)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if r.State != "held" {
			return Result{}, billing.ErrState
		}
		if err := s.finish(r, 0, "released", in.Reason, Evidence{}); err != nil {
			return Result{}, err
		}
		out, err := s.result(r.Unit, r.Scope)
		out.ReservationID = r.ID
		return out, err
	})
}
func (e *Engine) Revoke(ctx context.Context, in RevokeInput) (Result, error) {
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint("revoke", in.LotID, in.Reason), func(s *state) (Result, error) {
		l, ok, lookupErr := s.lot(in.LotID)
		if lookupErr != nil {
			return Result{}, lookupErr
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if !l.RevokedAt.IsZero() {
			return Result{}, billing.ErrState
		}
		l.RevokedAt = s.now
		s.saveLot(l)
		if err := s.change(l.ID, "revoke", "", in.Reason, -l.Available, 0, 0, 0, l.Available, s.now); err != nil {
			return Result{}, err
		}
		out, err := s.result(l.Unit.Code, l.Scope)
		out.LotID = l.ID
		out.Exposure = l.Consumed
		return out, err
	})
}
func (e *Engine) RevokeAmount(ctx context.Context, in RevokeAmountInput) (Result, error) {
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint("revoke_amount", in.LotID, in.Reason, strconv.FormatInt(in.Amount, 10)), func(s *state) (Result, error) {
		if in.Amount <= 0 || in.Reason == "" {
			return Result{}, billing.ErrInvalid
		}
		l, ok, lookupErr := s.lot(in.LotID)
		if lookupErr != nil {
			return Result{}, lookupErr
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if !l.RevokedAt.IsZero() {
			return Result{}, billing.ErrState
		}
		if in.Amount > l.Initial {
			return Result{}, billing.ErrInvalid
		}
		immediate := min(in.Amount, l.Available)
		pending := min(in.Amount-immediate, l.Held-l.PendingRevocation)
		l.PendingRevocation += pending
		s.saveLot(l)
		if err := s.change(l.ID, "revoke_amount", "", in.Reason, -immediate, 0, 0, 0, immediate, s.now); err != nil {
			return Result{}, err
		}
		s.entries[len(s.entries)-1].PendingRevocation = pending
		out, err := s.result(l.Unit.Code, l.Scope)
		out.LotID = l.ID
		out.Exposure = in.Amount - immediate
		return out, err
	})
}

func (e *Engine) SetLimit(ctx context.Context, in LimitInput) (Result, error) {
	l := in.Limit
	l.Period.Start = billing.CanonicalTime(l.Period.Start)
	l.Period.End = billing.CanonicalTime(l.Period.End)
	return e.run(ctx, in.Account, in.Operation, identity.Fingerprint("limit", l.Actor, l.Unit, identity.Instant(l.Period.Start), identity.Instant(l.Period.End), strconv.FormatInt(l.Amount, 10)), func(s *state) (Result, error) {
		if !billing.ValidID(l.Actor) || !billing.ValidID(l.Unit) || !l.Period.Valid() || l.Amount < 0 {
			return Result{}, billing.ErrInvalid
		}
		overlaps, err := s.limitsOverlapping(l.Actor, l.Unit, l.Period)
		if err != nil {
			return Result{}, &projectionReadError{cause: err}
		}
		for _, old := range overlaps {
			// time.Time must never be compared with ==: a repository may return
			// an equal instant carrying a different location, and the identical
			// stored period would then be read as a conflicting overlap.
			same := old.Period.Equal(l.Period)
			if old.Actor == l.Actor && old.Unit == l.Unit && !same && old.Period.Start.Before(l.Period.End) && l.Period.Start.Before(old.Period.End) {
				return Result{}, billing.ErrConflict
			}
		}
		used, err := s.committed(l.Actor, l.Unit, l.Period)
		if err != nil {
			return Result{}, err
		}
		if used > l.Amount {
			return Result{}, billing.ErrLimit
		}
		s.limitWrites = append(s.limitWrites, l)
		return s.result(l.Unit, "")
	})
}

// Balance never materializes LiveLots. A backlog larger than one page returns
// ErrMaintenanceRequired (not a recorded rejection); call Sweep before retrying.
func (e *Engine) Balance(ctx context.Context, account billing.AccountID, unit, scope string) (Balance, error) {
	var out Balance
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		now := billing.CanonicalTime(e.now())
		s, err := load(tx, now, "system:expiry")
		if err != nil {
			return err
		}
		if err = s.sweep(); err != nil {
			return err
		}
		if err = s.flush(tx); err != nil {
			return err
		}
		out, err = tx.StoredBalance(unit, scope, now)
		if err != nil {
			return &projectionReadError{cause: err}
		}
		return nil
	})
	return out, err
}

func (e *Engine) ActiveReservations(ctx context.Context, account billing.AccountID) ([]Reservation, error) {
	var out []Reservation
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		s, err := load(tx, billing.CanonicalTime(e.now()), "system:expiry")
		if err != nil {
			return err
		}
		if err = s.sweep(); err != nil {
			return err
		}
		rows, err := tx.ActiveReservations()
		if err != nil {
			return err
		}
		out = make([]Reservation, 0, len(rows))
		for _, row := range rows {
			out = append(out, cloneReservation(row))
		}
		return s.flush(tx)
	})
	return out, err
}

func (e *Engine) Reservation(ctx context.Context, account billing.AccountID, id string) (Reservation, error) {
	var out Reservation
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		s, err := load(tx, billing.CanonicalTime(e.now()), "system:expiry")
		if err != nil {
			return err
		}
		if err = s.sweep(); err != nil {
			return err
		}
		r, ok, err := s.reservation(id)
		if err != nil {
			return err
		}
		if !ok {
			return billing.ErrNotFound
		}
		out = cloneReservation(r)
		return s.flush(tx)
	})
	return out, err
}
