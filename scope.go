package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Merchant is distinct from the host billing account carried by the enclosing
// operation. Scope deliberately contains only strings so it remains comparable
// and can be used in operation keys and map keys.
type Scope struct {
	Provider    string
	Merchant    string
	Environment string
}

func (s Scope) Valid() bool {
	return ValidID(s.Provider) && ValidID(s.Merchant) && ValidID(s.Environment)
}

// UnmarshalJSON rejects conflicting duplicate fields. Parsing into locals
// leaves the receiver intact on failure.
func (s *Scope) UnmarshalJSON(data []byte) error {
	if s == nil {
		return ErrInvalid
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '{' {
		return ErrInvalid
	}
	fields, err := decodeObject(trimmed)
	if err != nil {
		return ErrInvalid
	}
	provider, err := stringField(fields, "Provider")
	if err != nil {
		return err
	}
	environment, err := stringField(fields, "Environment")
	if err != nil {
		return err
	}
	merchant, err := stringField(fields, "Merchant")
	if err != nil {
		return err
	}
	decoded := Scope{Provider: provider, Merchant: merchant, Environment: environment}
	if !decoded.Valid() {
		return ErrInvalid
	}
	*s = decoded
	return nil
}

type rawField struct {
	name string
	raw  json.RawMessage
}

func decodeObject(data []byte) ([]rawField, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := start.(json.Delim)
	if !ok || delim != '{' {
		return nil, ErrInvalid
	}
	fields := make([]rawField, 0, 3)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, ErrInvalid
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields = append(fields, rawField{name: name, raw: raw})
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, ErrInvalid
		}
		return nil, err
	}
	return fields, nil
}

func stringField(fields []rawField, name string) (string, error) {
	value, present, err := optionalStringField(fields, name)
	if err != nil {
		return "", err
	}
	if !present {
		return "", ErrInvalid
	}
	return value, nil
}

func optionalStringField(fields []rawField, name string) (string, bool, error) {
	var value string
	var present bool
	for _, field := range fields {
		if !strings.EqualFold(field.name, name) {
			continue
		}
		var candidate string
		if err := json.Unmarshal(field.raw, &candidate); err != nil {
			return "", false, ErrInvalid
		}
		if present && value != candidate {
			return "", false, ErrConflict
		}
		value, present = candidate, true
	}
	if present && !ValidID(value) {
		return "", false, ErrInvalid
	}
	return value, present, nil
}
