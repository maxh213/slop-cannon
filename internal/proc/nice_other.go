//go:build unix && !linux

package proc

import (
	"fmt"
	"syscall"
)

// Renice lowers the scheduling priority of the calling process to niceness
// n (0-19), so every process it starts from then on inherits it. Zero
// leaves the priority alone. Outside Linux niceness belongs to the
// process, so one call covers every thread.
func Renice(n int) error {
	if n <= 0 {
		return nil
	}
	cur, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err == nil && cur >= n {
		return nil
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, n); err != nil {
		return fmt.Errorf("proc: renice to %d: %w", n, err)
	}
	return nil
}
