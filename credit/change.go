package credit

// Kind is what happened to a lot.
type Kind string

const (
	Hold     Kind = "hold"     // available → held for a reservation
	Consume  Kind = "consume"  // held or available → consumed
	Release  Kind = "release"  // held → available
	Lapse    Kind = "lapse"    // available, or held being released, → expired
	Withdraw Kind = "withdraw" // available, or held being released, → revoked
)

// Change is one movement on one lot, as deltas of its counters; the host
// appends it to its journal. Reservation is set when the movement belongs to
// one.
type Change struct {
	LotID       string
	Reservation string
	Kind        Kind

	Available, Held, Consumed, Expired, Revoked int64
}
