package credit

import (
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

var (
	ErrIneligible  = errors.New("credit: eligibility evidence does not cover period")
	ErrMemberScope = errors.New("credit: member scope requires actor policy")
	ErrAdjustment  = errors.New("credit: adjustment policy required")
)

type PeriodInput struct {
	Definition  catalog.AllowanceDefinition
	Assignment  catalog.PlanAssignment
	Anchor      time.Time
	OriginalDay int
	Through     time.Time
}

func Periods(input PeriodInput) ([]billing.Period, error) {
	periods, _, err := PeriodsPage(input, time.Time{}, 0)
	return periods, err
}

func PeriodsPage(input PeriodInput, after time.Time, limit int) ([]billing.Period, bool, error) {
	if err := validatePeriodInput(input); err != nil {
		return nil, false, err
	}
	if limit < 0 {
		return nil, false, billing.ErrInvalid
	}
	anchor := input.Anchor.UTC()
	if anchor.IsZero() {
		anchor = input.Assignment.Effective.Start.UTC()
	}
	if anchor.IsZero() || anchor.After(input.Assignment.Effective.End.UTC()) {
		return nil, false, billing.ErrInvalid
	}
	if input.Through.Before(anchor) {
		return nil, false, billing.ErrInvalid
	}
	after = after.UTC()
	if input.Definition.Recurrence == catalog.AllowanceOneTime {
		period := input.Assignment.Effective
		period.Start = period.Start.UTC()
		period.End = period.End.UTC()
		if !period.Start.Equal(anchor) {
			return nil, false, billing.ErrInvalid
		}
		if period.Start.Before(input.Through) && period.Start.After(after) {
			return []billing.Period{period}, false, nil
		}
		return nil, false, nil
	}

	originalDay := input.OriginalDay
	if originalDay == 0 {
		originalDay = anchor.Day()
	}
	if originalDay < 1 || originalDay > 31 {
		return nil, false, billing.ErrInvalid
	}
	periods := make([]billing.Period, 0, minInt(limit, 8))
	step := recurrenceMonths(input.Definition.Recurrence)
	offset := calendarOffset(anchor, originalDay, after, step)
	for ; ; offset += step {
		start := calendarDate(anchor, originalDay, offset)
		if !start.Before(input.Through) || !start.Before(input.Assignment.Effective.End.UTC()) {
			break
		}
		end := calendarDate(anchor, originalDay, offset+step)
		if !end.After(start) || end.After(input.Assignment.Effective.End.UTC()) {
			break
		}
		if !end.After(input.Assignment.Effective.Start.UTC()) {
			continue
		}
		if !start.After(after) {
			continue
		}
		periods = append(periods, billing.Period{Start: start, End: end})
		if limit > 0 && len(periods) >= limit {
			return periods, true, nil
		}
	}
	return periods, false, nil
}

func calendarOffset(anchor time.Time, originalDay int, after time.Time, step int) int {
	if after.Before(anchor) {
		return 0
	}
	months := (after.Year()-anchor.Year())*12 + int(after.Month()-anchor.Month())
	if months <= 0 {
		return 0
	}
	offset := (months / step) * step
	for offset > 0 && calendarDate(anchor, originalDay, offset).After(after) {
		offset -= step
	}
	return offset
}

func minInt(a, b int) int {
	if a == 0 || a > b {
		return b
	}
	return a
}

func validatePeriodInput(input PeriodInput) error {
	if !billing.ValidID(input.Assignment.PlanVersionID) || input.Assignment.Quantity <= 0 || !input.Assignment.Effective.Valid() || input.Assignment.Perpetual {
		return billing.ErrInvalid
	}
	if input.Assignment.ID != "" && !billing.ValidID(input.Assignment.ID) {
		return billing.ErrInvalid
	}
	if input.Through.IsZero() {
		return billing.ErrInvalid
	}
	if !catalog.ValidPlan(catalog.PlanVersion{
		ID: "allowance-validation", PlanID: "allowance-validation", Version: 1,
		Allowances: []catalog.AllowanceDefinition{input.Definition},
	}) {
		return billing.ErrInvalid
	}
	return nil
}

func recurrenceMonths(recurrence catalog.AllowanceRecurrence) int {
	if recurrence == catalog.AllowanceAnnual {
		return 12
	}
	return 1
}

func calendarDate(anchor time.Time, originalDay, offset int) time.Time {
	monthIndex := int(anchor.Month()) - 1 + offset
	year := anchor.Year() + monthIndex/12
	month := time.Month(monthIndex%12 + 1)
	day := min(originalDay, daysInMonth(year, month))
	return time.Date(year, month, day, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
