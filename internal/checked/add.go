// Package checked implements exact bounded integer arithmetic.
package checked

import (
	"errors"
	"math"
)

// ErrOverflow is re-exported by billing for public error matching.
var ErrOverflow = errors.New("billing: quantity overflow")

func Add(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrOverflow
	}
	return a + b, nil
}
