package usage

// SettlementRejection is a recorded terminal business outcome. Hosts may commit
// after matching it with errors.AsType; do not treat it as a storage failure.
// Propagate other errors to roll back. Use errors.Is for the underlying reason.
type SettlementRejection struct{ Cause error }

func (r *SettlementRejection) Error() string { return r.Cause.Error() }
func (r *SettlementRejection) Unwrap() error { return r.Cause }
