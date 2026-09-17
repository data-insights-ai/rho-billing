// Package billing holds IDs, errors, clock precision, and provider identity.
package billing

import (
	"errors"
	"strings"
	"time"

	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type AccountID string
type OperationID string

var (
	ErrInvalid      = errors.New("billing: invalid input")
	ErrConflict     = errors.New("billing: operation or version conflicts with existing input")
	ErrNotFound     = errors.New("billing: record not found in account")
	ErrInsufficient = errors.New("billing: insufficient available credits")
	ErrLimit        = errors.New("billing: spending limit exceeded")
	ErrState        = errors.New("billing: invalid state transition")
	ErrExpired      = errors.New("billing: authorization expired")
	ErrOverflow     = checked.ErrOverflow
)

// Period is an absolute half-open interval. Calendar schedules are a separate
// concern: their anchors must be preserved instead of adding fixed durations.
type Period struct{ Start, End time.Time }

func (p Period) Valid() bool                { return !p.Start.IsZero() && p.End.After(p.Start) }
func (p Period) Contains(at time.Time) bool { return !at.Before(p.Start) && at.Before(p.End) }

// Equal compares the instants a period covers. Never use == on a Period: it
// contains time.Time, whose struct equality also compares the monotonic reading
// and the location pointer, so an equal instant read back from a database
// compares unequal to the one that was written.
func (p Period) Equal(other Period) bool {
	return p.Start.Equal(other.Start) && p.End.Equal(other.End)
}

// Unit fixes the scale of an integer credit quantity. Scale=1000 means one credit
// equals 1000 subunits. A published unit's scale must never change.
type Unit struct {
	Code  string
	Scale int64
}

func (u Unit) Valid() bool { return ValidID(u.Code) && u.Scale > 0 }

// ValidID allows opaque application IDs, but rejects empty/control-bearing IDs.
func ValidID(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsFunc(s, func(r rune) bool { return r < 32 || r == 127 })
}

// CanonicalTime is the billing clock's precision: UTC microseconds, matching
// PostgreSQL timestamptz. Normalize supplied boundaries before using them in
// operations, not only when serializing to storage.
func CanonicalTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
