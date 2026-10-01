//go:build linux

package proc

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
	"time"
)

// reapTagged stops every process whose environment carries tag: SIGTERM
// first, then SIGKILL for whatever is still there once the grace period
// (capped at reapGraceCap) has passed. A zero grace period kills at once.
func reapTagged(tag string, grace time.Duration) {
	marker := []byte(tag + "=1")
	pids := taggedPIDs(marker)
	if len(pids) == 0 {
		return
	}
	grace = reapGrace(grace)
	if grace <= 0 {
		signalAll(pids, syscall.SIGKILL)
		return
	}
	signalAll(pids, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(reapPoll)
		if pids = taggedPIDs(marker); len(pids) == 0 {
			return
		}
	}
	signalAll(taggedPIDs(marker), syscall.SIGKILL)
}

// taggedPIDs lists the live processes whose environment holds marker, a
// complete NAME=VALUE entry. Zombies have an empty environ and never match,
// and processes this user cannot read are skipped.
func taggedPIDs(marker []byte) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		environ, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(environ, []byte{0}) {
			if bytes.Equal(kv, marker) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func signalAll(pids []int, sig syscall.Signal) {
	for _, pid := range pids {
		_ = syscall.Kill(pid, sig)
	}
}
