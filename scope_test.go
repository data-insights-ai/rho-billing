package billing

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestScopeValidAndCanonicalJSON(t *testing.T) {
	scope := Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"}
	if !scope.Valid() {
		t.Fatal("valid scope rejected")
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"Provider":"paddle","Merchant":"merchant","Environment":"sandbox"}`; got != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
	if string(raw) == `{"Provider":"paddle","Account":"merchant","Environment":"sandbox"}` {
		t.Fatal("canonical JSON retained legacy Account")
	}
	var decoded Scope
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != scope {
		t.Fatalf("canonical round trip = %+v, %v", decoded, err)
	}
}

func TestScopeUnmarshalRejectsConflictAndPreservesReceiver(t *testing.T) {
	original := Scope{Provider: "old-provider", Merchant: "old-merchant", Environment: "old-environment"}
	for _, test := range []struct {
		name string
		raw  string
		want error
	}{
		{name: "old merchant spelling", raw: `{"Provider":"paddle","Account":"merchant","Environment":"sandbox"}`, want: ErrInvalid},
		{name: "duplicate canonical field", raw: `{"Provider":"paddle","Merchant":"first","Merchant":"second","Environment":"sandbox"}`, want: ErrConflict},
		{name: "duplicate canonical field case variant", raw: `{"Provider":"paddle","Merchant":"first","merchant":"second","Environment":"sandbox"}`, want: ErrConflict},
		{name: "null", raw: `null`, want: ErrInvalid},
		{name: "array", raw: `[]`, want: ErrInvalid},
		{name: "string", raw: `"scope"`, want: ErrInvalid},
		{name: "missing merchant", raw: `{"Provider":"paddle","Environment":"sandbox"}`, want: ErrInvalid},
		{name: "wrong field type", raw: `{"Provider":"paddle","Merchant":7,"Environment":"sandbox"}`, want: ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := original
			err := json.Unmarshal([]byte(test.raw), &got)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if got != original {
				t.Fatalf("receiver mutated on failure: %+v, want %+v", got, original)
			}
		})
	}
}
