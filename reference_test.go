package billing

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestReferenceValidAndComparable(t *testing.T) {
	base := Reference{
		Scope: Scope{Provider: "paddle", Merchant: "merchant-a", Environment: "sandbox"},
		ID:    "subscription-a",
	}
	if !base.Valid() {
		t.Fatal("valid reference rejected")
	}

	references := map[Reference]string{
		base: "base",
		{Scope: Scope{Provider: "paddle", Merchant: "merchant-b", Environment: "sandbox"}, ID: base.ID}:           "merchant",
		{Scope: Scope{Provider: "paddle", Merchant: base.Scope.Merchant, Environment: "production"}, ID: base.ID}: "environment",
		{Scope: base.Scope, ID: "subscription-b"}:                                                                 "id",
	}
	if got, want := len(references), 4; got != want {
		t.Fatalf("reference map length = %d, want %d", got, want)
	}
	for reference, want := range references {
		if got := references[reference]; got != want {
			t.Fatalf("reference map lookup = %q, want %q", got, want)
		}
	}
}

func TestReferenceValidRejectsMissingComponents(t *testing.T) {
	valid := Reference{
		Scope: Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"},
		ID:    "subscription",
	}
	for _, test := range []struct {
		name string
		ref  Reference
	}{
		{name: "provider", ref: Reference{Scope: Scope{Merchant: valid.Scope.Merchant, Environment: valid.Scope.Environment}, ID: valid.ID}},
		{name: "merchant", ref: Reference{Scope: Scope{Provider: valid.Scope.Provider, Environment: valid.Scope.Environment}, ID: valid.ID}},
		{name: "environment", ref: Reference{Scope: Scope{Provider: valid.Scope.Provider, Merchant: valid.Scope.Merchant}, ID: valid.ID}},
		{name: "id", ref: Reference{Scope: valid.Scope}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.ref.Valid() {
				t.Fatal("invalid reference accepted")
			}
		})
	}
}

func TestReferenceCanonicalJSONRoundTripPreservesID(t *testing.T) {
	reference := Reference{
		Scope: Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"},
		ID:    "subscription-123",
	}
	raw, err := json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"Scope":{"Provider":"paddle","Merchant":"merchant","Environment":"sandbox"},"ID":"subscription-123"}`; got != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}

	var decoded Reference
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != reference {
		t.Fatalf("round trip = %+v, want %+v", decoded, reference)
	}
	if decoded.ID != reference.ID {
		t.Fatalf("round-trip ID = %q, want %q", decoded.ID, reference.ID)
	}
}

func TestReferenceZeroJSONRoundTrip(t *testing.T) {
	var reference Reference
	raw, err := json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{}`; got != want {
		t.Fatalf("zero reference JSON = %s, want %s", got, want)
	}

	var decoded Reference
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != reference {
		t.Fatalf("zero round trip = %+v, want %+v", decoded, reference)
	}
	if decoded.Valid() {
		t.Fatal("zero reference reported valid")
	}
}

func TestReferenceJSONRejectsInvalidScope(t *testing.T) {
	var reference Reference
	if err := json.Unmarshal([]byte(`{"Scope":{"Provider":"paddle","Merchant":"merchant"},"ID":"subscription"}`), &reference); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}
