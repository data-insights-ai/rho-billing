package billing

import (
	"fmt"
)

type Support string

const (
	SupportSupported      Support = "supported"
	SupportActionRequired Support = "action_required"
	SupportUnsupported    Support = "unsupported"
	SupportUnresolved     Support = "unresolved"
)

func (s Support) valid() bool {
	switch s {
	case SupportSupported, SupportActionRequired, SupportUnsupported, SupportUnresolved:
		return true
	default:
		return false
	}
}

// Reason is optional for supported capabilities and required otherwise.
type Capability struct {
	Operation string
	Support   Support
	Reason    string
}

func (c Capability) Validate() error {
	if !ValidID(c.Operation) || !c.Support.valid() {
		return ErrInvalid
	}
	if c.Reason != "" && !ValidID(c.Reason) {
		return ErrInvalid
	}
	if c.Support != SupportSupported && c.Reason == "" {
		return ErrInvalid
	}
	return nil
}

// CapabilityError is the typed fail-closed result for an operation that is
// validly described but cannot proceed as supported. Capability is embedded so
// callers retain the operation, support status, and reason through errors.As.
type CapabilityError struct{ Capability }

func (e *CapabilityError) Error() string {
	if e == nil {
		return "provider: capability unavailable"
	}
	return fmt.Sprintf("provider: %s capability %s: %s", e.Operation, e.Support, e.Reason)
}

// RequireSupported fails closed for every status other than supported. The
// returned error retains the original status/reason.
func RequireSupported(c Capability) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Support == SupportSupported {
		return nil
	}
	return &CapabilityError{Capability: c}
}
