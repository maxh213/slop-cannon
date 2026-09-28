//go:build linux

package proc

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// taskDir lists the calling process's threads; a test points it elsewhere.
var taskDir = "/proc/self/task"

// Renice lowers the scheduling priority of the calling process to niceness
// n (0-19), so every process it starts from then on inherits it. Zero
// leaves the priority alone.
//
// On Linux niceness belongs to each thread, not to the process, and a new
// thread or child inherits the niceness of the thread that created it. The
// Go runtime runs on several threads, so every thread in /proc/self/task
// is set, and the listing is walked again until a pass finds nothing left
// to change: a thread created mid-walk by one not yet reniced is picked up
// by the next pass. A thread already at n or nicer is left alone, so
// nothing is ever made less nice.
func Renice(n int) error {
	if n <= 0 {
		return nil
	}
	for range 5 {
		entries, err := os.ReadDir(taskDir)
		if err != nil {
			return fmt.Errorf("proc: renice: listing threads: %w", err)
		}
		changed := false
		for _, e := range entries {
			tid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			cur, err := getNice(tid)
			if err != nil || cur >= n {
				continue // the thread exited, or is already nice enough
			}
			if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, n); err != nil {
				return fmt.Errorf("proc: renice thread %d to %d: %w", tid, n, err)
			}
			changed = true
		}
		if !changed {
			return nil
		}
	}
	return nil
}

// getNice returns a thread's niceness. The raw getpriority system call
// returns 20-nice so that it is never negative.
func getNice(tid int) (int, error) {
	p, err := syscall.Getpriority(syscall.PRIO_PROCESS, tid)
	if err != nil {
		return 0, err
	}
	return 20 - p, nil
}
