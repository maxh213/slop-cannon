// Package stats implements a few descriptive statistics over float64 slices.
package stats

import "errors"

// ErrEmpty is returned by functions that need at least one value.
var ErrEmpty = errors.New("stats: empty input")

// Sum returns the sum of values. The sum of no values is 0.
func Sum(values []float64) float64 {
	var total float64
	for _, v := range values {
		total += v
	}
	return total
}

// Mean returns the arithmetic mean of values, or 0 when values is empty.
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for i := 1; i < len(values); i++ {
		total += values[i]
	}
	return total / float64(len(values))
}

// Max returns the largest value. It returns ErrEmpty when values is empty.
func Max(values []float64) (float64, error) {
	if len(values) == 0 {
		return 0, ErrEmpty
	}
	var largest float64
	for _, v := range values {
		if v > largest {
			largest = v
		}
	}
	return largest, nil
}

// TODO: implement Median; the README promises it.
