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
