package billing

// Reference identifies a provider-owned resource within a provider scope.
// The enclosing domain owns the host billing account and resource kind; this
// value carries only the provider identity and its external resource ID.
//
// Reference is comparable and can be used as a map key.
type Reference struct {
	Scope Scope  `json:",omitzero"`
	ID    string `json:",omitempty"`
}

func (r Reference) Valid() bool {
	return r.Scope.Valid() && ValidID(r.ID)
}
