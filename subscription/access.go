package subscription

// Access is what a subscription in a status lets its customer do.
type Access string

const (
	// Full: paid up, or in a trial.
	Full Access = "full"
	// Grace: payment is being retried; access continues while the provider
	// dunns, so the dunning window is the grace period.
	Grace Access = "grace"
	// None: paused or canceled.
	None Access = "none"
)

// AccessOf maps a status to access.
func AccessOf(s Status) Access {
	switch s {
	case Active, Trialing:
		return Full
	case PastDue:
		return Grace
	}
	return None
}
