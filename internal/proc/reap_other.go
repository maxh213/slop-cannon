//go:build !linux

package proc

import "time"

// reapTagged is a no-op where /proc is unavailable: only the process group
// is swept there, so a descendant that left it with setsid survives.
func reapTagged(string, time.Duration) {}
