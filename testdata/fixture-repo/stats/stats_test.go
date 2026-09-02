package stats

import (
	"errors"
	"testing"
)

func TestSum(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{4}, 4},
		{"several", []float64{1, 2, 3.5}, 6.5},
		{"negative", []float64{-1, -2}, -3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sum(tc.values); got != tc.want {
				t.Errorf("Sum(%v) = %g, want %g", tc.values, got, tc.want)
			}
		})
	}
}

func TestMean(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"several", []float64{2, 4, 6}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Mean(tc.values); got != tc.want {
				t.Errorf("Mean(%v) = %g, want %g", tc.values, got, tc.want)
			}
		})
	}
}

func TestMax(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"single", []float64{3}, 3},
		{"positive", []float64{1, 9, 4}, 9},
		{"all negative", []float64{-3, -1, -7}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Max(tc.values)
			if err != nil {
				t.Fatalf("Max(%v) returned error: %v", tc.values, err)
			}
			if got != tc.want {
				t.Errorf("Max(%v) = %g, want %g", tc.values, got, tc.want)
			}
		})
	}
}

func TestMaxEmpty(t *testing.T) {
	if _, err := Max(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Max(nil) error = %v, want ErrEmpty", err)
	}
}
