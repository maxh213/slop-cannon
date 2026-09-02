//go:build !unix

package proc

import (
	"os"
	"os/exec"
	"time"
)

// setProcessGroup is a no-op where process groups are unavailable; only the
// direct child can be killed.
func setProcessGroup(*exec.Cmd) {}

// stopGroup kills the direct child. The grace period is ignored because no
// portable polite signal exists on this platform.
func stopGroup(p *os.Process, _ time.Duration) error {
	if p == nil {
		return os.ErrProcessDone
	}
	return p.Kill()
}

// killGroup kills the direct child, ignoring errors.
func killGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
