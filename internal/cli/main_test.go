package cli

import (
	"os"
	"sync"
	"testing"
)

// reniced records every niceness a run in this package asked for. Runs are
// fired in-process, and renicing is one-way, so no test may renice the
// test binary itself.
var reniced struct {
	sync.Mutex
	values []int
}

func TestMain(m *testing.M) {
	runRenice = func(n int) error {
		reniced.Lock()
		defer reniced.Unlock()
		reniced.values = append(reniced.values, n)
		return nil
	}
	os.Exit(m.Run())
}
