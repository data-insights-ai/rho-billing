package usage

// Rates are expressed in the target's smallest unit. For example, a USD target
// with a rate of "25" charges 25 cents per unit. Credit rates are expressed in
// the credit unit's immutable subunits. Calculations use exact rational
// arithmetic and are rounded once, after all components are added.

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
)

var (
	ErrInvalidRule      = errors.New("invalid rating rule")
	ErrInvalidInput     = errors.New("invalid rating input")
	ErrOverflow         = errors.New("rating output overflow")
	ErrUnknownMetric    = errors.New("unknown rating metric")
	ErrConflictingInput = errors.New("conflicting rating input")
)

type Kind uint8

const (
	KindFixed Kind = iota + 1
	KindWeighted
	KindIncludedQuota
	KindGraduated
)

func (k Kind) String() string {
	switch k {
	case KindFixed:
		return "fixed"
	case KindWeighted:
		return "weighted"
	case KindIncludedQuota:
		return "included_quota"
	case KindGraduated:
		return "graduated"
	default:
		return "unknown"
	}
}

// Rounding is applied once to the complete exact result.
type Rounding uint8

const (
	RoundDown Rounding = iota + 1
	RoundUp
	RoundHalfUp
	RoundHalfEven
)

func (r Rounding) String() string {
	switch r {
	case RoundDown:
		return "down"
	case RoundUp:
		return "up"
	case RoundHalfUp:
		return "half_up"
	case RoundHalfEven:
		return "half_even"
	default:
		return "unknown"
	}
}

// Currency quantities are minor units; credit quantities are subunits in the
// named credit unit.
type Target struct {
	Currency   string
	CreditUnit string
}

type Money struct {
	Currency   string
	MinorUnits int64
}

type Credits struct {
	Unit     string
	Subunits int64
}

type MetricQuantity struct {
	Name     string
	Quantity int64
}

type RateInput struct {
	ActionCount      int64
	Metrics          []MetricQuantity
	Waived           bool
	Adjustment       string
	AdjustmentReason string
}

type Evidence struct {
	ActionCount      int64
	Metrics          []MetricQuantity
	Waived           bool
	Adjustment       string
	AdjustmentReason string
}

type Result struct {
	RuleVersion   string
	Kind          Kind
	Target        Target
	Rounding      Rounding
	ExactAmount   string
	RoundedAmount int64
	Money         *Money
	Credits       *Credits
	Evidence      Evidence
}

type Tier struct {
	UpTo int64 // exclusive; 0 on the last tier means unbounded
	Rate string
}

type IncludedQuotaConfig struct {
	Metric   string
	Included int64
	Rate     string
}

type RuleConfig struct {
	Version   string
	Kind      Kind
	Target    Target
	Rounding  Rounding
	FixedRate string
	Weights   map[string]string
	Included  *IncludedQuotaConfig
	Metric    string
	Tiers     []Tier
}

type Rule struct {
	version  string
	kind     Kind
	target   Target
	rounding Rounding
	fixed    *big.Rat
	weights  map[string]*big.Rat
	included *includedQuota
	metric   string
	tiers    []tier
}

type includedQuota struct {
	included int64
	rate     *big.Rat
}

type tier struct {
	upTo int64
	rate *big.Rat
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func NewRule(config RuleConfig) (*Rule, error) {
	version := strings.TrimSpace(config.Version)
	if version == "" || version != config.Version {
		return nil, fmt.Errorf("%w: version must be non-empty and trimmed", ErrInvalidRule)
	}
	if config.Kind < KindFixed || config.Kind > KindGraduated {
		return nil, fmt.Errorf("%w: unknown kind %d", ErrInvalidRule, config.Kind)
	}
	target, err := validateTarget(config.Target)
	if err != nil {
		return nil, err
	}
	if config.Rounding < RoundDown || config.Rounding > RoundHalfEven {
		return nil, fmt.Errorf("%w: unknown rounding mode %d", ErrInvalidRule, config.Rounding)
	}
	rule := &Rule{version: version, kind: config.Kind, target: target, rounding: config.Rounding}
	switch config.Kind {
	case KindFixed:
		if len(config.Weights) != 0 || config.Included != nil || config.Metric != "" || len(config.Tiers) != 0 {
			return nil, fmt.Errorf("%w: fixed rule has incompatible configuration", ErrInvalidRule)
		}
		rate, err := parseNonNegativeRate(config.FixedRate)
		if err != nil {
			return nil, err
		}
		rule.fixed = rate
	case KindWeighted:
		if config.FixedRate != "" || config.Included != nil || config.Metric != "" || len(config.Tiers) != 0 || len(config.Weights) == 0 {
			return nil, fmt.Errorf("%w: weighted rule requires only non-empty weights", ErrInvalidRule)
		}
		rule.weights = make(map[string]*big.Rat, len(config.Weights))
		for name, value := range config.Weights {
			if !validName(name) {
				return nil, fmt.Errorf("%w: invalid metric name %q", ErrInvalidRule, name)
			}
			rate, err := parseNonNegativeRate(value)
			if err != nil {
				return nil, fmt.Errorf("%w: metric %q: %v", ErrInvalidRule, name, err)
			}
			rule.weights[name] = rate
		}
	case KindIncludedQuota:
		if config.FixedRate != "" || len(config.Weights) != 0 || config.Included == nil || config.Metric != "" || len(config.Tiers) != 0 {
			return nil, fmt.Errorf("%w: included-quota rule has incompatible configuration", ErrInvalidRule)
		}
		if !validName(config.Included.Metric) || config.Included.Included < 0 {
			return nil, fmt.Errorf("%w: invalid included-quota metric or quantity", ErrInvalidRule)
		}
		rate, err := parseNonNegativeRate(config.Included.Rate)
		if err != nil {
			return nil, err
		}
		rule.included = &includedQuota{included: config.Included.Included, rate: rate}
		rule.metric = config.Included.Metric
	case KindGraduated:
		if config.FixedRate != "" || len(config.Weights) != 0 || config.Included != nil || !validName(config.Metric) || len(config.Tiers) == 0 {
			return nil, fmt.Errorf("%w: graduated rule requires metric and tiers", ErrInvalidRule)
		}
		rule.metric = config.Metric
		rule.tiers = make([]tier, len(config.Tiers))
		var previous int64
		// The last tier must be unbounded (UpTo == 0). A bounded final tier
		// leaves every unit above it unpriced, and graduatedAmount would
		// silently discard them instead of charging for them.
		if config.Tiers[len(config.Tiers)-1].UpTo != 0 {
			return nil, fmt.Errorf("%w: last tier must be unbounded", ErrInvalidRule)
		}
		for i, source := range config.Tiers {
			if source.UpTo < 0 || (source.UpTo != 0 && source.UpTo <= previous) || (source.UpTo == 0 && i != len(config.Tiers)-1) {
				return nil, fmt.Errorf("%w: tier %d has invalid boundary", ErrInvalidRule, i)
			}
			rate, err := parseNonNegativeRate(source.Rate)
			if err != nil {
				return nil, fmt.Errorf("%w: tier %d: %v", ErrInvalidRule, i, err)
			}
			rule.tiers[i] = tier{upTo: source.UpTo, rate: rate}
			if source.UpTo != 0 {
				previous = source.UpTo
			}
		}
	}
	return rule, nil
}

func (r *Rule) Version() string { return r.version }

func (r *Rule) Kind() Kind { return r.kind }

func (r *Rule) Target() Target { return r.target }

func (r *Rule) Rounding() Rounding { return r.rounding }

// All arithmetic is exact until the configured final rounding step.
func (r *Rule) Rate(input RateInput) (Result, error) {
	if r == nil {
		return Result{}, fmt.Errorf("%w: nil rule", ErrInvalidRule)
	}
	if input.ActionCount < 0 {
		return Result{}, fmt.Errorf("%w: negative action count", ErrInvalidInput)
	}
	metrics, err := normalizeMetrics(input.Metrics)
	if err != nil {
		return Result{}, err
	}
	adjustment, err := parseAdjustment(input.Adjustment, input.AdjustmentReason, input.Waived)
	if err != nil {
		return Result{}, err
	}
	if r.kind == KindFixed && len(metrics) != 0 {
		return Result{}, fmt.Errorf("%w: fixed rule cannot receive metrics", ErrConflictingInput)
	}
	if r.kind != KindFixed && input.ActionCount != 0 {
		return Result{}, fmt.Errorf("%w: only fixed rules accept action count", ErrConflictingInput)
	}
	if r.kind == KindWeighted {
		for _, observed := range metrics {
			if _, ok := r.weights[observed.Name]; !ok {
				return Result{}, fmt.Errorf("%w: %q", ErrUnknownMetric, observed.Name)
			}
		}
	}
	if (r.kind == KindIncludedQuota || r.kind == KindGraduated) && len(metrics) != 1 {
		return Result{}, fmt.Errorf("%w: rule requires exactly one metric", ErrConflictingInput)
	}
	if (r.kind == KindIncludedQuota || r.kind == KindGraduated) && metrics[0].Name != r.metric {
		return Result{}, fmt.Errorf("%w: expected metric %q", ErrUnknownMetric, r.metric)
	}

	total := new(big.Rat)
	if !input.Waived {
		switch r.kind {
		case KindFixed:
			total.Mul(r.fixed, new(big.Rat).SetInt64(input.ActionCount))
		case KindWeighted:
			for _, observed := range metrics {
				term := new(big.Rat).Mul(r.weights[observed.Name], new(big.Rat).SetInt64(observed.Quantity))
				total.Add(total, term)
			}
		case KindIncludedQuota:
			overage := max(0, metrics[0].Quantity-r.included.included)
			total.Mul(r.included.rate, new(big.Rat).SetInt64(overage))
		case KindGraduated:
			total = graduatedAmount(r.tiers, metrics[0].Quantity)
		}
	}
	total.Add(total, adjustment)
	if total.Sign() < 0 {
		return Result{}, fmt.Errorf("%w: result is negative", ErrInvalidInput)
	}
	rounded, err := round(total, r.rounding)
	if err != nil {
		return Result{}, err
	}
	if !rounded.IsInt64() {
		return Result{}, ErrOverflow
	}
	amount := rounded.Int64()
	evidenceMetrics := slices.Clone(metrics)
	evidence := Evidence{ActionCount: input.ActionCount, Metrics: evidenceMetrics, Waived: input.Waived, Adjustment: canonicalRat(adjustment), AdjustmentReason: input.AdjustmentReason}
	result := Result{RuleVersion: r.version, Kind: r.kind, Target: r.target, Rounding: r.rounding, ExactAmount: canonicalRat(total), RoundedAmount: amount, Evidence: evidence}
	if r.target.Currency != "" {
		result.Money = new(Money)
		result.Money.Currency = r.target.Currency
		result.Money.MinorUnits = amount
	} else {
		result.Credits = new(Credits)
		result.Credits.Unit = r.target.CreditUnit
		result.Credits.Subunits = amount
	}
	return result, nil
}

func validateTarget(target Target) (Target, error) {
	if (target.Currency == "") == (target.CreditUnit == "") {
		return Target{}, fmt.Errorf("%w: exactly one currency or credit unit is required", ErrInvalidRule)
	}
	if target.Currency != "" {
		if len(target.Currency) != 3 || strings.ToUpper(target.Currency) != target.Currency || !lettersOnly(target.Currency) {
			return Target{}, fmt.Errorf("%w: currency must be three uppercase letters", ErrInvalidRule)
		}
		return Target{Currency: target.Currency}, nil
	}
	if !validName(target.CreditUnit) {
		return Target{}, fmt.Errorf("%w: invalid credit unit %q", ErrInvalidRule, target.CreditUnit)
	}
	return Target{CreditUnit: target.CreditUnit}, nil
}

func lettersOnly(value string) bool {
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func validName(value string) bool {
	return value != "" && namePattern.MatchString(value)
}

func parseNonNegativeRate(value string) (*big.Rat, error) {
	if value == "" {
		return nil, fmt.Errorf("%w: rate is required", ErrInvalidRule)
	}
	rate, ok := new(big.Rat).SetString(value)
	if !ok || rate.Sign() < 0 {
		return nil, fmt.Errorf("%w: invalid non-negative rate %q", ErrInvalidRule, value)
	}
	return rate, nil
}

func parseAdjustment(value, reason string, waived bool) (*big.Rat, error) {
	if value == "" {
		return new(big.Rat), nil
	}
	adjustment, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("%w: invalid adjustment %q", ErrInvalidInput, value)
	}
	if waived && adjustment.Sign() != 0 {
		return nil, fmt.Errorf("%w: waiver cannot include nonzero adjustment", ErrConflictingInput)
	}
	if adjustment.Sign() != 0 && strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: adjustment reason is required", ErrInvalidInput)
	}
	return adjustment, nil
}

func normalizeMetrics(values []MetricQuantity) ([]MetricQuantity, error) {
	if len(values) == 0 {
		return nil, nil
	}
	metrics := slices.Clone(values)
	seen := make(map[string]struct{}, len(metrics))
	for _, metric := range metrics {
		if !validName(metric.Name) {
			return nil, fmt.Errorf("%w: invalid metric name %q", ErrInvalidInput, metric.Name)
		}
		if metric.Quantity < 0 {
			return nil, fmt.Errorf("%w: negative quantity for %q", ErrInvalidInput, metric.Name)
		}
		if _, exists := seen[metric.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate metric %q", ErrConflictingInput, metric.Name)
		}
		seen[metric.Name] = struct{}{}
	}
	slices.SortFunc(metrics, func(a, b MetricQuantity) int {
		return strings.Compare(a.Name, b.Name)
	})
	return metrics, nil
}

func graduatedAmount(tiers []tier, quantity int64) *big.Rat {
	remaining := quantity
	var previous int64
	total := new(big.Rat)
	for _, current := range tiers {
		band := remaining
		if current.upTo != 0 {
			band = min(remaining, current.upTo-previous)
		}
		if band > 0 {
			total.Add(total, new(big.Rat).Mul(current.rate, new(big.Rat).SetInt64(band)))
			remaining -= band
		}
		if remaining == 0 {
			break
		}
		if current.upTo != 0 {
			previous = current.upTo
		}
	}
	return total
}

func round(value *big.Rat, mode Rounding) (*big.Int, error) {
	if value.Sign() < 0 {
		return nil, fmt.Errorf("%w: negative exact result", ErrInvalidInput)
	}
	numerator := new(big.Int).Set(value.Num())
	denominator := new(big.Int).Set(value.Denom())
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() == 0 {
		return quotient, nil
	}
	switch mode {
	case RoundDown:
		return quotient, nil
	case RoundUp:
		return quotient.Add(quotient, big.NewInt(1)), nil
	case RoundHalfUp, RoundHalfEven:
		twice := new(big.Int).Lsh(remainder, 1)
		if twice.Cmp(denominator) > 0 || (twice.Cmp(denominator) == 0 && mode == RoundHalfUp) {
			return quotient.Add(quotient, big.NewInt(1)), nil
		}
		if twice.Cmp(denominator) == 0 && quotient.Bit(0) == 1 {
			return quotient.Add(quotient, big.NewInt(1)), nil
		}
		return quotient, nil
	default:
		return nil, fmt.Errorf("%w: unknown rounding mode", ErrInvalidRule)
	}
}

func canonicalRat(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	return value.RatString()
}
