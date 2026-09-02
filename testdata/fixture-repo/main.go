// Command fixture prints the sum, mean, and maximum of the numbers given as
// arguments.
package main

import (
	"fmt"
	"os"
	"strconv"

	"fixture/stats"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fixture NUMBER [NUMBER...]")
		os.Exit(2)
	}
	values := make([]float64, 0, len(os.Args)-1)
	for _, arg := range os.Args[1:] {
		v, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fixture: %q is not a number\n", arg)
			os.Exit(2)
		}
		values = append(values, v)
	}
	max, err := stats.Max(values)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
	fmt.Printf("sum=%g mean=%g max=%g\n", stats.Sum(values), stats.Mean(values), max)
}
