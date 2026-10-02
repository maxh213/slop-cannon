//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup makes the child the leader of a new process group so the
// whole tree it spawns can be signalled at once.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopGroup asks the child's process group to stop. With a zero grace period
// it sends SIGKILL immediately; otherwise it sends SIGTERM and arranges a
// SIGKILL after grace if the group is still alive. It reports
// os.ErrProcessDone when the leader has already gone.
func stopGroup(p *os.Process, grace time.Duration) error {
	if grace <= 0 {
		return signalGroup(p, syscall.SIGKILL)
	}
	if err := signalGroup(p, syscall.SIGTERM); err != nil {
		return err
	}
	time.AfterFunc(grace, func() { killGroup(p) })
	return nil
}

// killGroup sends SIGKILL to the child's process group, ignoring errors. It
// is safe to call after the leader has been reaped: an empty group yields
// ESRCH, which is ignored.
func killGroup(p *os.Process) {
	_ = signalGroup(p, syscall.SIGKILL)
}

// reapGroup clears out whatever is left in the child's process group after
// the leader has exited: SIGTERM, then SIGKILL for anything still there once
// grace (capped at reapGraceCap) has passed. The wait matters: a git the
// agent started may be finishing a commit, and SIGTERM lets it remove its
// index.lock before it goes, where an immediate SIGKILL would leave the lock
// behind and wedge the worktree. A zero grace period kills at once.
func reapGroup(p *os.Process, grace time.Duration) {
	grace = reapGrace(grace)
	if grace <= 0 {
		killGroup(p)
		return
	}
	if signalGroup(p, syscall.SIGTERM) != nil {
		return // empty group, or nothing to signal
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(reapPoll)
		if signalGroup(p, 0) != nil {
			return
		}
	}
	killGroup(p)
}

func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
