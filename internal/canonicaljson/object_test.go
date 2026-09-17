package canonicaljson

import (
	"bytes"
	"testing"
)

func TestObjectPreservesExactNumbersAcrossSpellings(t *testing.T) {
	cases := map[string]string{
		`{"z":1e3,"a":9007199254740993}`:             `{"a":9007199254740993,"z":1000}`,
		`{"n":-0.000,"a":[1.2300e-2,12e-1,-100e-3]}`: `{"a":[0.0123,1.2,-0.1],"n":0}`,
		`{"n":0.00001000}`:                           `{"n":0.00001}`,
	}
	for input, want := range cases {
		out, err := Object([]byte(input), 1024)
		if err != nil || string(out) != want {
			t.Fatalf("%s => %s, %v want%s", input, out, err, want)
		}
		again, err := Object(out, 1024)
		if err != nil || !bytes.Equal(out, again) {
			t.Fatal("not idempotent")
		}
	}
}
func TestObjectRejectsUnboundedExpansionAndInvalidShapes(t *testing.T) {
	for _, input := range []string{`{"n":1e999999999}`, `{"n":1e-999999999}`, `{"n":1e999999999999999999999}`, `[]`, `null`, `{"n":NaN}`, `{} {}`, `{"n":01}`} {
		if _, err := Object([]byte(input), 1024); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func FuzzObjectCanonicalStable(f *testing.F) {
	for _, seed := range []string{`{"n":9007199254740993,"e":1e3}`, `{"a":[-0,1.230e-2]}`, `{"x":"text"}`, `{"x":1e999999999}`, `{"x":"\u0000"}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		out, err := Object([]byte(input), 1024)
		if err != nil {
			return
		}
		if len(out) > 1024 {
			t.Fatal("size exceeded")
		}
		again, err := Object(out, 1024)
		if err != nil || !bytes.Equal(out, again) {
			t.Fatalf("unstable normalization: %s => %s (%v)", out, again, err)
		}
	})
}

func TestObjectRejectsPostgresIncompatiblePayloads(t *testing.T) {
	for _, input := range []string{`{"n":1e-16384}`, `{"n":"\u0000"}`, `{"\u0000":1}`} {
		if _, err := Object([]byte(input), 65536); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
