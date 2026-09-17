// Package canonicaljson provides exact, bounded JSON object normalization for
// immutable evidence that passes through PostgreSQL JSONB.
package canonicaljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("canonicaljson: invalid or oversized object")

// Object sorts object keys and normalizes decimal/exponent number spellings
// without float64 conversion. The expanded output must fit limit. Input
// formatting may use twice that space so PostgreSQL JSONB whitespace does not
// invalidate stored evidence.
// Duplicate object keys follow encoding/json's last-value semantics; only the
// returned canonical object is retained or delivered downstream.
func Object(raw []byte, limit int) ([]byte, error) {
	if limit < 1 || len(raw) == 0 || (len(raw) > limit && len(raw)-limit > limit) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, ErrInvalid
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrInvalid
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, ErrInvalid
	}
	remaining := limit
	normalized, err := normalize(value, &remaining)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(normalized)
	if err != nil || len(out) > limit {
		return nil, ErrInvalid
	}
	return out, nil
}

func normalize(value any, remaining *int) (any, error) {
	switch v := value.(type) {
	case json.Number:
		out, err := number(string(v), *remaining)
		if err == nil {
			*remaining -= len(out)
		}
		return json.Number(out), err
	case string:
		if strings.IndexByte(v, 0) >= 0 {
			return nil, ErrInvalid
		}
		return v, nil
	case map[string]any:
		for key, item := range v {
			if strings.IndexByte(key, 0) >= 0 {
				return nil, ErrInvalid
			}
			next, err := normalize(item, remaining)
			if err != nil {
				return nil, err
			}
			v[key] = next
		}
		return v, nil
	case []any:
		for i, item := range v {
			next, err := normalize(item, remaining)
			if err != nil {
				return nil, err
			}
			v[i] = next
		}
		return v, nil
	default:
		return value, nil
	}
}

// number receives valid JSON number syntax from the decoder. It bounds exponent
// expansion before allocating zero padding, including very small fractions.
func number(raw string, limit int) (string, error) {
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign = "-"
		raw = raw[1:]
	}
	mantissa, exponent := raw, int64(0)
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		mantissa = raw[:i]
		var err error
		exponent, err = strconv.ParseInt(raw[i+1:], 10, 32)
		if err != nil {
			return "", ErrInvalid
		}
	}
	digits := mantissa
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		digits = mantissa[:i] + mantissa[i+1:]
		exponent -= int64(len(mantissa) - i - 1)
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += int64(len(digits) - len(trimmed))
	digits = trimmed
	if exponent >= 0 {
		if int64(len(sign)+len(digits))+exponent > int64(limit) {
			return "", ErrInvalid
		}
		return sign + digits + strings.Repeat("0", int(exponent)), nil
	}
	// PostgreSQL JSONB uses numeric, whose fractional scale is bounded.
	// https://www.postgresql.org/docs/17/datatype-numeric.html
	if exponent < -16383 {
		return "", ErrInvalid
	}
	position := int64(len(digits)) + exponent
	if position > 0 {
		if len(sign)+len(digits)+1 > limit {
			return "", ErrInvalid
		}
		return sign + digits[:int(position)] + "." + digits[int(position):], nil
	}
	if int64(len(sign)+2+len(digits))-position > int64(limit) {
		return "", ErrInvalid
	}
	return sign + "0." + strings.Repeat("0", int(-position)) + digits, nil
}
